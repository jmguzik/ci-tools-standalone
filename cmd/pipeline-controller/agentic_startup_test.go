package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/source"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

type fakeProwJobSyncSource struct {
	source.SyncingSource
	wait func(context.Context) error
}

func (s fakeProwJobSyncSource) WaitForSync(ctx context.Context) error { return s.wait(ctx) }

func TestProwJobSourceReadiness(t *testing.T) {
	for _, outcome := range []string{"synced", "error", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &readyProwJobSource{ready: make(chan struct{}), SyncingSource: fakeProwJobSyncSource{
				wait: func(context.Context) error {
					if outcome == "error" {
						return errors.New("handler did not sync")
					}
					if outcome == "canceled" {
						cancel() // Kind.WaitForSync can return nil on cancellation.
					}
					return nil
				},
			}}
			err := s.WaitForSync(ctx)
			if outcome == "synced" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			select {
			case <-s.ready:
				require.Equal(t, "synced", outcome)
			default:
				require.NotEqual(t, "synced", outcome)
			}
		})
	}
}

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

func TestAgenticStartupWaitsForWatch(t *testing.T) {
	for _, outcome := range []string{"recover", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "auto")
				ready := make(chan struct{})
				f.a.prowJobWatchReady = ready
				stop := startAgenticRunner(f)
				defer stop()
				require.Zero(t, f.gh.getPullRequestsCalls)
				require.Zero(t, f.gh.getPullRequestCalls)
				require.Empty(t, f.gh.checkWrites)
				if outcome == "cancel" {
					stop()
					require.Zero(t, f.gh.getPullRequestsCalls)
					require.Nil(t, f.a.scheduler)
					return
				}
				// Changes before the handler is ready must be seen by recovery,
				// even though its initial ProwJob notifications will be skipped.
				f.passFirstStage(t)
				f.plan(t)
				close(ready)
				synctest.Wait()
				check, _ := f.gate(t)
				require.Equal(t, "success", check.Conclusion)
				require.Equal(t, 2, f.gh.getPullRequestsCalls) // One scan plus the existing collision guard.
				assertAgenticIdle(t, f, 24*time.Hour)
			})
		})
	}
}
