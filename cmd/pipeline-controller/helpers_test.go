package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"

	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeGhClient implements minimalGhClient for testing.
type fakeGhClient struct {
	comments []string
	changes  []github.PullRequestChange
}

func (f *fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	return &github.PullRequest{}, nil
}

func (f *fakeGhClient) CreateComment(org, repo string, number int, comment string) error {
	f.comments = append(f.comments, comment)
	return nil
}

func (f *fakeGhClient) GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error) {
	return f.changes, nil
}

func (f *fakeGhClient) CreateStatus(org, repo, ref string, s github.Status) error {
	return nil
}

func (f *fakeGhClient) AddLabel(org, repo string, number int, label string) error {
	return nil
}

func (f *fakeGhClient) GetIssueLabels(org, repo string, number int) ([]github.Label, error) {
	return nil, nil
}

func newFakePJLister(pjs ...v1.ProwJob) ctrlruntimeclient.Reader {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	objs := make([]ctrlruntimeclient.Object, 0, len(pjs))
	for i := range pjs {
		objs = append(objs, &pjs[i])
	}
	return fakectrlruntimeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func makeProwJob(name string, sha string) v1.ProwJob {
	const (
		org      = "openshift"
		repo     = "myrepo"
		baseRef  = "main"
		prNumber = 42
	)
	return v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pj-%s-%s", name, sha[:7]),
			Namespace: "default",
			Labels: map[string]string{
				"prow.k8s.io/type":          "presubmit",
				"prow.k8s.io/refs.org":      org,
				"prow.k8s.io/refs.repo":     repo,
				"prow.k8s.io/refs.pull":     fmt.Sprintf("%d", prNumber),
				"prow.k8s.io/refs.base_ref": baseRef,
			},
		},
		Spec: v1.ProwJobSpec{
			Job:  name,
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				Org:     org,
				Repo:    repo,
				BaseRef: baseRef,
				Pulls: []v1.Pull{
					{Number: prNumber, SHA: sha},
				},
			},
		},
		Status: v1.ProwJobStatus{
			State: v1.SuccessState,
		},
	}
}

func makeTriggerPJ(org, repo, baseRef string, prNumber int, sha string) *v1.ProwJob {
	return &v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "trigger-pj",
			Namespace: "default",
		},
		Spec: v1.ProwJobSpec{
			Job:  "trigger-job",
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				Org:     org,
				Repo:    repo,
				BaseRef: baseRef,
				Pulls: []v1.Pull{
					{Number: prNumber, SHA: sha},
				},
			},
		},
	}
}

func TestSendCommentWithMode_ProtectedDedup(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	protectedJobName := fmt.Sprintf("pull-ci-%s-%s-%s-e2e-test", org, repo, baseRef)
	repoBaseRef := repo + "-" + baseRef

	protectedPresubmit := config.Presubmit{
		JobBase: config.JobBase{
			Name: protectedJobName,
		},
		Reporter: config.Reporter{
			Context: protectedJobName,
		},
		RerunCommand: "/test " + protectedJobName,
	}

	// Verify our test job name contains repoBaseRef (required for the filter)
	if !strings.Contains(protectedJobName, repoBaseRef) {
		t.Fatalf("test setup error: job name %q does not contain %q", protectedJobName, repoBaseRef)
	}

	tests := []struct {
		name              string
		isExplicitCommand bool
		existingPJs       []v1.ProwJob
		wantComment       string // substring to check for
		wantNoRerun       bool   // true if we expect the RerunCommand NOT to appear
	}{
		{
			name:              "protected tests triggered normally when no existing ProwJob",
			isExplicitCommand: false,
			existingPJs:       nil,
			wantComment:       "Scheduling required tests:",
			wantNoRerun:       false,
		},
		{
			name:              "protected tests NOT re-triggered when ProwJob exists at same SHA (dedup)",
			isExplicitCommand: false,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, sha),
			},
			wantComment: "No second-stage tests were triggered",
			wantNoRerun: true,
		},
		{
			name:              "protected tests ARE re-triggered with explicit command even with existing ProwJob",
			isExplicitCommand: true,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, sha),
			},
			wantComment: "Scheduling required tests:",
			wantNoRerun: false,
		},
		{
			name:              "protected tests triggered when ProwJob exists at different SHA",
			isExplicitCommand: false,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, "different_sha_123"),
			},
			wantComment: "Scheduling required tests:",
			wantNoRerun: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGhClient{}
			pjLister := newFakePJLister(tc.existingPJs...)

			presubmits := presubmitTests{
				protected: []config.Presubmit{protectedPresubmit},
			}

			pj := makeTriggerPJ(org, repo, baseRef, prNum, sha)

			deleteIdsCalled := false
			deleteIds := func() { deleteIdsCalled = true }

			err := sendCommentWithMode(presubmits, pj, ghc, deleteIds, pjLister, tc.isExplicitCommand)
			if err != nil {
				t.Fatalf("sendCommentWithMode returned error: %v", err)
			}

			if len(ghc.comments) != 1 {
				t.Fatalf("expected 1 comment, got %d", len(ghc.comments))
			}
			comment := ghc.comments[0]

			if !strings.Contains(comment, tc.wantComment) {
				t.Errorf("comment %q does not contain expected substring %q", comment, tc.wantComment)
			}

			rerunPresent := strings.Contains(comment, protectedPresubmit.RerunCommand)
			if tc.wantNoRerun && rerunPresent {
				t.Errorf("expected RerunCommand NOT to appear in comment but it did: %q", comment)
			}
			if !tc.wantNoRerun && !rerunPresent {
				t.Errorf("expected RerunCommand to appear in comment but it did not: %q", comment)
			}

			// deleteIds should not be called on success
			if deleteIdsCalled {
				t.Errorf("deleteIds was called unexpectedly")
			}
		})
	}
}

