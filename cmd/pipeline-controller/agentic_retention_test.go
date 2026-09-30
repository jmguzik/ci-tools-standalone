package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

func ageAgenticRecord(t *testing.T, f *agenticFixture, number int, modified time.Time) string {
	t.Helper()
	path := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", number)+".json")
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

func TestAgenticRetentionUsesModificationTimeForEveryRecordKind(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.a.options.stateTTL = 24 * time.Hour
	require.NoError(t, f.a.prepareStore())
	for i, kind := range []string{"old-observation-recent-write", "active", "frozen", "inactive", "departure-tombstone", "pending"} {
		pr := f.gh.pr
		pr.Number = i + 1
		state := newAgenticState("org", "repo", &pr, f.now)
		state.RevisionPending = false
		r := &agenticRecord{State: state}
		switch kind {
		case "old-observation-recent-write":
			state.ObservedAt = f.now.Add(-365 * 24 * time.Hour)
		case "frozen":
			state.Plan, state.Frozen = &agenticSelection{Source: "chai", Jobs: []agenticJob{}}, true
		case "inactive", "departure-tombstone":
			state.Inactive = true
		case "pending":
			state.PendingDispatch = true
			r.Desired = &github.CheckRun{Name: agenticGate, HeadSHA: state.HeadSHA, ExternalID: agenticExternalID("org", "repo", pr.Number), Status: "in_progress"}
			r.Wakeup = &agenticSavedWakeup{At: f.now.Add(time.Hour), Backoff: time.Minute}
		}
		if kind != "departure-tombstone" {
			r.Gate = github.CheckRun{ID: int64(pr.Number), Name: agenticGate, HeadSHA: state.HeadSHA, ExternalID: agenticExternalID("org", "repo", pr.Number)}
		}
		require.NoError(t, f.a.writeRecord(context.Background(), r))
		modified := f.now.Add(-f.a.options.stateTTL)
		if i == 0 {
			modified = f.now.Add(-time.Hour)
		}
		ageAgenticRecord(t, f, pr.Number, modified)
	}
	before := *f.gh
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.Equal(t, before, *f.gh, "expiry accessed GitHub")
	require.Zero(t, f.jobs.creates)
	require.Len(t, f.a.store.entries, 1)
	retained, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 1})
	require.NoError(t, err)
	require.NotNil(t, retained, "old observation overrode the recent file modification")
	// Writing the same state is activity too: the atomic replacement supplies
	// the filesystem's current mtime rather than retaining its old observation.
	ageAgenticRecord(t, f, 1, f.now.Add(-2*f.a.options.stateTTL))
	require.NoError(t, f.a.writeRecord(context.Background(), retained))
	info, err := os.Stat(filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", 1)+".json"))
	require.NoError(t, err)
	f.now = info.ModTime().Add(f.a.options.stateTTL - time.Nanosecond)
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.Len(t, f.a.store.entries, 1, "a successful write did not refresh retention")
}

func TestAgenticRetentionStartupExpiresBeforeRestoringActions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.a.options.stateTTL = time.Hour
		f.reconcile(t, nil)
		_, state := f.gate(t)
		state.RevisionPending = true
		state.record.Desired = &state.record.Gate
		state.record.Wakeup = &agenticSavedWakeup{At: f.now.Add(-time.Minute), Backoff: time.Minute, Comment: f.command(500, "remaining")}
		require.NoError(t, f.a.writeRecord(context.Background(), state.record))
		path := ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
		require.NoError(t, f.a.closeStore())
		before := *f.gh
		stop := startAgenticRunner(f)
		require.Equal(t, before, *f.gh, "startup restored expired actions")
		require.Empty(t, f.a.store.entries)
		require.Empty(t, f.a.scheduler.pending)
		require.NotNil(t, f.a.maintenance)
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err))
		assertAgenticIdle(t, f, 3*time.Hour)
		stop()
		require.Nil(t, f.a.maintenance, "shutdown retained the maintenance timer")
	})
}

func TestAgenticRetentionDisabledModesLeaveStorageUntouched(t *testing.T) {
	for _, mode := range []string{"default-off", "normal-only", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newScheduledAgenticFixture(t, "manual")
				f.reconcile(t, nil)
				modified := f.now.Add(-365 * 24 * time.Hour)
				path := ageAgenticRecord(t, f, 42, modified)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, f.a.closeStore())
				if mode != "default-off" {
					f.a.options.stateTTL = time.Hour
				}
				if mode == "normal-only" {
					f.a.watcher.config.Orgs[0].Repos[0].Mode.Agentic = AgenticConfig{}
				}
				f.a.dryRun = mode == "dry-run"
				before := *f.gh
				stop := startAgenticRunner(f)
				require.Nil(t, f.a.maintenance)
				if mode != "default-off" {
					require.Nil(t, f.a.store)
				}
				assertAgenticIdle(t, f, 2*time.Hour)
				stop()
				require.Equal(t, before, *f.gh)
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, data, after)
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.True(t, info.ModTime().Equal(modified))
				_, err = os.Stat(filepath.Join(f.a.options.stateDir, agenticFreshPlanMarker))
				require.True(t, os.IsNotExist(err))
			})
		})
	}
}

