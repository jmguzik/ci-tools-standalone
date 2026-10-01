package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/pjutil"
)

func TestAgenticMissedCommandRecoveryAndTimestampPrecision(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.now = f.now.Add(123 * time.Millisecond)
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	command := f.command(500, "remaining")
	// GitHub timestamps have second precision; the comment ID is newer than
	// the marker's recorded boundary even though its timestamp is rounded.
	f.gh.comments[len(f.gh.comments)-1].CreatedAt = command.CreatedAt.Truncate(time.Second)
	f.reconcile(t, nil) // no webhook delivered
	_, state := f.gate(t)
	if state.ManualRequestID != 500 || state.Dispatch != nil {
		t.Fatal("missed early command was not recovered for the observed revision")
	}
	f.passFirstStage(t)
	f.reconcile(t, nil)
	require.Len(t, f.allJobs(t), 1, "recovered command did not dispatch after first-stage success")
}

func TestAgenticUnboundInitialCommandRequiresRepost(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	command := f.command(500, "required") // already present when tracking starts
	f.reconcile(t, command)
	_, state := f.gate(t)
	if state.ManualRequestID != 0 || f.jobs.creates != 0 {
		t.Fatal("historical command was rebound to a newly observed revision")
	}
	var explained bool
	for _, comment := range f.gh.comments {
		explained = explained || strings.Contains(comment.Body, "Please post it again")
	}
	require.True(t, explained, "unbound command was silently dropped")
	f.reconcile(t, f.command(600, "required"))
	require.Equal(t, 1, f.jobs.creates, "fresh command after the boundary was not honored")
}

func TestAgenticEarlyPlanRevisionBoundaries(t *testing.T) {
	for _, transition := range []string{"new-head", "new-head-after-retarget", "same-head-retarget", "revisited-base", "revisited-head", "inactive-new-head"} {
		t.Run(transition, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			oldPlan := f.plan(t, "job-a")
			f.reconcile(t, nil)
			f.reconcile(t, f.command(400, "required"))
			_, original := f.gate(t)
			originalSHA := f.gh.pr.Head.SHA
			f.now = f.now.Add(time.Minute)
			switch transition {
			case "new-head-after-retarget":
				f.gh.pr.Base.Ref = "release"
				f.reconcile(t, nil)
				f.gh.pr.Base.Ref = "main"
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
			case "same-head-retarget":
				f.gh.pr.Base.Ref = "release"
			case "revisited-base":
				f.gh.pr.Base.Ref = "release"
				f.reconcile(t, nil)
				f.gh.pr.Base.Ref = "main"
			case "revisited-head":
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
				f.reconcile(t, nil)
				f.gh.pr.Head.SHA = originalSHA
			case "inactive-new-head":
				f.a.watcher.config.Orgs[0].Repos[0].Branches = []string{"main"}
				f.gh.pr.Base.Ref = "release"
				f.reconcile(t, nil)
				f.gh.pr.Base.Ref, f.gh.pr.Head.SHA = "main", strings.Repeat("c", 40)
			default:
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
			}
			plan := f.plan(t, "job-b") // Visible before the new revision is observed.
			f.command(500, "required") // Prose commands cannot bind before observation.
			wantPlan := transition == "new-head" || transition == "new-head-after-retarget"
			if wantPlan {
				oldPlan.ID = 501 // A later reply for the old SHA must still be ignored.
				f.gh.comments = append(f.gh.comments, oldPlan)
			}
			f.now = f.now.Add(time.Second)
			f.reconcile(t, nil)
			_, pending := f.gate(t)
			require.NotEqual(t, original.RevisionID, pending.RevisionID)
			f.passFirstStage(t)
			f.reconcile(t, nil)
			_, state := f.gate(t)
			require.Equal(t, pending.RevisionID, state.RevisionID, "first-stage readiness replayed an older revision")
			require.GreaterOrEqual(t, state.LastCommandID, 500)
			require.Zero(t, state.ManualRequestID, "early command rebound to a new revision")
			require.Nil(t, state.Command)
			if wantPlan {
				require.NotNil(t, state.Plan, "early plan for an unseen SHA was rejected")
				require.Equal(t, plan.ID, state.Plan.CommentID)
				require.Zero(t, state.PlanCommentFloor, "prior plan boundary leaked to an unseen SHA")
				require.Nil(t, state.PlanNotBefore)
				require.Equal(t, 1, f.jobs.creates)
			} else {
				require.Nil(t, state.Plan, "reused or inactive revision accepted an early plan")
				require.Zero(t, f.jobs.creates)
				f.plan(t, "job-b")
				f.reconcile(t, f.command(600, "required"))
				require.Equal(t, 1, f.jobs.creates, "fresh plan and command failed after the boundary")
			}
		})
	}
}

