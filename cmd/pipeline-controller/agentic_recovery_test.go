package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if len(f.allJobs(t)) != 1 {
		t.Fatal("recovered command did not dispatch after first-stage success")
	}
}

func TestAgenticUnboundInitialCommandRequiresRepost(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.passFirstStage(t)
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
	if !explained {
		t.Fatal("unbound command was silently dropped")
	}
	f.reconcile(t, f.command(600, "required"))
	if f.jobs.creates != 1 {
		t.Fatal("fresh command after the boundary was not honored")
	}
}

func TestAgenticReturningToOldSHAStartsNewDecision(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.passFirstStage(t)
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
	f := newAgenticFixture(t, "auto")
	f.plan(t)
	f.passFirstStage(t)
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

func TestAgenticRepeatedMarkerWriteFailuresCannotReplayOldRequests(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-b")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	_, original := f.gate(t)
	f.gh.failEditComment = true
	f.gh.pr.Base.Ref = "release"
	if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
		t.Fatal("expected marker edit failure")
	}
	f.gh.pr.Base.Ref = "main"
	if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
		t.Fatal("expected second marker edit failure")
	}
	_, pending := f.gate(t)
	if pending.RevisionUpdate == nil || pending.RevisionID == original.RevisionID {
		t.Fatal("fresh revision update was not journaled")
	}
	f.gh.failEditComment = false
	f.passFirstStage(t)
	f.reconcile(t, nil)
	_, state := f.gate(t)
	if state.RevisionID != pending.RevisionID || state.Plan != nil || state.ManualRequestID != 0 || state.RevisionUpdate != nil || f.jobs.creates != 0 {
		t.Fatal("failed marker update rolled the gate back to an old plan/request")
	}
}

func TestAgenticMissingMarkerRetainsFrozenFallback(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.now = f.now.Add(defaultAgenticTimeout)
	f.reconcile(t, nil)
	_, before := f.gate(t)
	var comments []github.IssueComment
	for _, comment := range f.gh.comments {
		if !strings.Contains(comment.Body, agenticRevisionMarker) {
			comments = append(comments, comment)
		}
	}
	f.gh.comments = comments
	f.plan(t) // A late empty plan must not replace the frozen timeout selection.
	f.reconcile(t, nil)
	_, after := f.gate(t)
	if !after.Frozen || after.Plan.Source != "timeout" || !reflect.DeepEqual(before.Dispatch, after.Dispatch) || !reflect.DeepEqual(before.FirstStage, after.FirstStage) {
		t.Fatal("missing tracking comment erased a durable selection or report proof")
	}
}

func TestAgenticFirstStageWitnessSurvivesGarbageCollection(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.plan(t)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	var all v1.ProwJobList
	if err := f.jobs.List(context.Background(), &all); err != nil {
		t.Fatal(err)
	}
	for i := range all.Items {
		if err := f.jobs.Delete(context.Background(), &all.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	f.reconcile(t, nil)
	check, _ := f.gate(t)
	if check.Conclusion != "success" {
		t.Fatal("persisted first-stage evidence was lost after ProwJob cleanup")
	}
	// A raw success without a witnessed ProwJob does not establish a new gate.
	unobserved := newAgenticFixture(t, "auto")
	unobserved.plan(t)
	unobserved.gh.statuses[unobserved.gh.pr.Head.SHA] = f.gh.statuses[f.gh.pr.Head.SHA]
	unobserved.reconcile(t, nil)
	check, _ = unobserved.gate(t)
	if check.Conclusion == "success" {
		t.Fatal("unscoped GitHub status created first-stage proof")
	}
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
	if err := f.a.reconcile(context.Background(), "org", "repo", 43, nil); err == nil {
		t.Fatal("shared HEAD was not blocked")
	}
	if len(f.gh.checks) != 1 || f.gh.checks[0].ID != gate.ID || f.gh.checks[0].Output.Text != gate.Output.Text || f.gh.checks[0].ExternalID != gate.ExternalID || f.gh.checks[0].Conclusion != "failure" {
		t.Fatal("collision failed to close the original gate while retaining its journal")
	}
	f.gh.listCommentsError = errors.New("GitHub unavailable")
	if err := f.a.reconcile(context.Background(), "org", "repo", 43, nil); err == nil {
		t.Fatal("expected read failure")
	}
	if f.gh.checks[0].Output.Text != gate.Output.Text {
		t.Fatal("pre-collision API failure overwrote the owner's journal")
	}
	f.gh.pr, f.gh.otherPRs, f.gh.listCommentsError = owner, nil, nil
	f.plan(t)
	f.reconcile(t, nil)
	_, after := f.gate(t)
	if after.Plan.Source != "timeout" || !reflect.DeepEqual(before.Dispatch, after.Dispatch) {
		t.Fatal("clearing a collision allowed a late Chai reply to replace fallback")
	}
}

func TestAgenticClosedSiblingGateIsReusedWithoutAuthorization(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "remaining"))
	gate, _ := f.gate(t)
	f.gh.pr.Number = 43 // the old PR is closed and absent from GetPullRequests
	f.gh.comments = nil
	if err := f.a.reconcile(context.Background(), "org", "repo", 43, nil); err != nil {
		t.Fatal(err)
	}
	check, state := f.gate(t)
	if check.ID != gate.ID || len(f.gh.checks) != 1 || check.Conclusion == "success" || state.Number != 43 || state.ManualRequestID != 0 || state.Plan != nil {
		t.Fatal("closed sibling left a competing successful gate or stale authorization")
	}
}