func TestAgenticRetentionMaintenanceIsLocalAndStopsObsoleteRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.a.options.stateTTL = time.Hour
		stop := startAgenticRunner(f)
		defer stop()
		f.reconcile(t, nil)
		_, state := f.gate(t)
		other := *state
		other.Number, other.record = 43, nil
		require.NoError(t, f.a.writeRecord(context.Background(), &agenticRecord{State: &other}))
		ageAgenticRecord(t, f, 42, f.now)
		ageAgenticRecord(t, f, 43, f.now.Add(2*time.Hour))
		work := agenticWork{org: "org", repo: "repo", number: 42}
		f.a.mu.Lock()
		f.a.installWakeup(f.a.scheduler, work, &agenticWakeup{comment: f.command(500, "remaining")}, f.now.Add(2*time.Hour))
		f.a.mu.Unlock()
		before := *f.gh
		advanceAgenticTime(f, time.Hour)
		require.Equal(t, before, *f.gh)
		require.NotContains(t, f.a.scheduler.pending, work)
		key := agenticWork{org: "org", repo: "repo", sha: state.HeadSHA}
		require.Contains(t, f.a.statusContexts, key, "expiring one PR hid another PR's SHA interests")
		advanceAgenticTime(f, 2*time.Hour)
		require.Equal(t, before, *f.gh, "a canceled retry polled GitHub")
		require.Empty(t, f.a.store.entries)
		require.NotContains(t, f.a.statusContexts, key)
	})
}

func TestAgenticRetentionEventCannotReuseExpiredSelectionOrGreenGate(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	gate, original := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
	f.a.options.stateTTL = time.Hour
	ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
	f.now = f.now.Add(time.Minute)
	f.reconcile(t, nil) // No maintenance sweep has run.
	current, state := f.gate(t)
	require.Equal(t, gate.ID, current.ID)
	require.NotEqual(t, "success", current.Conclusion)
	require.NotEqual(t, original.RevisionID, state.RevisionID)
	require.Nil(t, state.Plan)
	require.Nil(t, state.Dispatch)
	require.False(t, state.Frozen)
	require.Zero(t, state.ManualRequestID)
	require.Equal(t, 1, f.jobs.creates)
	f.plan(t, "job-b")
	f.reconcile(t, f.command(600, "required"))
	_, state = f.gate(t)
	require.Equal(t, 600, state.ManualRequestID)
	require.Equal(t, "job-b", state.Plan.Jobs[0].Name)
	// The fresh selection is allowed to execute again after retention expiry.
	require.Equal(t, 2, f.jobs.creates)
}

func TestAgenticRetentionMissingGateStillRequiresFreshPlan(t *testing.T) {
	for _, phase := range []string{"first-observation", "after-expiry-ttl-disabled"} {
		t.Run(phase, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.a.options.stateTTL = time.Hour
			f.reconcile(t, nil)
			if phase == "after-expiry-ttl-disabled" {
				ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
				require.NoError(t, f.a.expireRecordsLocked(context.Background()))
				require.Empty(t, f.a.store.entries)
				require.NoError(t, f.a.closeStore())
				f.a.options.stateTTL, f.gh.checks = 0, nil
				f.now = f.now.Add(time.Minute)
				f.reconcile(t, nil)
			}
			_, state := f.gate(t)
			require.Nil(t, state.Plan, "missing state and gate accepted a pre-tracking plan")
			require.Zero(t, f.jobs.creates)
			f.plan(t, "job-a")
			f.reconcile(t, nil)
			require.Equal(t, 1, f.jobs.creates)
		})
	}
}

func TestAgenticRetentionPreservesCorruptionAndDurabilityFaults(t *testing.T) {
	for _, fault := range []string{"corruption", "marker-sync", "deletion-sync"} {
		t.Run(fault, func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			if fault != "marker-sync" {
				f.a.options.stateTTL = time.Hour
			}
			f.reconcile(t, nil)
			path := ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
			f.a.options.stateTTL = time.Hour
			if fault == "corruption" {
				require.NoError(t, os.WriteFile(path, []byte("invalid"), 0600))
				ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
			} else {
				require.NoError(t, f.a.store.directory.Close())
			}
			require.Error(t, f.a.expireRecordsLocked(context.Background()))
			require.Len(t, f.a.store.entries, 1, "uncertain deletion removed routing metadata")
			if fault == "deletion-sync" {
				require.Error(t, f.a.writeRecord(context.Background(), &agenticRecord{}), "directory fault did not latch")
			} else {
				_, err := os.Stat(path)
				require.NoError(t, err, "cleanup removed unvalidated or unguarded state")
			}
			_ = f.a.closeStore() // The fault-injected directory handle is already closed.
		})
	}
}

func TestAgenticRetentionRejectsInvalidFreshPlanMarker(t *testing.T) {
	for _, invalid := range []string{"nonempty", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, agenticFreshPlanMarker)
			if invalid == "symlink" {
				require.NoError(t, os.Symlink(t.TempDir(), path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("invalid"), 0600))
			}
			_, err := openAgenticStore(dir)
			require.ErrorContains(t, err, "fresh-plan marker")
		})
	}
}

func TestAgenticRetentionDoesNotRescheduleARecordExpiredDuringRetryPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.a.options.stateTTL = time.Hour
		stop := startAgenticRunner(f)
		defer stop()
		f.reconcile(t, nil)
		ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
		f.a.retryReconciliation("org", "repo", 42, f.command(500, "remaining"), io.ErrUnexpectedEOF)
		require.Empty(t, f.a.scheduler.pending, "saving a retry revived expired work")
		assertAgenticIdle(t, f, time.Hour)
	})
}