func TestAgenticReturningToOldSHAStartsNewDecision(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	gate, previous := f.gate(t)
	originalSHA := f.gh.pr.Head.SHA
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.reconcile(t, nil)
	f.gh.pr.Head.SHA = originalSHA
	// Keep the same clock second to exercise the chained revision identity.
	f.reconcile(t, nil)
	current, state := f.gate(t)
	if state.RevisionID == previous.RevisionID || state.Plan != nil || state.ManualRequestID != 0 || state.Dispatch != nil || current.ID != gate.ID || current.Status != "in_progress" || current.Conclusion == "success" {
		t.Fatalf("returning to an old SHA reused its previous authorization/green gate: %+v", state)
	}
}

func TestAgenticRetargetDoesNotReuseFirstStageStatus(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	f.now = f.now.Add(time.Minute)
	f.gh.pr.Base.Ref = "release"
	f.reconcile(t, nil)
	f.plan(t)
	f.reconcile(t, nil)
	check, state := f.gate(t)
	if check.ID != gate.ID || check.Status != "in_progress" || check.Conclusion == "success" || len(state.FirstStage) != 0 {
		t.Fatal("old-base commit status qualified as current-base first-stage evidence")
	}
	f.passFirstStage(t)
	f.reconcile(t, nil)
	check, _ = f.gate(t)
	if check.Conclusion != "success" || len(f.gh.checks) != 1 {
		t.Fatal("fresh base-scoped evidence failed to open the same gate")
	}
}

func TestAgenticFirstStageWitnessSurvivesGarbageCollection(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	var all v1.ProwJobList
	require.NoError(t, f.jobs.List(context.Background(), &all))
	f.deleteJobs(t, all.Items...)
	firstStageStatuses := append([]github.Status(nil), f.gh.statuses[f.gh.pr.Head.SHA]...)
	f.gh.statuses = nil
	f.reconcile(t, nil)
	check, _ := f.gate(t)
	require.Equal(t, "success", check.Conclusion, "persisted first-stage evidence was lost after ProwJob cleanup")
	// A raw success without a witnessed ProwJob does not establish a new gate.
	unobserved := newAgenticFixture(t, "auto")
	unobserved.plan(t)
	unobserved.gh.statuses[unobserved.gh.pr.Head.SHA] = firstStageStatuses
	unobserved.reconcile(t, nil)
	check, _ = unobserved.gate(t)
	require.NotEqual(t, "success", check.Conclusion, "unscoped GitHub status created first-stage proof")
}

func TestAgenticSharedSHAKeepsOwnersJournalOnConflictAndReadError(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.now = f.now.Add(defaultAgenticTimeout)
	f.reconcile(t, nil)
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	gate, before := f.gate(t)
	owner := f.gh.pr
	f.gh.pr.Number = 43
	f.gh.otherPRs = []github.PullRequest{owner}
	require.Error(t, f.tryReconcile(nil), "shared HEAD was not blocked")
	if len(f.gh.checks) != 1 || f.gh.checks[0].ID != gate.ID || f.gh.checks[0].Output.Text != gate.Output.Text || f.gh.checks[0].ExternalID != gate.ExternalID || f.gh.checks[0].Conclusion != "failure" {
		t.Fatal("collision failed to close the original gate while retaining its journal")
	}
	f.gh.listCommentsError = errors.New("GitHub unavailable")
	require.Error(t, f.tryReconcile(nil), "expected read failure")
	require.Equal(t, gate.Output.Text, f.gh.checks[0].Output.Text, "pre-collision API failure overwrote the owner's journal")
	f.gh.pr, f.gh.otherPRs, f.gh.listCommentsError = owner, nil, nil
	f.plan(t)
	f.reconcile(t, nil)
	_, after := f.gate(t)
	if after.Plan.Source != "timeout" || !reflect.DeepEqual(before.Dispatch, after.Dispatch) {
		t.Fatal("clearing a collision allowed a late Chai reply to replace fallback")
	}
}

