package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/prow/pkg/github"
)

// Run the real queue/deadlines on virtual time, without network requests.
func newScheduledAgenticFixture(t *testing.T, mode string) *agenticFixture {
	f := newAgenticFixture(t, mode)
	f.now, f.a.now = time.Now(), time.Now
	f.gh.pr.CreatedAt = f.now.Add(-time.Hour)
	return f
}

func startAgenticRunner(f *agenticFixture) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.a.Run(ctx) }()
	synctest.Wait()
	select {
	case err := <-done:
		require.NoError(f.t, err)
		f.t.Fatal("runner stopped before cancellation")
	default:
	}
	return func() { cancel(); require.NoError(f.t, <-done) }
}

func advanceAgenticTime(f *agenticFixture, duration time.Duration) {
	f.a.mu.Lock()
	f.a.mu.Unlock() //nolint:staticcheck // SA2001: publish fixture edits to future worker callbacks.
	time.Sleep(duration)
	synctest.Wait()
	f.now = time.Now()
}

func assertAgenticIdle(t *testing.T, f *agenticFixture, duration time.Duration) {
	t.Helper()
	reads, lists, writes := f.gh.getPullRequestCalls, f.gh.getPullRequestsCalls, len(f.gh.checkWrites)
	advanceAgenticTime(f, duration)
	require.Equal(t, reads, f.gh.getPullRequestCalls, "idle worker polled GitHub")
	require.Equal(t, lists, f.gh.getPullRequestsCalls)
	require.Len(t, f.gh.checkWrites, writes)
}

func restartAgenticFixture(f *agenticFixture) {
	previous := f.a
	f.a = &agenticController{gh: previous.gh, reader: previous.reader, config: previous.config,
		watcher: previous.watcher, lgtmWatcher: previous.lgtmWatcher, appID: previous.appID,
		logger: previous.logger, options: previous.options, now: previous.now}
}

func TestAgenticQueueRestoresOnlySelectionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.a.options.timeout = 5 * time.Minute
		f.passFirstStage(t)
		stop := startAgenticRunner(f)
		f.a.enqueue("org", "repo", 42, nil)
		synctest.Wait()
		_, state := f.gate(t)
		started := *state.WaitingSince
		assertAgenticIdle(t, f, 3*time.Minute)
		stop()
		restartAgenticFixture(f)
		reads := f.gh.getPullRequestCalls
		stop = startAgenticRunner(f)
		defer stop()
		require.Equal(t, reads, f.gh.getPullRequestCalls, "restart reconciled before the deadline")
		advanceAgenticTime(f, 2*time.Minute-time.Nanosecond)
		require.Zero(t, f.jobs.creates)
		advanceAgenticTime(f, time.Nanosecond)
		_, state = f.gate(t)
		require.Equal(t, "timeout", state.Plan.Source)
		require.Equal(t, &started, state.ActivatedAt)
		require.Nil(t, state.WaitingSince)
		require.Equal(t, 2, f.jobs.creates)
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticQueuePreservesCommandsAndServerCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.passFirstStage(t)
		f.plan(t, "job-a")
		f.reconcile(t, nil)
		stop := startAgenticRunner(f)
		defer stop()
		command := f.command(500, "remaining")
		f.gh.getPullRequestError = errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)")
		reads := f.gh.getPullRequestCalls
		event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated,
			Repo: f.gh.pr.Base.Repo, Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *command}
		require.True(t, f.a.handleIssueComment(f.a.logger, event))
		synctest.Wait()
		require.Equal(t, reads+1, f.gh.getPullRequestCalls, "worker repeated a routing read during server cooldown")
		reads = f.gh.getPullRequestCalls
		f.gh.getPullRequestError = nil
		event.Comment = *f.command(600, "remaining")
		require.True(t, f.a.handleIssueComment(f.a.logger, event))
		event.Comment.Body = "Unrelated discussion."
		require.True(t, f.a.handleIssueComment(f.a.logger, event))
		f.a.enqueue("org", "repo", 42, nil)
		synctest.Wait()
		advanceAgenticTime(f, 11*time.Minute-time.Nanosecond)
		require.Equal(t, reads, f.gh.getPullRequestCalls, "new events shortened GitHub's cooldown")
		advanceAgenticTime(f, time.Nanosecond)
		_, state := f.gate(t)
		require.Equal(t, 600, state.ManualRequestID)
		require.Equal(t, 1, f.jobs.creates, "coalescing lost/repeated a command")
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticQueueKeepsEventsArrivingDuringDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		f.plan(t, "job-a")
		f.gh.beforeComment = func(string) {
			f.gh.getPullRequestError = errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)")
			f.a.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: github.IssueCommentActionCreated,
				Repo: f.gh.pr.Base.Repo, Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *f.command(500, "remaining")})
			f.gh.getPullRequestError = nil
			f.a.enqueue("org", "repo", 42, nil)
		}
		f.a.enqueue("org", "repo", 42, nil) // Accept events before Run as well.
		stop := startAgenticRunner(f)
		defer stop()
		require.Equal(t, 1, f.jobs.creates)
		require.Len(t, f.gh.comments, 3)
		assertAgenticIdle(t, f, 11*time.Minute-time.Nanosecond)
		advanceAgenticTime(f, time.Nanosecond)
		_, state := f.gate(t)
		require.Equal(t, 500, state.ManualRequestID)
		require.Equal(t, 1, f.jobs.creates)
		require.Empty(t, f.a.inputs, "event arriving during I/O was lost or retained forever")
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticQueueRecoversAmbiguousDispatchWithoutReposting(t *testing.T) {
	for _, checkpointed := range []bool{true, false} {
		t.Run(fmt.Sprintf("context-checkpoint-%t", checkpointed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				f.plan(t, "job-a")
				f.gh.failCommentAfterWrite = true
				require.Error(t, f.tryReconcile(nil))
				if !checkpointed {
					// Earlier schema-2 records lack the context-publication checkpoint.
					_, state := f.gate(t)
					state.Dispatch.ContextsPublished = false
					require.NoError(t, f.a.writeRecord(t.Context(), state.record))
				}
				comments := len(f.gh.comments)
				require.NoError(t, f.a.closeStore())
				restartAgenticFixture(f)
				stop := startAgenticRunner(f)
				defer stop()
				_, state := f.gate(t)
				require.True(t, state.Dispatch.Posted)
				require.Equal(t, "success", f.gh.checks[0].Conclusion)
				require.Equal(t, 1, f.gh.statusWrites, "retry reset an already-reported job context")
				require.Len(t, f.gh.comments, comments)
				require.Equal(t, 1, f.jobs.creates)
				assertAgenticIdle(t, f, time.Hour)
			})
		})
	}
}

