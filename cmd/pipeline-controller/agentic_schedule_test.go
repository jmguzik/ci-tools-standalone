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
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.a.Run(ctx)
	}()
	synctest.Wait()
	return func() {
		cancel()
		<-done
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

func TestAgenticRunDoesNotPoll(t *testing.T) {
	for _, phase := range []string{"first-stage", "manual-trigger", "reported"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mode := "auto"
				if phase == "manual-trigger" {
					mode = "manual"
				}
				f := newScheduledAgenticFixture(t, mode)
				if phase != "first-stage" {
					f.passFirstStage(t)
				}
				if phase == "reported" {
					f.plan(t) // No additional tests; startup can complete the gate.
				}
				stop := startAgenticRunner(f)
				defer stop()
				assertAgenticIdle(t, f, 24*time.Hour)
			})
		})
	}
}

func TestAgenticDeadlineSurvivesRestartWithoutPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		stop := startAgenticRunner(f)
		_, original := f.gate(t)
		if original.WaitingSince == nil || len(f.a.scheduler.pending) != 1 {
			t.Fatal("missing one-shot Chai deadline")
		}
		reads := f.gh.getPullRequestCalls
		advanceAgenticTime(f, 15*time.Minute)
		if f.gh.getPullRequestCalls != reads {
			t.Fatal("polled before the Chai deadline")
		}
		stop()
		advanceAgenticTime(f, 4*time.Minute)
		stop = startAgenticRunner(f)
		defer stop()
		_, recovered := f.gate(t)
		if recovered.WaitingSince == nil || !recovered.WaitingSince.Equal(*original.WaitingSince) {
			t.Fatal("restart reset the persisted waiting timestamp")
		}
		advanceAgenticTime(f, time.Minute-time.Nanosecond)
		if f.jobs.creates != 0 {
			t.Fatal("fallback ran before the restored deadline")
		}
		advanceAgenticTime(f, time.Nanosecond)
		_, state := f.gate(t)
		if state.Plan == nil || state.Plan.Source != "timeout" || f.jobs.creates != 2 || len(f.a.scheduler.pending) != 0 {
			t.Fatal("deadline did not dispatch once and retire its timer")
		}
		assertAgenticIdle(t, f, 24*time.Hour)
		f.report(t, v1.PendingState)
		f.a.handleStatus(f.a.logger, github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: "ci/job-a", State: "pending"})
		gate, _ := f.gate(t)
		if gate.Conclusion != "success" {
			t.Fatal("report event failed to finish dispatch")
		}
	})
}

func TestAgenticOverdueDeadlineRecoveredAtStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		stop := startAgenticRunner(f)
		stop()
		advanceAgenticTime(f, defaultAgenticTimeout+time.Minute)
		stop = startAgenticRunner(f)
		defer stop()
		_, state := f.gate(t)
		if state.Plan == nil || state.Plan.Source != "timeout" || f.jobs.creates != 2 || len(f.a.scheduler.pending) != 0 {
			t.Fatal("startup did not act on an already-expired persisted deadline")
		}
	})
}

func TestAgenticEventsCancelChaiDeadline(t *testing.T) {
	for _, change := range []string{"plan", "push", "retarget", "draft", "closed"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
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
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		f.plan(t, "unknown-job")
		stop := startAgenticRunner(f)
		defer stop()
		reads := f.gh.getPullRequestCalls
		advanceAgenticTime(f, defaultAgenticTimeout-time.Second)
		if f.gh.getPullRequestCalls != reads || f.jobs.creates != 0 {
			t.Fatal("unchanged invalid plan was repeatedly fetched")
		}
		advanceAgenticTime(f, time.Second)
		_, state := f.gate(t)
		if state.Plan == nil || state.Plan.Source != "timeout" || f.jobs.creates != 2 {
			t.Fatal("invalid plan lost its bounded fallback deadline")
		}
	})
}

func TestAgenticInvalidPlanPersistenceFailureRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.passFirstStage(t)
		stop := startAgenticRunner(f)
		defer stop()
		f.plan(t, "unknown-job")
		f.gh.failCheck = true
		if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
			t.Fatal("expected a failed journal write")
		}
		f.gh.failCheck = false
		reads := f.gh.getPullRequestCalls
		advanceAgenticTime(f, 5*time.Second)
		if f.gh.getPullRequestCalls != reads+1 {
			t.Fatal("an invalid plan suppressed retrying its failed journal write")
		}
		reads = f.gh.getPullRequestCalls
		advanceAgenticTime(f, time.Minute)
		if f.gh.getPullRequestCalls != reads {
			t.Fatal("successfully persisted invalid plan kept retrying")
		}
	})
}

