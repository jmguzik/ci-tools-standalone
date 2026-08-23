package opsproxy

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidateProposedSilence(t *testing.T) {
	t.Parallel()
	firing := []FiringIncident{
		{
			ID: "infrastructure-job-failures/job-a",
			Labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job_name":  "job-a",
				"severity":  "critical",
			},
		},
		{
			ID: "infrastructure-job-failures/job-b",
			Labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job_name":  "job-b",
				"severity":  "critical",
			},
		},
		{
			ID: "openshift-priv-image-building-jobs-failing",
			Labels: map[string]string{
				"alertname": "openshift-priv-image-building-jobs-failing",
				"job_tail":  "4.19-ocp",
			},
		},
	}
	testCases := []struct {
		name     string
		matchers []Matcher
		firing   []FiringIncident
		wantErr  string
	}{
		{
			name:     "refuse severity-only",
			matchers: []Matcher{equalMatcher("severity", "critical")},
			firing:   firing,
			wantErr:  "severity-only",
		},
		{
			name:     "refuse infrastructure-job-failures without job_name",
			matchers: []Matcher{equalMatcher("alertname", "infrastructure-job-failures")},
			firing:   firing,
			wantErr:  "without job_name",
		},
		{
			name: "refuse multi-incident job_tail-less alertname covering two cards",
			matchers: []Matcher{
				equalMatcher("alertname", "infrastructure-job-failures"),
				equalMatcher("severity", "critical"),
			},
			firing:  firing,
			wantErr: "without job_name",
		},
		{
			name: "narrow job_name is ok even with extra severity matcher",
			matchers: []Matcher{
				equalMatcher("severity", "critical"),
				equalMatcher("alertname", "infrastructure-job-failures"),
				equalMatcher("job_name", "job-a"),
			},
			firing: firing,
		},
		{
			name: "narrow job_name is ok",
			matchers: []Matcher{
				equalMatcher("alertname", "infrastructure-job-failures"),
				equalMatcher("job_name", "job-a"),
			},
			firing: firing,
		},
		{
			name: "class alertname-only is ok for one class incident",
			matchers: []Matcher{
				equalMatcher("alertname", "openshift-priv-image-building-jobs-failing"),
			},
			firing: firing,
		},
		{
			name:    "empty matchers refused",
			firing:  firing,
			wantErr: "empty",
		},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateProposedSilence(tc.matchers, tc.firing)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateProposedSilenceMultiIncident(t *testing.T) {
	t.Parallel()
	firing := []FiringIncident{
		{ID: "a/one", Labels: map[string]string{"alertname": "a", "job_tail": "one", "cluster": "app.ci"}},
		{ID: "a/two", Labels: map[string]string{"alertname": "a", "job_tail": "two", "cluster": "app.ci"}},
	}
	err := ValidateProposedSilence([]Matcher{equalMatcher("alertname", "a"), equalMatcher("cluster", "app.ci")}, firing)
	if err == nil {
		t.Fatal("expected multi-incident refuse")
	}
	if !strings.Contains(err.Error(), "2 currently firing incidents") {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestMatchersCover(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"alertname": "foo", "job_name": "bar"}
	ok := MatchersCover([]Matcher{equalMatcher("alertname", "foo"), equalMatcher("job_name", "bar")}, labels)
	if !ok {
		t.Fatal("expected cover")
	}
	if MatchersCover([]Matcher{equalMatcher("alertname", "foo"), equalMatcher("job_name", "other")}, labels) {
		t.Fatal("did not expect cover")
	}
	if diff := cmp.Diff(false, MatchersCover(nil, labels)); diff != "" {
		t.Fatalf("empty matchers: %s", diff)
	}
}