func TestAgenticQueueCompletesPreviouslyPostedDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		f.plan(t, "job-a")
		f.reconcile(t, nil)
		_, state := f.gate(t)
		// Existing schema-2 records may still have the old results-waiting gate.
		state.record.Gate.Status, state.record.Gate.Conclusion = "in_progress", ""
		f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "in_progress", ""
		require.NoError(t, f.a.writeRecord(t.Context(), state.record))
		comments, writes := len(f.gh.comments), f.gh.statusWrites
		require.NoError(t, f.a.closeStore())
		restartAgenticFixture(f)
		stop := startAgenticRunner(f)
		defer stop()
		require.Equal(t, "success", f.gh.checks[0].Conclusion)
		require.Len(t, f.gh.comments, comments)
		require.Equal(t, writes, f.gh.statusWrites)
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticQueueRestoresInterruptedDecisionSaves(t *testing.T) {
	for _, source := range []string{"opt-out", "timeout"} {
		t.Run(source, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			f.passFirstStage(t)
			if source == "timeout" {
				f.reconcile(t, nil)
				f.now = f.now.Add(f.a.options.timeout)
			} else {
				f.gh.pr.Labels = append(f.gh.pr.Labels, github.Label{Name: agenticSkipLabel})
			}
			f.gh.afterCheckWrite = func(check github.CheckRun) {
				if source == "opt-out" || strings.Contains(check.Output.Summary, "Chai timed out; preparing normal selection.") {
					// Rename saves the acknowledged decision, then fsync fails.
					// Restart must restore dispatch even though Dirty is cleared.
					require.NoError(t, f.a.store.directory.Close())
				}
			}
			require.Error(t, f.tryReconcile(nil))
			f.gh.afterCheckWrite = nil
			_ = f.a.closeStore() // The injected directory handle is already closed.
			restartAgenticFixture(f)
			f.a.startQueue(t.Context())
			t.Cleanup(f.a.queue.ShutDown)
			require.NoError(t, f.a.restoreRecords(t.Context()))
			_, state := f.gate(t)
			require.Equal(t, source, state.Plan.Source)
			require.Nil(t, state.WaitingSince)
			require.Nil(t, state.Dispatch)
			require.False(t, state.record.Dirty)
			require.True(t, state.PendingDispatch)
			require.Equal(t, 1, f.a.queue.Len(), "ready decision was stranded across restart")
			f.a.processNext(t.Context())
			_, state = f.gate(t)
			require.True(t, state.Dispatch.Posted)
			require.False(t, state.PendingDispatch)
			require.Equal(t, 2, f.jobs.creates)
		})
	}
}

func TestAgenticQueueDropsLateEventsAfterShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		stop := startAgenticRunner(f)
		stop()
		owner, err := openAgenticStore(f.a.options.stateDir)
		require.NoError(t, err)
		defer func() { require.NoError(t, owner.close()) }()
		before := *f.gh
		f.a.handlePullRequest(f.a.logger, github.PullRequestEvent{Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr})
		f.a.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: github.IssueCommentActionCreated,
			Repo: f.gh.pr.Base.Repo, Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *f.command(500, "remaining")})
		// Ignore the local test command appended above; neither handler may call GitHub.
		before.comments = f.gh.comments
		require.Equal(t, before, *f.gh)
		require.Nil(t, f.a.store)
	})
}