func TestAgenticFailedReopeningCannotDispatch(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	f.gh.failCheckAt = f.gh.checkAttempts + 2 // fail after clearing the old success
	require.Error(t, f.tryReconcile(f.command(500, "required")), "expected reopening failure")
	if f.jobs.creates != 1 || len(f.gh.checks) != 1 || f.gh.checks[0].Conclusion != "failure" {
		t.Fatal("failed reopening left stale success or created reruns")
	}
	f.gh.failCheckAt = 0
	f.reconcile(t, nil)
	require.Equal(t, 2, f.jobs.creates, "recorded rerun was not recovered after reopening succeeded")
}

func TestAgenticUnchangedGatePersistsStateAndIgnoresServerOutputFields(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	f.gh.checks[0].Output.AnnotationsURL = "https://api.github.com/check-runs/1/annotations"
	f.gh.checks[0].Conclusion = "failure" // omitted PATCH conclusion is retained
	gate, state := f.gate(t)
	before := f.gh.checkAttempts
	state.LastCommandID = 500
	require.NoError(t, f.a.saveState(&gate, state, gate.Conclusion, gate.Output.Summary))
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.Equal(t, 500, r.State.LastCommandID)
	require.Nil(t, r.Desired, "unchanged publication left unfinished intent")
	require.False(t, r.Reopen)
	for range 3 {
		f.reconcile(t, nil)
	}
	require.Equal(t, before, f.gh.checkAttempts, "unchanged waiting state rewrote the CheckRun")
}

func TestAgenticPendingLabelCommandAppliedBeforeNewerCommand(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.gh.failAddLabel = true
	require.Error(t, f.tryReconcile(f.command(500, "skip-agent-review")), "expected label failure")
	f.gh.failAddLabel = false
	f.reconcile(t, f.command(501, "required"))
	_, state := f.gate(t)
	if !hasAgenticLabel(&f.gh.pr, agenticSkipLabel) || state.Plan.Source != "opt-out" || state.ManualRequestID != 501 || len(state.Plan.Jobs) != 2 {
		t.Fatal("new command overwrote an unapplied durable opt-out")
	}
}

func TestAgenticFrozenSelectionAndOptOutAcrossPushes(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	_, before := f.gate(t)
	f.reconcile(t, f.command(500, "agent-review"))
	f.reconcile(t, f.command(600, "skip-agent-review"))
	_, after := f.gate(t)
	if !reflect.DeepEqual(before.Plan, after.Plan) || before.Dispatch.ID != after.Dispatch.ID || after.Review != nil || !hasAgenticLabel(&f.gh.pr, agenticSkipLabel) {
		t.Fatal("command replaced a frozen selection or lost persistent opt-out")
	}
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.now = f.now.Add(time.Minute)
	f.passFirstStage(t)
	f.plan(t)
	f.reconcile(t, nil)
	_, after = f.gate(t)
	if after.Plan.Source != "opt-out" || len(after.Plan.Jobs) != 2 || f.jobs.creates != 3 {
		t.Fatal("opt-out did not carry normal selection across the push")
	}
}

func TestAgenticAdoptedExecutionReportSurvivesGarbageCollection(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, f.cfg.GetPresubmitsStatic("org/repo")[1], "manual", nil)
	pj.Name, pj.Namespace, pj.CreationTimestamp = "manually-started", "ci", metav1.NewTime(f.now)
	pj.Status.URL = "https://prow/view/manual/123"
	pj.Status.PrevReportStates = map[string]v1.ProwJobState{"github-reporter": v1.PendingState}
	require.NoError(t, f.jobs.Client.Create(context.Background(), &pj))
	f.reconcile(t, f.command(500, "remaining"))
	_, state := f.gate(t)
	if state.Dispatch.Executions[0].Name != pj.Name || state.Dispatch.Executions[0].URL != pj.Status.URL || f.jobs.creates != 0 {
		t.Fatal("remaining did not persist the adopted execution identity")
	}
	f.deleteJobs(t, pj)
	f.gh.statuses[f.gh.pr.Head.SHA] = append(f.gh.statuses[f.gh.pr.Head.SHA], github.Status{Context: pj.Spec.Context, State: github.StatusFailure, TargetURL: pj.Status.URL})
	f.reconcile(t, nil)
	check, state := f.gate(t)
	if check.Conclusion != "success" || !state.Dispatch.Executions[0].Reported || f.jobs.creates != 0 {
		t.Fatal("GitHub report for a witnessed deleted execution was not recovered")
	}
}

