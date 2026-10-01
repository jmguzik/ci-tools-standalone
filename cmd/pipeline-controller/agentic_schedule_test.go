package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

// synctest runs the real scheduler/timers on virtual time. The hours-long idle
// checks below complete immediately and make no network requests.
func newScheduledAgenticFixture(t *testing.T, mode string) *agenticFixture {
	f := newAgenticFixture(t, mode)
	f.now = time.Now()
	f.a.now = time.Now
	f.gh.pr.CreatedAt = f.now.Add(-time.Hour)
	return f
}

func startAgenticRunner(f *agenticFixture) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.a.Run(ctx)
	}()
	synctest.Wait()
	select {
	case err := <-done:
		require.NoError(f.t, err)
		f.t.Fatal("agentic runner stopped before cancellation")
	default:
	}
	return func() {
		cancel()
		require.NoError(f.t, <-done)
	}
}

func advanceAgenticTime(f *agenticFixture, duration time.Duration) {
	// Publish fixture edits/observations to future callbacks. synctest.Wait
	// synchronizes callbacks that finished, not ones whose timer is still due.
	f.a.mu.Lock()
	f.a.mu.Unlock() //nolint:staticcheck // SA2001: intentional happens-before barrier for future scheduler callbacks.
	time.Sleep(duration)
	synctest.Wait()
	f.now = time.Now()
}

func assertAgenticIdle(t *testing.T, f *agenticFixture, duration time.Duration) {
	t.Helper()
	reads, lists, writes := f.gh.getPullRequestCalls, f.gh.getPullRequestsCalls, len(f.gh.checkWrites)
	advanceAgenticTime(f, duration)
	if f.gh.getPullRequestCalls != reads || f.gh.getPullRequestsCalls != lists || len(f.gh.checkWrites) != writes || len(f.a.scheduler.pending) != 0 {
		t.Fatal("idle controller polled GitHub or retained a recurring timer")
	}
}

func TestAgenticPersistWakeupOnlyWritesChanges(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	f.a.options.stateTTL = time.Hour
	work := agenticWork{org: "org", repo: "repo", number: 42}
	deadline := f.now.Add(20 * time.Minute)
	for _, tc := range []struct {
		name    string
		wakeup  *agenticSavedWakeup
		changed bool
	}{
		{name: "nil-to-nil"},
		{name: "set", wakeup: &agenticSavedWakeup{At: deadline, Deadline: deadline}, changed: true},
		{name: "unchanged", wakeup: &agenticSavedWakeup{At: deadline, Deadline: deadline}},
		{name: "retry", wakeup: &agenticSavedWakeup{At: deadline, Deadline: deadline, Backoff: time.Minute, Comment: f.command(500, "remaining")}, changed: true},
		{name: "clear", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ageAgenticRecord(t, f, 42, f.now.Add(-time.Minute))
			before, err := os.Stat(path)
			require.NoError(t, err)
			require.False(t, f.a.persistWakeup(context.Background(), work, tc.wakeup))
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, tc.changed, !before.ModTime().Equal(after.ModTime()), "wakeup change did not match retention refresh")
			r, err := f.a.readRecord(context.Background(), work)
			require.NoError(t, err)
			require.Equal(t, tc.wakeup, r.Wakeup)
		})
	}
	t.Run("expired-nil", func(t *testing.T) {
		path := ageAgenticRecord(t, f, 42, f.now.Add(-f.a.options.stateTTL))
		require.True(t, f.a.persistWakeup(context.Background(), work, nil), "unchanged wakeup skipped expiry")
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err))
		require.Empty(t, f.a.store.entries, "expiry retained routing metadata")
	})
}