func TestSendCommentWithMode_ConditionalDedupStillWorks(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	conditionalJobName := fmt.Sprintf("pull-ci-%s-%s-%s-conditional-test", org, repo, baseRef)

	conditionalPresubmit := config.Presubmit{
		JobBase: config.JobBase{
			Name: conditionalJobName,
			Annotations: map[string]string{
				"pipeline_run_if_changed": "^cmd/",
			},
		},
		Reporter: config.Reporter{
			Context: conditionalJobName,
		},
		RerunCommand: "/test " + conditionalJobName,
	}

	tests := []struct {
		name              string
		isExplicitCommand bool
		existingPJs       []v1.ProwJob
		changes           []github.PullRequestChange
		wantManualControl bool // true if we expect the manual control message
	}{
		{
			name:              "conditional tests dedup returns manual control message when ProwJob exists",
			isExplicitCommand: false,
			existingPJs: []v1.ProwJob{
				makeProwJob(conditionalJobName, sha),
			},
			changes: []github.PullRequestChange{
				{Filename: "cmd/main.go"},
			},
			wantManualControl: true,
		},
		{
			name:              "conditional tests are triggered when no ProwJob exists",
			isExplicitCommand: false,
			existingPJs:       nil,
			changes: []github.PullRequestChange{
				{Filename: "cmd/main.go"},
			},
			wantManualControl: false,
		},
		{
			name:              "conditional tests triggered with explicit command even when ProwJob exists",
			isExplicitCommand: true,
			existingPJs: []v1.ProwJob{
				makeProwJob(conditionalJobName, sha),
			},
			changes: []github.PullRequestChange{
				{Filename: "cmd/main.go"},
			},
			wantManualControl: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGhClient{changes: tc.changes}
			pjLister := newFakePJLister(tc.existingPJs...)

			presubmits := presubmitTests{
				pipelineConditionallyRequired: []config.Presubmit{conditionalPresubmit},
			}

			pj := makeTriggerPJ(org, repo, baseRef, prNum, sha)

			err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, tc.isExplicitCommand)
			if err != nil {
				t.Fatalf("sendCommentWithMode returned error: %v", err)
			}

			if len(ghc.comments) != 1 {
				t.Fatalf("expected 1 comment, got %d", len(ghc.comments))
			}
			comment := ghc.comments[0]

			manualControlMsg := "Tests from second stage were triggered manually"
			if tc.wantManualControl && !strings.Contains(comment, manualControlMsg) {
				t.Errorf("expected manual control message in comment but got: %q", comment)
			}
			if !tc.wantManualControl && strings.Contains(comment, manualControlMsg) {
				t.Errorf("did not expect manual control message but got it in comment: %q", comment)
			}
		})
	}
}

func TestExistsAtSHA(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	jobName := fmt.Sprintf("pull-ci-%s-%s-%s-e2e-test", org, repo, baseRef)

	pj := makeTriggerPJ(org, repo, baseRef, prNum, sha)

	tests := []struct {
		name        string
		existingPJs []v1.ProwJob
		jobName     string
		wantExists  bool
	}{
		{
			name:        "returns true when ProwJob exists at same SHA",
			existingPJs: []v1.ProwJob{makeProwJob(jobName, sha)},
			jobName:     jobName,
			wantExists:  true,
		},
		{
			name:        "returns false when no ProwJob exists",
			existingPJs: nil,
			jobName:     jobName,
			wantExists:  false,
		},
		{
			name:        "returns false when ProwJob exists at different SHA",
			existingPJs: []v1.ProwJob{makeProwJob(jobName, "differentsha1234")},
			jobName:     jobName,
			wantExists:  false,
		},
		{
			name:        "returns false when ProwJob exists for different job name",
			existingPJs: []v1.ProwJob{makeProwJob("some-other-job", sha)},
			jobName:     jobName,
			wantExists:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pjLister := newFakePJLister(tc.existingPJs...)
			got := existsAtSHA(context.Background(), pjLister, pj, tc.jobName)
			if got != tc.wantExists {
				t.Errorf("existsAtSHA() = %v, want %v", got, tc.wantExists)
			}
		})
	}
}