func TestAgenticSupersededReportRecovery(t *testing.T) {
	for _, scenario := range []string{"superseded", "deleted", "current", "wrong-context", "wrong-url", "wrong-sha", "invalid-state", "missing-history", "missing-ack", "missing-url", "missing-context", "history-error", "first-stage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a", "job-b")
			f.reconcile(t, nil)
			_, original := f.gate(t)
			f.report(t, v1.PendingState)
			sha := f.gh.pr.Head.SHA
			current := f.gh.statuses[sha]
			f.gh.statusHistory = map[string][]github.Status{sha: append([]github.Status{}, current...)}
			if scenario == "deleted" {
				// Persist the URLs before GC, without yet observing their reports.
				f.gh.statuses[sha] = current[:1]
				f.reconcile(t, nil)
				f.deleteJobs(t, f.allJobs(t)...)
				f.gh.statuses[sha] = current
			}
			if scenario != "current" {
				// A later /test report replaces each selected execution's latest URL.
				for i := range current {
					if current[i].Context != "ci/first" {
						current[i].TargetURL += "/rerun"
						current[i].State = github.StatusFailure
					}
				}
			}
			wantReported, wantReads := false, 1
			switch scenario {
			case "superseded", "deleted":
				wantReported = true
			case "current":
				wantReported, wantReads = true, 0
			case "wrong-context", "wrong-url", "invalid-state":
				for i := range f.gh.statusHistory[sha] {
					switch scenario {
					case "wrong-context":
						f.gh.statusHistory[sha][i].Context += "/other"
					case "wrong-url":
						f.gh.statusHistory[sha][i].TargetURL += "/other"
					case "invalid-state":
						f.gh.statusHistory[sha][i].State = ""
					}
				}
			case "wrong-sha":
				f.gh.statusHistory = map[string][]github.Status{"another-head": f.gh.statusHistory[sha]}
			case "missing-history":
				f.gh.statusHistory = nil
			case "missing-ack", "missing-url":
				wantReads = 0
				for _, pj := range f.allJobs(t) {
					if scenario == "missing-ack" {
						pj.Status.PrevReportStates = nil
					} else {
						pj.Status.URL = ""
					}
					require.NoError(t, f.jobs.Update(context.Background(), &pj))
				}
			case "missing-context":
				f.gh.statuses[sha], wantReads = current[:1], 0
			case "history-error":
				f.gh.listStatusesError = io.ErrUnexpectedEOF
			case "first-stage-failure":
				current[0].State, wantReads = github.StatusFailure, 0
				var jobs v1.ProwJobList
				require.NoError(t, f.jobs.List(context.Background(), &jobs))
				for _, pj := range jobs.Items {
					if pj.Spec.Job == "first-stage" {
						pj.Status.State = v1.FailureState
						require.NoError(t, f.jobs.Update(context.Background(), &pj))
					}
				}
			}
			err := f.tryReconcile(f.command(500, "remaining"))
			if scenario == "history-error" {
				if !errors.Is(err, io.ErrUnexpectedEOF) || !agenticRetryFor(err).transient {
					t.Fatalf("history failure did not remain retryable: %v", err)
				}
				gate, state := f.gate(t)
				if gate.Conclusion == "success" || state.Dispatch.Executions[0].Reported || f.gh.listStatusesCalls != 1 {
					t.Fatal("failed history lookup accepted incomplete evidence")
				}
				f.gh.listStatusesError = nil
				f.reconcile(t, nil)
				wantReported, wantReads = true, 2
			} else {
				require.NoError(t, err)
			}
			gate, state := f.gate(t)
			if (gate.Conclusion == "success") != wantReported || !reflect.DeepEqual(state.Plan, original.Plan) {
				t.Fatalf("gate=%s/%s, want reported=%v; selection changed", gate.Status, gate.Conclusion, wantReported)
			}
			for _, execution := range state.Dispatch.Executions {
				if execution.Reported != wantReported {
					t.Fatalf("reported=%v, want %v: %+v", execution.Reported, wantReported, execution)
				}
			}
			if f.gh.listStatusesCalls != wantReads || f.jobs.creates != 2 || f.gh.statusWrites != 0 {
				t.Fatalf("history reads=%d, want %d; job creates=%d; status writes=%d", f.gh.listStatusesCalls, wantReads, f.jobs.creates, f.gh.statusWrites)
			}
			if wantReported {
				f.reconcile(t, nil)
				if f.gh.listStatusesCalls != wantReads {
					t.Fatal("recovered report was not persisted in the journal")
				}
			}
		})
	}
}