func TestAgenticFailedReopeningCannotDispatch(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.plan(t, "job-a")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	f.gh.failCheckAt = f.gh.checkAttempts + 2 // fail after clearing the old success
	if err := f.a.reconcile(context.Background(), "org", "repo", 42, f.command(500, "required")); err == nil {
		t.Fatal("expected reopening failure")
	}
	if f.jobs.creates != 1 || len(f.gh.checks) != 1 || f.gh.checks[0].Conclusion != "failure" {
		t.Fatal("failed reopening left stale success or created reruns")
	}
	f.gh.failCheckAt = 0
	f.reconcile(t, nil)
	if f.jobs.creates != 2 {
		t.Fatal("recorded rerun was not recovered after reopening succeeded")
	}
}

func TestAgenticUnchangedGateIgnoresServerOutputFields(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	f.gh.checks[0].Output.AnnotationsURL = "https://api.github.com/check-runs/1/annotations"
	f.gh.checks[0].Conclusion = "failure" // omitted PATCH conclusion is retained
	before := f.gh.checkAttempts
	f.reconcile(t, nil)
	if f.gh.checkAttempts != before {
		t.Fatal("unchanged waiting state rewrote the CheckRun")
	}
}

func TestAgenticPendingLabelCommandAppliedBeforeNewerCommand(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.gh.failAddLabel = true
	if err := f.a.reconcile(context.Background(), "org", "repo", 42, f.command(500, "skip-agent-review")); err == nil {
		t.Fatal("expected label failure")
	}
	f.gh.failAddLabel = false
	f.reconcile(t, f.command(501, "required"))
	_, state := f.gate(t)
	if !hasAgenticLabel(&f.gh.pr, agenticSkipLabel) || state.Plan.Source != "opt-out" || state.ManualRequestID != 501 || len(state.Plan.Jobs) != 2 {
		t.Fatal("new command overwrote an unapplied durable opt-out")
	}
}

func TestAgenticFrozenSelectionAndOptOutAcrossPushes(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.plan(t, "job-a")
	f.passFirstStage(t)
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
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, f.cfg.GetPresubmitsStatic("org/repo")[1], "manual", nil)
	pj.Name, pj.Namespace, pj.CreationTimestamp = "manually-started", "ci", metav1.NewTime(f.now)
	pj.Status.URL = "https://prow/view/manual/123"
	if err := f.jobs.Client.Create(context.Background(), &pj); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, f.command(500, "remaining"))
	_, state := f.gate(t)
	if !state.Dispatch.Executions[0].Adopted || state.Dispatch.Executions[0].URL != pj.Status.URL || f.jobs.creates != 0 {
		t.Fatal("remaining did not persist the adopted execution identity")
	}
	if err := f.jobs.Delete(context.Background(), &pj); err != nil {
		t.Fatal(err)
	}
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
			f := newAgenticFixture(t, "auto")
			f.plan(t, "job-a", "job-b")
			f.passFirstStage(t)
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
				for _, pj := range f.allJobs(t) {
					if err := f.jobs.Delete(context.Background(), &pj); err != nil {
						t.Fatal(err)
					}
				}
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
					if err := f.jobs.Update(context.Background(), &pj); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-context":
				f.gh.statuses[sha], wantReads = current[:1], 0
			case "history-error":
				f.gh.listStatusesError = io.ErrUnexpectedEOF
			case "first-stage-failure":
				current[0].State, wantReads = github.StatusFailure, 0
			}
			err := f.a.reconcile(context.Background(), "org", "repo", 42, f.command(500, "remaining"))
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
			} else if err != nil {
				t.Fatal(err)
			}
			gate, state := f.gate(t)
			if (gate.Conclusion == "success") != wantReported || state.Dispatch.ID != original.Dispatch.ID {
				t.Fatalf("gate=%s/%s, want reported=%v; dispatch changed=%v", gate.Status, gate.Conclusion, wantReported, state.Dispatch.ID != original.Dispatch.ID)
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
	for _, corruption := range []string{"metadata", "head", "external-id", "request", "dispatch"} {
		t.Run(corruption, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			f.plan(t, "job-a")
			f.reconcile(t, nil)
			_, state := f.gate(t)
			switch corruption {
			case "head":
				state.HeadSHA = "another-head"
			case "external-id":
				f.gh.checks[0].ExternalID = "not-the-controller-identity"
			case "request":
				state.ManualRequestID = 999 // no recorded command authorizes it
			case "dispatch":
				state.Dispatch = &agenticDispatch{ID: "unbound-execution"}
			}
			body, err := agenticMetadata(agenticStateMarker, state)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "metadata" {
				body = "unreadable journal"
			}
			f.gh.checks[0].Output.Text = body
			f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "completed", "success"
			f.passFirstStage(t)
			if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
				t.Fatal("corrupted journal was treated as a fresh dispatch decision")
			}
			if f.jobs.creates != 0 || len(f.gh.checks) != 1 || f.gh.checks[0].Conclusion != "failure" || f.gh.checks[0].Output.Text != body {
				t.Fatal("recovery did not fail closed while preserving diagnostic state")
			}
		})
	}
}

