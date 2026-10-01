package main

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/event"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

func TestAgenticProwJobInitialCreates(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	r := &reconciler{agentic: f.a}
	for _, test := range []struct {
		name, repo, branch string
		initial, want      bool
	}{
		{"agentic-initial", "repo", "main", true, false},
		{"agentic-live", "repo", "main", false, true},
		{"mixed-legacy", "repo", "release", true, true},
		{"normal-only", "normal", "main", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pj := &v1.ProwJob{Spec: v1.ProwJobSpec{Type: v1.PresubmitJob,
				Refs: &v1.Refs{Org: "org", Repo: test.repo, BaseRef: test.branch}}}
			require.Equal(t, test.want, r.shouldReconcileProwJobCreate(event.CreateEvent{Object: pj, IsInInitialList: test.initial}))
		})
	}
	for _, object := range []event.CreateEvent{{}, {Object: (*v1.ProwJob)(nil)}, {Object: &v1.ProwJob{}, IsInInitialList: true}} {
		require.True(t, r.shouldReconcileProwJobCreate(object))
	}
}

func TestAgenticRestartDoesNotScanGitHub(t *testing.T) {
	for _, phase := range []string{"untracked", "first-stage", "manual-trigger", "manual-plan", "empty-plan", "inflight", "completed"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mode := "auto"
				if phase == "manual-trigger" || phase == "manual-plan" {
					mode = "manual"
				}
				f := newScheduledAgenticFixture(t, mode)
				if phase != "untracked" {
					if phase != "first-stage" {
						f.passFirstStage(t)
						if phase == "empty-plan" {
							f.plan(t)
						} else if phase != "manual-trigger" {
							f.plan(t, "job-a")
						}
					}
					f.reconcile(t, nil)
					if phase == "completed" {
						f.report(t, v1.PendingState)
						f.reconcile(t, nil)
					}
					if phase == "empty-plan" || phase == "completed" {
						gate, _ := f.gate(t)
						require.Equal(t, "success", gate.Conclusion)
					}
				}
				// A new process has no scheduler or SHA/context interest cache.
				previous := f.a
				require.NoError(t, previous.closeStore())
				f.a = &agenticController{gh: previous.gh, reader: previous.reader,
					config: previous.config, watcher: previous.watcher, lgtmWatcher: previous.lgtmWatcher,
					appID: previous.appID, logger: previous.logger, options: previous.options, now: previous.now}
				reads, lists := f.gh.getPullRequestCalls, f.gh.getPullRequestsCalls
				checks, comments, statuses := f.gh.listCheckRunsCalls, f.gh.listCommentsCalls, f.gh.getCombinedStatusCalls
				writes, history := len(f.gh.checkWrites), f.gh.listStatusesCalls
				stop := startAgenticRunner(f)
				defer stop()
				assertAgenticIdle(t, f, 24*time.Hour)
				require.Equal(t, reads, f.gh.getPullRequestCalls)
				require.Equal(t, lists, f.gh.getPullRequestsCalls)
				require.Equal(t, checks, f.gh.listCheckRunsCalls)
				require.Equal(t, comments, f.gh.listCommentsCalls)
				require.Equal(t, statuses, f.gh.getCombinedStatusCalls)
				require.Equal(t, history, f.gh.listStatusesCalls)
				require.Len(t, f.gh.checkWrites, writes)
				if phase == "untracked" {
					require.Empty(t, f.gh.checks, "startup discovered a PR that had no persisted work")
				}
			})
		})
	}
}
