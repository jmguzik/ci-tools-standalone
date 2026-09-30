package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

func TestAgenticFreshCollisionCheckBeforeActions(t *testing.T) {
	for _, action := range []string{"ownership", "dispatch", "missing-job", "first-success", "lookup-error"} {
		t.Run(action, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			switch action {
			case "dispatch", "lookup-error":
				f = newAgenticFixture(t, "auto")
				f.reconcile(t, nil)
				f.plan(t, "job-a")
				f.passFirstStage(t)
			case "missing-job", "first-success":
				f.reconcile(t, nil)
				if action == "missing-job" {
					f.deleteJobs(t, f.allJobs(t)...)
				} else {
					f.report(t, v1.PendingState)
				}
			}
			created := f.jobs.creates
			f.gh.getPullRequestsCalls = 0
			if action == "lookup-error" {
				f.gh.getPullRequestsError = io.ErrUnexpectedEOF
			} else {
				other := f.gh.pr
				other.Number = 43
				f.gh.otherPRs = append(f.gh.otherPRs, other)
			}
			err := f.tryReconcile(nil)
			require.Error(t, err)
			if action != "lookup-error" {
				require.ErrorContains(t, err, "share a commit-scoped dispatch gate")
			}
			require.Equal(t, 1, f.gh.getPullRequestsCalls)
			require.Equal(t, created, f.jobs.creates, "fresh collision check authorized a new execution")
			for _, gate := range f.gh.checks {
				require.NotEqual(t, "success", gate.Conclusion)
			}
		})
	}
}

func TestAgenticStableEventDoesNotListRepositoryPulls(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "inflight", true: "complete"}[reported], func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			if reported {
				f.report(t, v1.PendingState)
				f.reconcile(t, nil)
			}
			f.gh.getPullRequestsCalls = 0
			f.reconcile(t, nil)
			require.Zero(t, f.gh.getPullRequestsCalls)
		})
	}
}
