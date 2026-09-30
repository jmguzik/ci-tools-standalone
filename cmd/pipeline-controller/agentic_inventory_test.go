package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/prow/pkg/github"
)

// Unlike the single-PR fixture, repository recovery needs to return each PR's
// own current metadata when it walks the listing.
type inventoryAgenticGitHub struct{ *fakeAgenticGitHub }

func (g inventoryAgenticGitHub) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	pr, err := g.fakeAgenticGitHub.GetPullRequest(org, repo, number)
	if err != nil || pr.Number == number {
		return pr, err
	}
	for _, other := range g.otherPRs {
		if other.Number == number {
			return &other, nil
		}
	}
	return nil, io.ErrUnexpectedEOF
}

func TestAgenticRepositoryPassReusesInventory(t *testing.T) {
	for _, shaOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "status"}[shaOnly], func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			other := f.gh.pr
			other.Number, other.Head.SHA = 43, strings.Repeat("c", 40)
			f.reconcile(t, nil)
			second := newAgenticFixture(t, "auto")
			second.gh.pr = other
			second.reconcile(t, nil)
			second.gh.checks[0].ID++
			f.gh.checks = append(f.gh.checks, second.gh.checks...)
			f.gh.getPullRequestsCalls, f.gh.getPullRequestCalls = 0, 0
			f.gh.otherPRs = []github.PullRequest{other}
			f.a.gh = inventoryAgenticGitHub{f.gh}
			sha := ""
			if shaOnly {
				sha = f.gh.pr.Head.SHA
			}
			f.a.reconcileRepo(context.Background(), "org", "repo", sha)
			require.Equal(t, 1, f.gh.getPullRequestsCalls)
			if shaOnly {
				require.Equal(t, 1, f.gh.getPullRequestCalls)
			} else {
				require.Equal(t, 2, f.gh.getPullRequestCalls)
			}
		})
	}
}

func TestAgenticInventoryKeepsCollisionGuard(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	before, _ := f.gate(t)
	other := f.gh.pr
	other.Number = 43
	f.gh.otherPRs = []github.PullRequest{other}
	f.gh.getPullRequestsCalls = 0
	prs, err := f.gh.GetPullRequests("org", "repo")
	require.NoError(t, err)
	err = f.a.reconcileWorkWithInventory(context.Background(), agenticWork{org: "org", repo: "repo", number: 42}, nil, 0, &agenticPullInventory{prs: prs})
	require.ErrorContains(t, err, "share a commit-scoped dispatch gate")
	require.Equal(t, 1, f.gh.getPullRequestsCalls)
	require.Equal(t, 1, f.jobs.creates)
	require.Equal(t, before.Output.Text, f.gh.checks[0].Output.Text)
}

func TestAgenticStableDispatchPassReusesInventory(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "inflight", true: "complete"}[reported], func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			if reported {
				f.report(t, "pending")
				f.reconcile(t, nil)
			}
			f.gh.getPullRequestsCalls = 0
			f.a.reconcileRepo(context.Background(), "org", "repo", "")
			require.Equal(t, 1, f.gh.getPullRequestsCalls)
		})
	}
}

func TestAgenticInventoryRefreshesChangedRevision(t *testing.T) {
	for _, change := range []string{"head", "base", "refresh-error"} {
		t.Run(change, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			before, _ := f.gate(t)
			stale := f.gh.pr
			if change == "base" {
				stale.Base.Ref = "release"
			} else {
				stale.Head.SHA = strings.Repeat("c", 40)
			}
			other := f.gh.pr
			other.Number = 43
			f.gh.otherPRs = []github.PullRequest{other}
			if change == "refresh-error" {
				f.gh.getPullRequestsError = io.ErrUnexpectedEOF
			}
			f.gh.getPullRequestsCalls = 0
			err := f.a.reconcileWorkWithInventory(context.Background(), agenticWork{org: "org", repo: "repo", number: 42}, nil, 0, &agenticPullInventory{prs: []github.PullRequest{stale}})
			require.Error(t, err)
			require.Equal(t, 1, f.gh.getPullRequestsCalls)
			require.Equal(t, 1, f.jobs.creates)
			require.Equal(t, before.Output.Text, f.gh.checks[0].Output.Text)
		})
	}
}

func TestAgenticInventoryRefreshesBeforeNewActions(t *testing.T) {
	for _, action := range []string{"ownership", "dispatch", "missing-job", "first-success", "dispatch-refresh-error"} {
		t.Run(action, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			switch action {
			case "dispatch", "dispatch-refresh-error":
				f = newAgenticFixture(t, "auto")
				f.reconcile(t, nil)
				f.plan(t, "job-a")
				f.passFirstStage(t)
			case "missing-job", "first-success":
				f.reconcile(t, nil)
				if action == "missing-job" {
					f.deleteJobs(t, f.allJobs(t)...)
				} else {
					f.report(t, "pending")
				}
			}
			created := f.jobs.creates
			f.gh.getPullRequestsCalls = 0
			// The repository listing was collision-free, but a sibling opens
			// before the action is authorized using fresh PR metadata.
			f.gh.beforePullRequest = func(int) {
				other := f.gh.pr
				other.Number = 43
				f.gh.otherPRs = []github.PullRequest{other}
				if action == "dispatch-refresh-error" {
					f.gh.getPullRequestsError = io.ErrUnexpectedEOF
				}
			}
			f.a.reconcileRepo(context.Background(), "org", "repo", "")
			require.Equal(t, 2, f.gh.getPullRequestsCalls)
			require.Equal(t, created, f.jobs.creates)
			gate, _ := f.gate(t)
			require.Equal(t, "failure", gate.Conclusion)
		})
	}
}

func TestAgenticInventoryIsNotReusedByRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.gh.getPullRequestError = io.ErrUnexpectedEOF
		stop := startAgenticRunner(f)
		defer stop()
		require.Equal(t, 1, f.gh.getPullRequestsCalls)
		f.gh.getPullRequestError = nil
		other := f.gh.pr
		other.Number = 43
		f.gh.otherPRs = []github.PullRequest{other}
		advanceAgenticTime(f, agenticInitialBackoff)
		require.Equal(t, 2, f.gh.getPullRequestsCalls)
		require.Zero(t, f.jobs.creates)
		gate, state := f.gate(t)
		require.Equal(t, "failure", gate.Conclusion)
		require.Nil(t, state.Dispatch)
		require.Empty(t, f.a.scheduler.pending)
	})
}