func TestAgenticDeadlineSurvivesRestartWithoutPolling(t *testing.T) {
	for _, downtime := range []time.Duration{time.Minute, 3 * time.Minute} {
		t.Run(downtime.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.a.options.timeout = 5 * time.Minute
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				f.reconcile(t, nil)
				_, original := f.gate(t)
				require.NotNil(t, original.WaitingSince, "missing Chai deadline")
				reads := f.gh.getPullRequestCalls
				advanceAgenticTime(f, 3*time.Minute)
				require.Equal(t, reads, f.gh.getPullRequestCalls, "polled before the Chai deadline")
				stop()
				advanceAgenticTime(f, downtime)
				stop = startAgenticRunner(f)
				defer stop()
				if remaining := f.a.options.timeout - (3*time.Minute + downtime); remaining > 0 {
					_, recovered := f.gate(t)
					require.Equal(t, original.WaitingSince, recovered.WaitingSince, "restart reset the persisted waiting timestamp")
					advanceAgenticTime(f, remaining-time.Nanosecond)
					require.Zero(t, f.jobs.creates, "fallback ran before the restored global deadline")
					advanceAgenticTime(f, time.Nanosecond)
				}
				_, state := f.gate(t)
				require.NotNil(t, state.Plan)
				require.Equal(t, "timeout", state.Plan.Source)
				require.Equal(t, 2, f.jobs.creates, "persisted deadline did not dispatch once")
				assertAgenticIdle(t, f, 24*time.Hour)
				f.report(t, v1.PendingState)
				f.a.handleStatus(f.a.logger, github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: "ci/job-a", State: "pending"})
				gate, _ := f.gate(t)
				require.Equal(t, "success", gate.Conclusion, "report event failed to finish dispatch")
			})
		})
	}
}

func TestAgenticEventsCancelChaiDeadline(t *testing.T) {
	for _, change := range []string{"plan", "push", "retarget", "draft", "closed"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
				f.reconcile(t, nil)
				switch change {
				case "plan":
					f.plan(t, "job-a")
				case "push":
					f.gh.pr.Head.SHA = strings.Repeat("c", 40)
				case "retarget":
					f.gh.pr.Base.Ref = "release"
				case "draft":
					f.gh.pr.Draft = true
				case "closed":
					f.gh.pr.State = github.PullRequestStateClosed
				}
				f.reconcile(t, nil)
				if len(f.a.scheduler.pending) != 0 {
					t.Fatal("obsolete deadline was not canceled")
				}
				assertAgenticIdle(t, f, time.Hour)
			})
		})
	}
}

func TestAgenticTransientRetriesContinueUntilRecovery(t *testing.T) {
	for _, failing := range []string{"pull", "repository", "expired-deadline"} {
		t.Run(failing, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				failure := io.ErrUnexpectedEOF
				var calls func() int
				switch failing {
				case "repository":
					f.gh.getPullRequestsError = failure
					calls = func() int { return f.gh.getPullRequestsCalls }
				default:
					calls = func() int { return f.gh.getPullRequestCalls }
					if failing == "pull" {
						f.gh.getPullRequestError = failure
					}
				}
				stop := startAgenticRunner(f)
				defer stop()
				_ = f.tryReconcile(nil)
				if failing == "expired-deadline" {
					f.gh.getPullRequestError = failure
					advanceAgenticTime(f, defaultAgenticTimeout)
				}
				expected := calls()
				for _, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
					advanceAgenticTime(f, delay-time.Nanosecond)
					if calls() != expected {
						t.Fatal("retry ran before its backoff elapsed")
					}
					advanceAgenticTime(f, time.Nanosecond)
					expected++
					if calls() != expected || len(f.a.scheduler.pending) != 1 || f.jobs.creates != 0 {
						t.Fatalf("targeted recovery stopped or duplicated: calls=%d, want=%d, pending=%d", calls(), expected, len(f.a.scheduler.pending))
					}
				}
				f.gh.getPullRequestError, f.gh.getPullRequestsError = nil, nil
				f.plan(t, "job-a")
				advanceAgenticTime(f, agenticMaxBackoff)
				if f.jobs.creates != 1 || len(f.a.scheduler.pending) != 0 {
					t.Fatal("transient recovery needed a new event after five attempts")
				}
				assertAgenticIdle(t, f, 24*time.Hour)
			})
		})
	}
}