func TestAgenticNonRetryableErrorsWaitForEvent(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		startup       bool
	}{
		{"startup-unknown", "unclassified operational failure", true},
		{"pending-deadline-auth", "return code not 2XX: 401 Unauthorized", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				if tc.startup {
					f.gh.getPullRequestError = errors.New(tc.failure)
				}
				stop := startAgenticRunner(f)
				defer stop()
				f.plan(t, "job-a")
				if !tc.startup {
					f.gh.getPullRequestError = errors.New(tc.failure)
					require.Error(t, f.tryReconcile(nil), "expected operational failure")
				}
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

func TestAgenticMalformedJournalWriteFailureRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		stop := startAgenticRunner(f)
		defer stop()
		f.gh.checks[0].Output.Text = "invalid saved state"
		f.gh.failCheck = true
		if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil || !agenticRetryFor(err).transient {
			t.Fatal("terminal journal error hid its transient failure-report error")
		}
		f.gh.failCheck = false
		advanceAgenticTime(f, agenticInitialBackoff)
		if f.gh.checks[0].Conclusion != "failure" || len(f.a.scheduler.pending) != 0 || f.jobs.creates != 0 {
			t.Fatal("failure-report recovery did not stop after recording the terminal error")
		}
		assertAgenticIdle(t, f, time.Hour)
	})
}

func TestAgenticRateLimitKeepsDeadlineAndOriginalCommand(t *testing.T) {
	for _, kind := range []string{"server-wait", "no-hint"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				f.passFirstStage(t)
				stop := startAgenticRunner(f)
				defer stop()
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

func TestAgenticRetryFinishesPartialDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "auto")
		f.plan(t, "job-a", "job-b")
		f.passFirstStage(t)
		f.jobs.failAt = 2
		stop := startAgenticRunner(f)
		defer stop()
		if len(f.allJobs(t)) != 1 {
			t.Fatal("did not reach a partial dispatch")
		}
		first := f.allJobs(t)[0].Name
		advanceAgenticTime(f, 5*time.Second)
		if len(f.allJobs(t)) != 2 || f.jobs.creates != 3 || len(f.a.scheduler.pending) != 0 {
			t.Fatal("targeted retry did not finish exactly the missing execution")
		}
		found := false
		for _, job := range f.allJobs(t) {
			found = found || job.Name == first
		}
		if !found {
			t.Fatal("retry replaced an already-created execution")
		}
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
		w := f.a.watcher
		enabled, err := yaml.Marshal(w.config)
		require.NoError(t, err)
		w.config.Orgs[0].Repos[0].Mode.Agentic = AgenticConfig{}
		w.filePath = filepath.Join(t.TempDir(), "pipeline.yaml")
		w.setOnChange(f.a.configurationChanged)
		stop := startAgenticRunner(f)
		defer stop()
		if f.gh.getPullRequestsCalls != 0 {
			t.Fatal("normal-only startup performed an agentic scan")
		}
		require.NoError(t, os.WriteFile(w.filePath, enabled, 0600))
		require.NoError(t, w.reloadConfig())
		if f.gh.getPullRequestsCalls == 0 || len(f.gh.checks) != 1 {
			t.Fatal("new enrollment did not recover existing PRs")
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

func TestAgenticInformerResyncIsNotGitHubPolling(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	r := &reconciler{agentic: f.a}
	old := &v1.ProwJob{Spec: v1.ProwJobSpec{Refs: &v1.Refs{Org: "org", Repo: "repo", BaseRef: "main"}}}
	old.ResourceVersion = "10"
	current := old.DeepCopy()
	update := event.UpdateEvent{ObjectOld: old, ObjectNew: current}
	if r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("unchanged agentic informer resync was accepted")
	}
	current.ResourceVersion = "11"
	if !r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("a real ProwJob update was dropped")
	}
	current.ResourceVersion, current.Spec.Refs.BaseRef = "10", "release"
	if r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("old normal-branch jobs can still cause GitHub polling after retargeting")
	}
	r.agentic = nil
	if !r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("normal-only controller update behavior changed")
	}
}