func TestAgenticMalformedJournalFailsClosed(t *testing.T) {
	for _, corruption := range []string{"metadata", "head", "external-id", "request", "command", "dispatch"} {
		t.Run(corruption, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto")
			f.reconcile(t, nil)
			gate, state := f.gate(t)
			require.Equal(t, "success", gate.Conclusion)
			r := state.record
			switch corruption {
			case "head":
				state.HeadSHA = "another-head"
			case "external-id":
				r.Gate.ExternalID = "not-the-controller-identity"
			case "request":
				state.ManualRequestID = 999 // no recorded command authorizes it
			case "command":
				state.LastCommandID = 0
				state.Command = &agenticCommand{Command: "remaining"}
			case "dispatch":
				state.Dispatch = &agenticDispatch{ID: "unbound-execution"}
			}
			body, err := json.Marshal(r)
			require.NoError(t, err)
			if corruption == "metadata" {
				body = []byte("unreadable record")
			}
			path := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", 42)+".json")
			require.NoError(t, os.WriteFile(path, body, 0600))
			require.Error(t, f.tryReconcile(nil), "corrupted journal was treated as a fresh dispatch decision")
			require.Zero(t, f.jobs.creates)
			require.Len(t, f.gh.checks, 1)
			require.Equal(t, gate.ID, f.gh.checks[0].ID)
			require.Equal(t, "failure", f.gh.checks[0].Conclusion)
			preserved, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, body, preserved)
		})
	}
}

func TestAgenticForeignAppGateDoesNotAuthorizeDispatch(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	body := f.gh.checks[0].Output.Text
	f.gh.checks[0].App.ID = 999
	f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "completed", "success"
	require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 42))
	f.passFirstStage(t)
	f.reconcile(t, nil)
	gate, recovered := f.gate(t)
	if gate.ID == 1 || gate.Status != "in_progress" || recovered.ManualRequestID != 0 || f.jobs.creates != 0 || f.gh.checks[0].Output.Text != body {
		t.Fatal("foreign App gate was reused or modified")
	}
}

func TestAgenticAutoOverrideStillWaitsForFirstStage(t *testing.T) {
	f := newAgenticFixture(t, "lgtm")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "auto"))
	f.now = f.now.Add(time.Hour)
	f.reconcile(t, nil)
	_, state := f.gate(t)
	if !hasAgenticLabel(&f.gh.pr, PipelineAutoLabel) || hasAgenticLabel(&f.gh.pr, "lgtm") || state.WaitingSince != nil || f.jobs.creates != 0 {
		t.Fatal("LGTM override bypassed first-stage readiness")
	}
	f.passFirstStage(t)
	f.reconcile(t, nil)
	require.Equal(t, 1, f.jobs.creates, "LGTM override did not dispatch after first-stage success")
}