func TestAgenticInvalidPlanOnlyWaitsForDeadline(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "failed-write"}[failWrite], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
				if failWrite {
					f.reconcile(t, nil) // Preserve an established deadline through the failed write.
				}
				f.plan(t, "unknown-job")
				f.gh.failCheck = failWrite
				err := f.tryReconcile(nil)
				require.Error(t, err)
				require.Equal(t, failWrite, agenticRetryFor(err).transient, "invalid plan hid the failed write")
				f.gh.failCheck = false
				reads := f.gh.getPullRequestCalls
				if failWrite {
					reads++
				}
				advanceAgenticTime(f, agenticInitialBackoff)
				require.Equal(t, reads, f.gh.getPullRequestCalls, "invalid plan retry did not follow persistence outcome")
				advanceAgenticTime(f, defaultAgenticTimeout-agenticInitialBackoff-time.Second)
				require.Equal(t, reads, f.gh.getPullRequestCalls, "persisted invalid plan kept retrying")
				require.Zero(t, f.jobs.creates)
				advanceAgenticTime(f, time.Second)
				_, state := f.gate(t)
				require.NotNil(t, state.Plan)
				require.Equal(t, "timeout", state.Plan.Source)
				require.Equal(t, 2, f.jobs.creates, "invalid plan lost its bounded fallback deadline")
			})
		})
	}
}

func TestAgenticNonRetryableErrorsWaitForEvent(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		established   bool
	}{
		{"new-event-unknown", "unclassified operational failure", false},
		{"pending-deadline-auth", "return code not 2XX: 401 Unauthorized", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
				if tc.established {
					f.reconcile(t, nil)
				}
				f.plan(t, "job-a")
				f.gh.getPullRequestError = errors.New(tc.failure)
				require.Error(t, f.tryReconcile(nil), "expected operational failure")
				assertAgenticIdle(t, f, 24*time.Hour)
				f.gh.getPullRequestError = nil
				f.a.handlePullRequest(f.a.logger, github.PullRequestEvent{Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr})
				if f.jobs.creates != 1 {
					t.Fatal("relevant event did not reactivate failed work")
				}
			})
		})
	}
}

func TestAgenticRateLimitKeepsDeadlineAndOriginalCommand(t *testing.T) {
	for _, kind := range []string{"server-wait", "no-hint"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
				f.reconcile(t, nil)
				advanceAgenticTime(f, defaultAgenticTimeout-time.Second)
				command := f.command(500, "required")
				wait := 11 * time.Minute
				failure := errors.New("sleep time for abuse rate limit exceeds max sleep time (11m0s > 1m0s)")
				if kind == "no-hint" {
					failure = errors.New("return code not 2XX: 429 Too Many Requests")
					wait = time.Minute
				}
				f.gh.getPullRequestError = failure
				event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
					Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *command}
				f.a.handleIssueComment(f.a.logger, event)
				pending := f.a.scheduler.pending[agenticWork{org: "org", repo: "repo", number: 42}]
				if pending == nil || pending.comment == nil || pending.comment.ID != command.ID || pending.deadline.IsZero() || !pending.notBefore.Equal(time.Now().Add(wait)) {
					t.Fatal("routing failure lost the original comment, deadline, or cooldown")
				}
				reads := f.gh.getPullRequestCalls
				f.gh.getPullRequestError = nil
				f.a.handleIssueComment(f.a.logger, event)
				f.a.handlePullRequest(f.a.logger, github.PullRequestEvent{Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr})
				advanceAgenticTime(f, wait-time.Nanosecond)
				if f.gh.getPullRequestCalls != reads || f.jobs.creates != 0 {
					t.Fatal("event or nearer Chai deadline bypassed the targeted cooldown")
				}
				advanceAgenticTime(f, time.Nanosecond)
				_, state := f.gate(t)
				if f.jobs.creates != 2 || state.Plan.Source != "timeout" || state.ForceRequestID != command.ID || len(f.a.scheduler.pending) != 0 {
					t.Fatal("cooldown recovery lost the expired deadline or original request")
				}
			})
		})
	}
}

