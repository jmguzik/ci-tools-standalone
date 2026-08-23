package opsproxy

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestIdentityFromLabels(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		labels  map[string]string
		want    Identity
		wantErr error
	}{
		{
			name: "job_name infra job",
			labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job_name":  "periodic-openshift-release-merge-blockers",
				"job":       "prometheus-k8s",
			},
			want: Identity{
				ID:           "infrastructure-job-failures/periodic-openshift-release-merge-blockers",
				AlertName:    "infrastructure-job-failures",
				MatcherName:  "job_name",
				MatcherValue: "periodic-openshift-release-merge-blockers",
			},
		},
		{
			name: "reason coalesce high-ci-operator-error-rate",
			labels: map[string]string{
				"alertname": "high-ci-operator-error-rate",
				"reason":    "creating_release_images",
			},
			want: Identity{
				ID:           "ci-operator-error/creating_release_images",
				AlertName:    "high-ci-operator-error-rate",
				MatcherName:  "reason",
				MatcherValue: "creating_release_images",
			},
		},
		{
			name: "reason coalesce high-ci-operator-infra-error-rate",
			labels: map[string]string{
				"alertname": "high-ci-operator-infra-error-rate",
				"reason":    "creating_release_images",
			},
			want: Identity{
				ID:           "ci-operator-error/creating_release_images",
				AlertName:    "high-ci-operator-infra-error-rate",
				MatcherName:  "reason",
				MatcherValue: "creating_release_images",
			},
		},
		{
			name: "reason without coalesce",
			labels: map[string]string{
				"alertname": "some-other-error-rate",
				"reason":    "creating_release_images",
			},
			want: Identity{
				ID:           "some-other-error-rate/creating_release_images",
				AlertName:    "some-other-error-rate",
				MatcherName:  "reason",
				MatcherValue: "creating_release_images",
			},
		},
		{
			name: "priv-image class ignores job_tail",
			labels: map[string]string{
				"alertname": "openshift-priv-image-building-jobs-failing",
				"job_tail":  "4.19-ocp",
				"job":       "prometheus-k8s",
			},
			want: Identity{
				ID:        "openshift-priv-image-building-jobs-failing",
				AlertName: "openshift-priv-image-building-jobs-failing",
			},
		},
		{
			name: "job_tail identity",
			labels: map[string]string{
				"alertname": "some-image-jobs-failing",
				"job_tail":  "4.19-ocp",
			},
			want: Identity{
				ID:           "some-image-jobs-failing/4.19-ocp",
				AlertName:    "some-image-jobs-failing",
				MatcherName:  "job_tail",
				MatcherValue: "4.19-ocp",
			},
		},
		{
			name: "refuse scrape job only",
			labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job":       "prometheus-k8s",
			},
			wantErr: ErrNoIdentity,
		},
		{
			name:    "refuse empty",
			labels:  nil,
			wantErr: ErrNoIdentity,
		},
		{
			name: "refuse alertname only",
			labels: map[string]string{
				"alertname": "DiskRunningFull",
				"severity":  "critical",
			},
			wantErr: ErrNoIdentity,
		},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := IdentityFromLabels(tc.labels)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("identity mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestIdentitySilenceMatchers(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		id   Identity
		want []Matcher
	}{
		{
			name: "job_name",
			id: Identity{
				AlertName:    "infrastructure-job-failures",
				MatcherName:  "job_name",
				MatcherValue: "periodic-foo",
			},
			want: []Matcher{equalMatcher("alertname", "infrastructure-job-failures"), equalMatcher("job_name", "periodic-foo")},
		},
		{
			name: "coalesce still uses payload alertname for a single set",
			id: Identity{
				ID:           "ci-operator-error/creating_release_images",
				AlertName:    "high-ci-operator-error-rate",
				MatcherName:  "reason",
				MatcherValue: "creating_release_images",
			},
			want: []Matcher{equalMatcher("alertname", "high-ci-operator-error-rate"), equalMatcher("reason", "creating_release_images")},
		},
		{
			name: "priv-image class is alertname only",
			id: Identity{
				ID:        "openshift-priv-image-building-jobs-failing",
				AlertName: "openshift-priv-image-building-jobs-failing",
			},
			want: []Matcher{equalMatcher("alertname", "openshift-priv-image-building-jobs-failing")},
		},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.id.SilenceMatchers()
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("matchers mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCoalesceSameCard(t *testing.T) {
	t.Parallel()
	a, err := IdentityFromLabels(map[string]string{
		"alertname": "high-ci-operator-error-rate",
		"reason":    "foo",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := IdentityFromLabels(map[string]string{
		"alertname": "high-ci-operator-infra-error-rate",
		"reason":    "foo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("coalesced ids differ: %s vs %s", a.ID, b.ID)
	}
}

func TestSilenceMatcherSetsCoalesceBothAlertnames(t *testing.T) {
	t.Parallel()
	id := Identity{
		ID:           "ci-operator-error/creating_release_images",
		AlertName:    "high-ci-operator-infra-error-rate",
		MatcherName:  "reason",
		MatcherValue: "creating_release_images",
	}
	got := id.SilenceMatcherSets()
	want := [][]Matcher{
		{equalMatcher("alertname", "high-ci-operator-error-rate"), equalMatcher("reason", "creating_release_images")},
		{equalMatcher("alertname", "high-ci-operator-infra-error-rate"), equalMatcher("reason", "creating_release_images")},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}