func TestAgenticDepartureInvalidatesReturningRevision(t *testing.T) {
	for _, source := range []string{"local-record", "missing-record", "missing-record-other-head"} {
		t.Run(source, func(t *testing.T) {
			f, _ := mixedAgenticFixture(t)
			f.plan(t, "job-a")
			f.reconcile(t, nil)
			f.reconcile(t, f.command(500, "required"))
			gate, old := f.gate(t)
			if old.ManualRequestID != 500 || old.Plan == nil || old.Dispatch != nil {
				t.Fatal("expected a saved early request and plan")
			}
			originalSHA := f.gh.pr.Head.SHA
			if source != "local-record" {
				require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 42))
				if source == "missing-record-other-head" {
					f.gh.pr.Head.SHA = strings.Repeat("c", 40)
				}
			}
			reads, writes := f.gh.listCheckRunsCalls+f.gh.listCommentsCalls, len(f.gh.checkWrites)
			f.gh.pr.Base.Ref = "release"
			err := f.tryReconcile(nil)
			if err != nil {
				t.Fatalf("unexpected departure error: %v", err)
			}
			r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
			require.NoError(t, err)
			if !r.State.Inactive || len(f.gh.checks) != 1 || f.jobs.creates != 0 {
				t.Fatal("departure was not durably recorded in the existing gate")
			}
			if source == "local-record" {
				require.Equal(t, "failure", f.gh.checks[0].Conclusion)
			} else {
				require.Zero(t, r.Gate.ID)
				require.False(t, r.State.RevisionPending)
				require.Equal(t, reads, f.gh.listCheckRunsCalls+f.gh.listCommentsCalls)
				require.Len(t, f.gh.checkWrites, writes, "missing-state departure published a gate")
			}
			require.NoError(t, f.a.closeStore()) // Reentry must honor the durable departure after restart.
			f.gh.pr.Head.SHA = originalSHA
			f.gh.pr.Base.Ref = "main"
			f.passFirstStage(t)
			f.reconcile(t, nil)
			current, returned := f.gate(t)
			if current.ID != gate.ID || current.Conclusion == "success" || returned.ManualRequestID != 0 || returned.Plan != nil || returned.RevisionID == old.RevisionID || returned.Inactive || f.jobs.creates != 0 {
				t.Fatal("returning to the same SHA/base revived old selection or authorization")
			}
			f.plan(t, "job-a")
			f.reconcile(t, f.command(900, "required"))
			require.Equal(t, 1, f.jobs.creates, "fresh plan and command after reentry did not dispatch")
		})
	}
}

func TestAgenticRetryFinishesInactiveDeparture(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.reconcile(t, nil)
	f.gh.pr.Base.Ref = "release"
	f.gh.failCheck = true
	require.Error(t, f.tryReconcile(nil), "expected gate update failure")
	f.gh.failCheck = false
	f.reconcile(t, nil)
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	if !r.State.Inactive || r.Desired != nil || f.gh.checks[0].Conclusion != "failure" || len(f.gh.checks) != 1 || f.jobs.creates != 0 {
		t.Fatal("retry did not finish the tracked inactive departure")
	}
	writes, comments := len(f.gh.checkWrites), len(f.gh.comments)
	f.reconcile(t, nil)
	if len(f.gh.checkWrites) != writes || len(f.gh.comments) != comments {
		t.Fatal("already-recorded departure caused repeated writes")
	}
}

func TestAgenticDepartureNeverCreatesOrdinaryBranchGate(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.gh.pr.Base.Ref = "release"
	f.reconcile(t, nil)
	if len(f.gh.checks) != 0 || len(f.gh.comments) != 0 || f.jobs.creates != 0 {
		t.Fatal("ordinary untracked PR acquired agentic state")
	}
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.True(t, r.State.Inactive)
	reads := f.gh.getPullRequestCalls
	f.a.reconcileRepo(context.Background(), "org", "repo", f.gh.pr.Head.SHA)
	require.Equal(t, reads, f.gh.getPullRequestCalls, "normal-branch statuses woke an inactive departure")
	f.gh.pr.State = github.PullRequestStateClosed
	f.reconcile(t, nil)
	require.Empty(t, f.a.store.entries, "observed closure retained the departure record")
}

func TestAgenticDepartureClosesPreviouslySuccessfulGate(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.a.watcher.config.Orgs[0].Repos[0].Branches = []string{"main"}
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	require.Equal(t, "success", gate.Conclusion, "expected an initially satisfied empty plan")
	f.gh.pr.Base.Ref = "release"
	f.reconcile(t, nil)
	if len(f.gh.checks) != 1 || f.gh.checks[0].ID != gate.ID || f.gh.checks[0].Conclusion != "failure" {
		t.Fatal("departure left an old successful gate usable")
	}
	f.gh.pr.Base.Ref = "main"
	f.reconcile(t, nil)
	current, state := f.gate(t)
	if current.Conclusion == "success" || state.Plan != nil {
		t.Fatal("returning to enrollment reused the old empty plan")
	}
}