func TestAgenticCommentRoutingFailurePreservesCommand(t *testing.T) {
	for _, mode := range []string{"manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, mode)
				if mode == "auto" {
					f.passFirstStage(t)
				}
				stop := startAgenticRunner(f)
				defer stop()
				f.reconcile(t, nil)
				commandName, delay, wantCreates := "remaining", agenticInitialBackoff, 0
				if mode == "auto" {
					advanceAgenticTime(f, defaultAgenticTimeout-time.Second)
					commandName, delay, wantCreates = "required", time.Second, 2
				}
				command := f.command(500, commandName)
				f.gh.getPullRequestError = io.ErrUnexpectedEOF
				f.a.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: github.IssueCommentActionCreated,
					Repo: f.gh.pr.Base.Repo, Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: *command})
				f.gh.getPullRequestError = nil
				advanceAgenticTime(f, delay)
				_, state := f.gate(t)
				if state.ManualRequestID != command.ID || (mode == "auto" && state.ForceRequestID != command.ID) || f.jobs.creates != wantCreates || len(f.a.scheduler.pending) != 0 {
					t.Fatal("retry or nearer Chai deadline lost the delivered manual command")
				}
			})
		})
	}
}

func TestAgenticOverlappingFailuresPreserveCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		stop := startAgenticRunner(f)
		defer stop()
		f.reconcile(t, nil)
		command := f.command(500, "remaining")
		// Two routing reads can overlap outside a.mu. The later 503 result must
		// not replace the earlier rate-limit hint with an ordinary short retry.
		f.a.retryReconciliation("org", "repo", 42, command, errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)"))
		f.a.retryReconciliation("org", "repo", 42, nil, errors.New("return code not 2XX: 503 Service Unavailable"))
		pending := f.a.scheduler.pending[agenticWork{org: "org", repo: "repo", number: 42}]
		if pending == nil || pending.comment == nil || pending.comment.ID != command.ID || !pending.notBefore.Equal(time.Now().Add(11*time.Minute)) {
			t.Fatal("overlapping transient result discarded cooldown or original command")
		}
		reads := f.gh.getPullRequestCalls
		advanceAgenticTime(f, 11*time.Minute-time.Nanosecond)
		if f.gh.getPullRequestCalls != reads {
			t.Fatal("overlapping result shortened the server cooldown")
		}
		advanceAgenticTime(f, time.Nanosecond)
		_, state := f.gate(t)
		if state.ManualRequestID != command.ID || len(f.a.scheduler.pending) != 0 {
			t.Fatal("coalesced retry did not recover the original command")
		}
	})
}

func TestAgenticRetryRecoversLostDispatchCommentResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.plan(t, "job-a", "job-b")
		f.passFirstStage(t)
		f.gh.failCommentAfterWrite = true
		stop := startAgenticRunner(f)
		require.Error(t, f.tryReconcile(nil), "expected lost comment response")
		gate, before := f.gate(t)
		require.Equal(t, "failure", gate.Conclusion)
		require.Len(t, f.allJobs(t), 2)
		comments := len(f.gh.comments)
		stop()
		f.a.statusContexts = nil
		stop = startAgenticRunner(f)
		defer stop()
		advanceAgenticTime(f, 5*time.Second)
		if len(f.allJobs(t)) != 2 || len(f.gh.comments) != comments || len(f.a.scheduler.pending) != 0 {
			t.Fatal("targeted retry reposted an already-created comment")
		}
		_, after := f.gate(t)
		require.Equal(t, before.Dispatch.ID, after.Dispatch.ID)
		require.True(t, after.Dispatch.Posted)
	})
}

