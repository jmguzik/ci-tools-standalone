package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/pjutil"
)

func TestAgenticRevisionBoundaries(t *testing.T) {
	for _, transition := range []string{"new-head", "base", "revisited-head"} {
		t.Run(transition, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "manual", "job-a")
			f.reconcile(t, nil)
			f.reconcile(t, f.command(500, "required"))
			f.report(t, v1.SuccessState)
			f.reconcile(t, nil)
			oldGate, old := f.gate(t)
			sha := f.gh.pr.Head.SHA
			f.now = f.now.Add(time.Minute)
			switch transition {
			case "base":
				f.gh.pr.Base.Ref = "release"
			case "revisited-head":
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
				f.reconcile(t, nil)
				f.gh.pr.Head.SHA = sha
			default:
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
			}
			f.plan(t, "job-b") // Present before the new revision is observed.
			f.reconcile(t, nil)
			gate, state := f.gate(t)
			require.NotEqual(t, old.RevisionID, state.RevisionID)
			require.Zero(t, state.ManualRequestID)
			require.Nil(t, state.Dispatch)
			require.NotEqual(t, "success", gate.Conclusion)
			if transition == "new-head" {
				require.NotNil(t, state.Plan, "a plan explicitly bound to an unseen SHA is valid")
			} else {
				require.Equal(t, oldGate.ID, gate.ID)
				require.Nil(t, state.Plan, "reused gates require a fresh decision")
			}
			if transition == "base" {
				require.Nil(t, state.ActivatedAt, "old-base first-stage jobs satisfied the new base")
			}
			f.passFirstStage(t)
			f.plan(t, "job-b")
			f.reconcile(t, f.command(600, "remaining"))
			require.Equal(t, 2, f.jobs.creates)
		})
	}
}

func TestAgenticDispatchSuccessSurvivesRestartAndJobCleanup(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a", "job-b")
	f.reconcile(t, nil)
	var jobs v1.ProwJobList
	require.NoError(t, f.jobs.List(context.Background(), &jobs))
	f.deleteJobs(t, jobs.Items...)
	require.NoError(t, f.a.closeStore())
	f.gh.comments = nil
	f.reconcile(t, nil)
	gate, state := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
	require.Len(t, state.FirstStage, 1)
	require.Len(t, state.Dispatch.Executions, 2, "expected work must not come from the observed subset")
	require.Equal(t, 2, f.jobs.creates)

}

func TestAgenticNewFirstStageRunRevokesOldWitness(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, f.cfg.GetPresubmitsStatic("org/repo")[0], "first", nil)
	pj.Name, pj.Namespace, pj.CreationTimestamp = "new-first-stage", "ci", metav1.NewTime(f.now)
	pj.Status.State = v1.PendingState
	require.NoError(t, f.jobs.Client.Create(context.Background(), &pj))
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	gate, state := f.gate(t)
	require.NotEqual(t, "success", gate.Conclusion)
	require.Empty(t, state.FirstStage)
	require.Nil(t, state.Dispatch)
}

func TestAgenticFrozenSelectionAndOptOutAcrossPushes(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	_, before := f.gate(t)
	f.plan(t, "job-b")
	f.reconcile(t, f.command(500, "agent-review"))
	f.reconcile(t, f.command(600, "skip-agent-review"))
	_, after := f.gate(t)
	require.Equal(t, before.Plan, after.Plan)
	require.Equal(t, before.Dispatch.ID, after.Dispatch.ID)
	require.Nil(t, after.Review)
	require.True(t, hasAgenticLabel(&f.gh.pr, agenticSkipLabel))
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.now = f.now.Add(time.Minute)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	_, after = f.gate(t)
	require.Equal(t, "opt-out", after.Plan.Source)
	require.Len(t, after.Plan.Jobs, 2)
	require.Equal(t, 3, f.jobs.creates)
}

func TestAgenticCompletedDispatchIgnoresLatePlansAndReadFailures(t *testing.T) {
	for _, completion := range []string{"posted", "override"} {
		t.Run(completion, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			f.passFirstStage(t)
			if completion == "posted" {
				f.plan(t, "job-a")
			}
			f.reconcile(t, nil)
			if completion == "override" {
				f.reconcile(t, f.command(500, "tests-dispatched"))
			}
			gate, _ := f.gate(t)
			reads, creates := f.gh.listCommentsCalls, f.jobs.creates
			f.gh.listCommentsError = io.ErrUnexpectedEOF
			latePlan := f.plan(t, "job-b")
			f.reconcile(t, &latePlan)
			require.Equal(t, reads, f.gh.listCommentsCalls, "completed dispatch must not reread irrelevant late plans")
			require.Equal(t, gate.Conclusion, f.gh.checks[0].Conclusion)
			command := f.command(900, "required")
			require.Error(t, f.tryReconcile(command))
			require.Equal(t, gate.Conclusion, f.gh.checks[0].Conclusion, "a failed command read must not revoke completed dispatch")
			require.Equal(t, creates, f.jobs.creates)
			f.gh.listCommentsError = nil
			f.reconcile(t, command)
			require.Equal(t, creates+1, f.jobs.creates, "the command must still work after the read recovers")
		})
	}
}

func TestAgenticUncertainSuccessIsClearedBeforeForcedRerun(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	gate, state := f.gate(t)
	// Success reached GitHub, but the local acknowledgment did not.
	f.gh.checks[0].Status, f.gh.checks[0].Conclusion = "completed", "success"
	state.record.Dirty = true
	require.NoError(t, f.a.writeRecord(context.Background(), state.record))
	f.gh.failCheck = true
	require.Error(t, f.tryReconcile(f.command(500, "required")))
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.True(t, r.Reopen)
	f.gh.failCheck = false
	require.NoError(t, f.a.closeStore())
	f.gh.beforeComment = func(string) { require.NotEqual(t, "success", f.gh.checks[0].Conclusion) }
	f.reconcile(t, nil)
	require.Equal(t, 2, f.jobs.creates)
	require.Equal(t, gate.ID, f.gh.checks[0].ID)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
}

func TestAgenticMalformedRecordFailsClosed(t *testing.T) {
	for _, corruption := range []string{"json", "version", "head", "request", "dispatch"} {
		t.Run(corruption, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto")
			f.reconcile(t, nil)
			gate, state := f.gate(t)
			switch corruption {
			case "version":
				state.Version-- // Do not reuse older report-based/rerun evidence.
			case "head":
				state.HeadSHA = "another-head"
			case "request":
				state.ManualRequestID = 999
			case "dispatch":
				state.Dispatch = &agenticDispatch{ID: "unbound-execution"}
			}
			body, err := json.Marshal(state.record)
			require.NoError(t, err)
			if corruption == "json" {
				body = []byte("unreadable record")
			}
			path := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", 42)+".json")
			require.NoError(t, os.WriteFile(path, body, 0600))
			require.Error(t, f.tryReconcile(nil))
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

func TestAgenticDepartureClosesGateAndRequiresFreshDecision(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.a.watcher.config.Orgs[0].Repos[0].Branches = []string{"main"}
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
	f.gh.pr.Base.Ref = "release"
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	require.NoError(t, f.a.closeStore())
	f.gh.pr.Base.Ref = "main"
	f.reconcile(t, nil)
	current, state := f.gate(t)
	require.Equal(t, gate.ID, current.ID)
	require.NotEqual(t, "success", current.Conclusion)
	require.Nil(t, state.Plan)
	f.gh.pr.State = github.PullRequestStateClosed
	f.reconcile(t, nil)
	require.Empty(t, f.a.store.entries)
}
