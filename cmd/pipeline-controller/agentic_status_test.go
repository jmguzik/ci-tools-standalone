package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticStatusFiltering(t *testing.T) {
	for _, test := range []struct {
		name, context string
		change        func(*agenticFixture, *github.StatusEvent)
		want          bool
	}{
		{"gate", agenticGate, nil, false},
		{"unrelated", "unrelated/check", nil, false},
		{"first-stage", "ci/first", nil, true},
		{"second-stage", "ci/job-a", nil, true},
		{"empty-context", "", nil, false},
		{"empty-sha", "ci/first", func(_ *agenticFixture, e *github.StatusEvent) { e.SHA = "" }, false},
		{"unenrolled", "ci/first", func(_ *agenticFixture, e *github.StatusEvent) { e.Repo.Name = "normal" }, false},
		{"unknown-sha", "unrelated/check", func(_ *agenticFixture, e *github.StatusEvent) { e.SHA = strings.Repeat("c", 40) }, true},
		{"before-recovery", "unrelated/check", func(f *agenticFixture, _ *github.StatusEvent) { f.a.statusContexts = nil }, true},
		{"legacy", "ci/first", func(f *agenticFixture, _ *github.StatusEvent) {
			f.a.watcher.config.Orgs[0].Repos[0].Mode.Agentic = AgenticConfig{}
		}, false},
		{"gate-named-job", agenticGate, func(f *agenticFixture, _ *github.StatusEvent) {
			jobs := f.cfg.GetPresubmitsStatic("org/repo")
			jobs[0].Context = agenticGate
			require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": jobs}))
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			f.reconcile(t, nil) // Establish a known journal, even with no selected jobs yet.
			e := github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: test.context, State: github.StatusPending}
			if test.change != nil {
				test.change(f, &e)
			}
			lists, reads, writes := f.gh.getPullRequestsCalls, f.gh.getPullRequestCalls, len(f.gh.checkWrites)
			f.a.handleStatus(f.a.logger, e)
			if test.want {
				require.Greater(t, f.gh.getPullRequestsCalls, lists)
			} else {
				require.Equal(t, lists, f.gh.getPullRequestsCalls)
				require.Equal(t, reads, f.gh.getPullRequestCalls)
				require.Len(t, f.gh.checkWrites, writes)
			}
		})
	}
}

func TestAgenticStatusKeepsFrozenContextsAfterConfigRemoval(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "restart"}[restart], func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			var jobs []config.Presubmit
			for _, job := range f.cfg.GetPresubmitsStatic("org/repo") {
				if job.Name != "job-a" {
					jobs = append(jobs, job)
				}
			}
			require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": jobs}))
			if restart {
				f.a.statusContexts = nil
			}
			f.report(t, v1.PendingState) // Pending is a valid dispatch report, too.
			f.a.handleStatus(f.a.logger, github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: "ci/job-a", State: github.StatusPending})
			check, _ := f.gate(t)
			require.Equal(t, "success", check.Conclusion)
			require.True(t, f.a.statusContexts[agenticWork{org: "org", repo: "repo", sha: f.gh.pr.Head.SHA}]["ci/job-a"])
		})
	}
}

func TestAgenticStatusRetainsSelectionBeforeFailedWrite(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	f.passFirstStage(t)
	f.plan(t, "job-a")
	f.gh.failCheck = true
	require.Error(t, f.a.reconcile(context.Background(), "org", "repo", 42, nil))
	require.True(t, f.a.statusContexts[agenticWork{org: "org", repo: "repo", sha: f.gh.pr.Head.SHA}]["ci/job-a"])
}

func TestAgenticStatusKeepsRemovedFirstStageContext(t *testing.T) {
	for _, jobContext := range []string{"ci/first", agenticGate} {
		t.Run(jobContext, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			jobs := f.cfg.GetPresubmitsStatic("org/repo")
			jobs[0].Context = jobContext
			require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": jobs}))
			f.plan(t)
			f.reconcile(t, nil)
			check, state := f.gate(t)
			require.NotEqual(t, "success", check.Conclusion)
			require.Empty(t, state.FirstStage) // No success witness exists yet.
			require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": jobs[1:]}))
			f.a.handleStatus(f.a.logger, github.StatusEvent{Repo: f.gh.pr.Base.Repo, SHA: f.gh.pr.Head.SHA, Context: jobContext, State: github.StatusSuccess})
			check, _ = f.gate(t)
			require.Equal(t, "success", check.Conclusion)
		})
	}
}