func TestAgenticShutdownStopsTimers(t *testing.T) {
	for _, timer := range []string{"deadline", "transient-retry"} {
		t.Run(timer, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				if timer == "transient-retry" {
					f.gh.getPullRequestError = io.ErrUnexpectedEOF
				}
				stop := startAgenticRunner(f)
				_ = f.tryReconcile(nil)
				require.Len(t, f.a.scheduler.pending, 1, "fixture did not create the timer being canceled")
				stop()
				reads := f.gh.getPullRequestCalls
				advanceAgenticTime(f, 24*time.Hour)
				if f.a.scheduler != nil || f.gh.getPullRequestCalls != reads || f.jobs.creates != 0 {
					t.Fatal("shutdown left a live reconciliation timer")
				}
			})
		})
	}
}

func TestAgenticConfigurationRecoveryOnlyOnChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		w := newWatcher(filepath.Join(t.TempDir(), "pipeline.yaml"), f.a.logger)
		w.config = f.a.watcher.config
		f.a.watcher = w
		enabled, err := yaml.Marshal(w.config)
		require.NoError(t, err)
		w.config.Orgs[0].Repos[0].Mode.Agentic = AgenticConfig{}
		w.setOnChange(f.a.configurationChanged)
		stop := startAgenticRunner(f)
		defer stop()
		if f.gh.getPullRequestsCalls != 0 {
			t.Fatal("normal-only startup performed an agentic scan")
		}
		require.NoError(t, os.WriteFile(w.filePath, enabled, 0600))
		require.NoError(t, w.reloadConfig())
		if f.gh.getPullRequestsCalls != 0 || len(f.gh.checks) != 0 {
			t.Fatal("new enrollment scanned previously untracked PRs")
		}
		reads, lists := f.gh.getPullRequestCalls, f.gh.getPullRequestsCalls
		for range 3 {
			require.NoError(t, w.reloadConfig())
		}
		if f.gh.getPullRequestCalls != reads || f.gh.getPullRequestsCalls != lists {
			t.Fatal("unchanged local config checks caused GitHub polling")
		}
		invalid := strings.Replace(string(enabled), "mode: chai", "mode: unknown", 1)
		before := w.getConfig()
		require.NoError(t, os.WriteFile(w.filePath, []byte(invalid), 0600))
		if err := w.reloadConfig(); err == nil || f.gh.getPullRequestsCalls != lists {
			t.Fatal("invalid configuration triggered recovery")
		}
		if after := w.getConfig(); !reflect.DeepEqual(before, after) {
			t.Fatalf("invalid reload replaced live configuration: %+v", after)
		}
	})
}

type agenticConfigLogWriter func([]byte) (int, error)

func (write agenticConfigLogWriter) Write(data []byte) (int, error) { return write(data) }

func TestAgenticConfigurationChangesAllowInterleavedDeletion(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	for _, number := range []int{42, 43} {
		pr := f.gh.pr
		pr.Number = number
		require.NoError(t, f.a.writeRecord(t.Context(), &agenticRecord{State: newAgenticState("org", "repo", &pr, f.now)}))
	}
	f.a.startScheduling(t.Context())
	names := f.a.store.recordNames()
	second := f.a.store.entries[names[1]]
	f.gh.getPullRequestError = errors.New("permanent PR read failure")
	interleaved := false
	f.a.logger.Logger.SetOutput(agenticConfigLogWriter(func(data []byte) (int, error) {
		// The first PR's error log is a deterministic boundary before the next
		// tracked PR. Other controller work must be able to take mu here.
		if strings.Contains(string(data), "Cannot reevaluate tracked agentic PR") && f.a.mu.TryLock() {
			defer f.a.mu.Unlock()
			require.NoError(t, f.a.deleteRecord(t.Context(), second.org, second.repo, second.number))
			interleaved = true
		}
		return len(data), nil
	}))
	f.a.configurationChanged()
	require.True(t, interleaved, "configuration batch retained mu between tracked PRs")
	require.Equal(t, 1, f.gh.getPullRequestCalls, "batch reconciled a record deleted after its snapshot")
	require.Len(t, f.a.store.entries, 1, "interleaved deletion disturbed another record")
}