func TestAgenticForeignAppGateDoesNotAuthorizeDispatch(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	_, state := f.gate(t)
	state.ManualRequestID, state.ForceRequestID, state.LastCommandID = 500, 500, 500
	body, err := agenticMetadata(agenticStateMarker, state)
	if err != nil {
		t.Fatal(err)
	}
	f.gh.checks[0].App.ID = 999
	f.gh.checks[0].Output.Text = body
	f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "completed", "success"
	f.passFirstStage(t)
	f.reconcile(t, nil)
	gate, recovered := f.gate(t)
	if gate.ID == 1 || gate.Status != "in_progress" || recovered.ManualRequestID != 0 || f.jobs.creates != 0 || f.gh.checks[0].Output.Text != body {
		t.Fatal("foreign App journal was trusted or modified")
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
	if f.jobs.creates != 1 {
		t.Fatal("LGTM override did not dispatch after first-stage success")
	}
}

func TestAgenticDepartureInvalidatesReturningRevision(t *testing.T) {
	for _, markerState := range []string{"present", "write-failed", "deleted-after-departure", "deleted-before-departure"} {
		t.Run(markerState, func(t *testing.T) {
			f, _ := mixedAgenticFixture(t)
			f.plan(t, "job-a")
			f.reconcile(t, nil)
			f.reconcile(t, f.command(500, "required"))
			gate, old := f.gate(t)
			if old.ManualRequestID != 500 || old.Plan == nil || old.Dispatch != nil {
				t.Fatal("expected a saved early request and plan")
			}
			deleteMarker := func() {
				var kept []github.IssueComment
				for _, comment := range f.gh.comments {
					if !strings.Contains(comment.Body, agenticRevisionMarker) {
						kept = append(kept, comment)
					}
				}
				f.gh.comments = kept
			}
			if markerState == "deleted-before-departure" {
				deleteMarker()
			}
			f.gh.pr.Base.Ref = "release"
			f.gh.failEditComment = markerState == "write-failed"
			err := f.a.reconcile(context.Background(), "org", "repo", 42, nil)
			if (err != nil) != (markerState == "write-failed") {
				t.Fatalf("unexpected departure error: %v", err)
			}
			var inactive agenticState
			if err := parseAgenticMetadata(f.gh.checks[0].Output.Text, agenticStateMarker, &inactive); err != nil {
				t.Fatal(err)
			}
			if inactive.Departure == nil || f.gh.checks[0].Conclusion != "failure" || len(f.gh.checks) != 1 || f.jobs.creates != 0 {
				t.Fatal("departure was not durably recorded in the existing gate")
			}
			if markerState == "deleted-after-departure" {
				deleteMarker()
			}
			f.gh.failEditComment = false
			f.gh.pr.Base.Ref = "main"
			f.passFirstStage(t)
			f.reconcile(t, nil)
			current, returned := f.gate(t)
			if current.ID != gate.ID || current.Conclusion == "success" || returned.ManualRequestID != 0 || returned.Plan != nil || returned.RevisionID == old.RevisionID || returned.Departure != nil || f.jobs.creates != 0 {
				t.Fatal("returning to the same SHA/base revived old selection or authorization")
			}
			f.plan(t, "job-a")
			f.reconcile(t, f.command(900, "required"))
			if f.jobs.creates != 1 {
				t.Fatal("fresh plan and command after reentry did not dispatch")
			}
		})
	}
}

func TestAgenticStartupRecoveryFinishesInactiveDeparture(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.reconcile(t, nil)
	f.gh.pr.Base.Ref = "release"
	f.gh.failEditComment = true
	if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
		t.Fatal("expected marker update failure")
	}
	f.gh.failEditComment = false
	f.a.reconcileRepo(context.Background(), "org", "repo", "")
	var revision agenticRevision
	for _, comment := range f.gh.comments {
		if strings.Contains(comment.Body, agenticRevisionMarker) {
			if err := parseAgenticMetadata(comment.Body, agenticRevisionMarker, &revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !revision.Inactive || revision.BaseBranch != "release" || len(f.gh.checks) != 1 || f.jobs.creates != 0 {
		t.Fatal("startup recovery did not finish the tracked inactive departure")
	}
	writes, comments := len(f.gh.checkWrites), len(f.gh.comments)
	f.a.reconcileRepo(context.Background(), "org", "repo", "")
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
}

func TestAgenticDepartureClosesPreviouslySuccessfulGate(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.a.watcher.config.Orgs[0].Repos[0].Branches = []string{"main"}
	f.plan(t)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	if gate.Conclusion != "success" {
		t.Fatal("expected an initially satisfied empty plan")
	}
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
