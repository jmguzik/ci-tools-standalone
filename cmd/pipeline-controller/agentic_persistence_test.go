package main

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticPersistedCooldownAndCommandSurviveRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		stop := startAgenticRunner(f)
		f.reconcile(t, nil)
		command := f.command(500, "remaining")
		f.a.retryReconciliation("org", "repo", 42, command, io.ErrUnexpectedEOF)
		stop()
		f.a.statusContexts = nil
		reads := f.gh.getPullRequestCalls
		stop = startAgenticRunner(f)
		defer stop()
		require.Equal(t, reads, f.gh.getPullRequestCalls)
		advanceAgenticTime(f, agenticInitialBackoff-time.Nanosecond)
		require.Equal(t, reads, f.gh.getPullRequestCalls)
		advanceAgenticTime(f, time.Nanosecond)
		_, state := f.gate(t)
		require.Equal(t, command.ID, state.ManualRequestID)
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticStaleChaiWakeupDoesNotDelayUnfinishedDispatch(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfinished", true: "completed"}[completed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				f.plan(t, "job-a")
				if completed {
					f.reconcile(t, nil)
					f.report(t, v1.PendingState)
					f.reconcile(t, nil)
				} else {
					f.jobs.failAt = 1
					require.Error(t, f.tryReconcile(nil))
				}
				_, state := f.gate(t)
				at := time.Now().Add(time.Hour)
				state.record.Wakeup = &agenticSavedWakeup{At: at, Deadline: at}
				require.NoError(t, f.a.writeRecord(context.Background(), state.record))
				reads := f.gh.getPullRequestCalls
				stop := startAgenticRunner(f)
				defer stop()
				if completed {
					require.Equal(t, reads, f.gh.getPullRequestCalls)
				} else {
					require.Len(t, f.allJobs(t), 1, "startup waited for an obsolete Chai deadline")
				}
				assertAgenticIdle(t, f, 2*time.Hour)
			})
		})
	}
}

func TestAgenticShutdownDropsLateEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		stop := startAgenticRunner(f)
		f.reconcile(t, nil)
		command := f.command(500, "required")
		stop()
		owner, err := openAgenticStore(f.a.options.stateDir)
		require.NoError(t, err, "shutdown did not release the writer lock")
		defer func() { require.NoError(t, owner.close()) }()
		before := *f.gh
		f.a.handlePullRequest(f.a.logger, github.PullRequestEvent{Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr})
		f.a.handleStatus(f.a.logger, github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: "ci/first", State: "success"})
		f.a.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: github.IssueCommentActionCreated,
			Repo: f.gh.pr.Base.Repo, Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *command})
		require.Equal(t, before, *f.gh)
		require.Zero(t, f.jobs.creates)
		require.Nil(t, f.a.store)
	})
}

func TestAgenticUncertainSuccessIsClearedBeforeForcedRerun(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	f.report(t, v1.PendingState)
	gate, state := f.gate(t)
	// A success PATCH reached GitHub, but persisting its acknowledgment did not.
	remote := gate
	remote.Status, remote.Conclusion = "completed", "success"
	f.gh.checks[0] = remote
	state.record.Desired = &remote
	require.NoError(t, f.a.writeRecord(context.Background(), state.record))
	f.gh.failCheck = true
	require.Error(t, f.tryReconcile(f.command(500, "required")))
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.True(t, r.Reopen, "uncertain success must remain durable across a failed reopening")
	f.gh.failCheck = false
	f.jobs.beforeCreate = func(*v1.ProwJob) {
		require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
	}
	f.reconcile(t, nil)
	require.Equal(t, 2, f.jobs.creates)
	require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
}

func TestAgenticFailedProjectionRepublishesAfterRecovery(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	check, _ := f.gate(t)
	require.Equal(t, "success", check.Conclusion)
	f.gh.listCommentsError = io.ErrUnexpectedEOF
	require.Error(t, f.tryReconcile(nil))
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	f.gh.listCommentsError = nil
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
}

func TestAgenticAppliedRerunIntentResumesAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		f.plan(t, "job-a")
		f.reconcile(t, nil)
		f.report(t, v1.PendingState)
		f.reconcile(t, nil)
		gate, state := f.gate(t)
		command := f.command(500, "required")
		require.NoError(t, f.a.recordCommand(&gate, state, *command))
		cfg, _ := f.a.repoConfig("org", "repo", "main")
		require.NoError(t, f.a.applyCommand(&gate, state, cfg, &f.gh.pr, f.gh.comments))
		// Crash after command acknowledgment, before dispatch preparation.
		require.True(t, state.PendingDispatch)
		stop := startAgenticRunner(f)
		defer stop()
		require.Equal(t, 2, f.jobs.creates)
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticReturningOwnerClearsAnotherOwnersSuccess(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.reconcile(t, nil)
	f.gh.pr.Base.Ref = "release"
	f.reconcile(t, nil)
	// Another PR reused this commit-scoped gate and succeeded while this PR
	// was inactive. The old record still remembers only its earlier failure.
	f.gh.checks[0].ExternalID = agenticExternalID("org", "repo", 43)
	f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "completed", "success"
	f.gh.pr.Base.Ref = "main"
	f.reconcile(t, nil)
	require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
	require.Equal(t, agenticExternalID("org", "repo", 42), f.gh.checks[0].ExternalID)
	require.Zero(t, f.jobs.creates)
}

func TestAgenticGateTransferRetiresOwnerAfterMissedClose(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	oldGate, oldState := f.gate(t)
	f.gh.pr.Number, f.gh.comments = 43, nil // #42 closed without a webhook.
	f.plan(t, "job-b")                      // Reusing another PR's gate requires a post-tracking plan.
	f.reconcile(t, nil)
	siblingGate, siblingState := f.gate(t)
	if siblingGate.ID != oldGate.ID || len(f.gh.checks) != 1 || siblingGate.Conclusion == "success" || siblingState.Number != 43 || siblingState.ManualRequestID != 0 || siblingState.Plan != nil {
		t.Fatal("closed sibling left a competing successful gate or stale authorization")
	}
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.True(t, r.State.Inactive)
	require.Zero(t, r.Gate.ID)
	owner := f.gh.pr
	f.gh.pr.Number = 42
	f.gh.otherPRs = []github.PullRequest{owner}
	require.Error(t, f.tryReconcile(nil), "the new live owner must block reentry")
	f.gh.otherPRs = nil // #43 has now closed.
	f.reconcile(t, nil)
	gate, state := f.gate(t)
	require.Equal(t, oldGate.ID, gate.ID)
	require.NotEqual(t, oldState.RevisionID, state.RevisionID)
	require.Nil(t, state.Plan)
	require.Zero(t, state.ManualRequestID)
	require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
	require.Equal(t, 1, f.jobs.creates)
}
