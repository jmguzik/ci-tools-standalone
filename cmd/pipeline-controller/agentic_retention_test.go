package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

func ageAgenticRecord(t *testing.T, f *agenticFixture, modified time.Time) string {
	t.Helper()
	path := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", f.gh.pr.Number)+".json")
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

func TestAgenticRetentionUsesLastModification(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.a.options.stateTTL = 24 * time.Hour
	f.reconcile(t, nil)
	_, state := f.gate(t)
	state.ObservedAt = f.now.Add(-365 * 24 * time.Hour)
	require.NoError(t, f.a.writeRecord(context.Background(), state.record))
	path := ageAgenticRecord(t, f, f.now.Add(-time.Hour))
	before := *f.gh
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.Len(t, f.a.store.entries, 1, "creation/observation time overrode recent modification")
	ageAgenticRecord(t, f, f.now.Add(-48*time.Hour))
	require.NoError(t, f.a.writeRecord(context.Background(), state.record))
	info, err := os.Stat(path)
	require.NoError(t, err)
	f.now = info.ModTime().Add(f.a.options.stateTTL - time.Nanosecond)
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.Len(t, f.a.store.entries, 1, "a successful write did not refresh retention")
	f.now = f.now.Add(time.Nanosecond)
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.Empty(t, f.a.store.entries)
	require.Equal(t, before, *f.gh, "retention accessed GitHub")
}

func TestAgenticRetentionExpiresBeforeStartupRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.a.options.stateTTL = time.Hour
		f.reconcile(t, nil)
		_, state := f.gate(t)
		state.record.Dirty, state.PendingDispatch = true, true
		require.NoError(t, f.a.writeRecord(context.Background(), state.record))
		path := ageAgenticRecord(t, f, f.now.Add(-time.Hour))
		require.NoError(t, f.a.closeStore())
		before := *f.gh
		stop := startAgenticRunner(f)
		require.Equal(t, before, *f.gh, "startup restored expired work")
		require.Empty(t, f.a.store.entries)
		require.NotNil(t, f.a.maintenance)
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err))
		assertAgenticIdle(t, f, 3*time.Hour)
		stop()
		require.Nil(t, f.a.maintenance)
	})
}

func TestAgenticRetentionMaintenanceAndStaleQueueItemsAreLocal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newScheduledAgenticFixture(t, "manual")
		f.a.options.stateTTL = time.Hour
		stop := startAgenticRunner(f)
		defer stop()
		f.reconcile(t, nil)
		ageAgenticRecord(t, f, f.now)
		f.a.queue.AddAfter(agenticWork{org: "org", repo: "repo", number: 42}, 2*time.Hour)
		assertAgenticIdle(t, f, 3*time.Hour)
		require.Empty(t, f.a.store.entries)
	})
}

func TestAgenticRetentionRequiresFreshDecisionInsteadOfReusingGreenGate(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	f.report(t, v1.SuccessState)
	f.reconcile(t, nil)
	gate, original := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
	f.a.options.stateTTL = time.Hour
	ageAgenticRecord(t, f, f.now.Add(-time.Hour))
	f.now = f.now.Add(time.Minute)
	f.reconcile(t, nil)
	current, state := f.gate(t)
	require.Equal(t, gate.ID, current.ID)
	require.NotEqual(t, "success", current.Conclusion)
	require.NotEqual(t, original.RevisionID, state.RevisionID)
	require.Nil(t, state.Plan)
	require.Zero(t, state.ManualRequestID)
	require.Equal(t, 1, f.jobs.creates)
	// The freshness marker survives expiry, a missing check and TTL disablement.
	ageAgenticRecord(t, f, f.now.Add(-time.Hour))
	require.NoError(t, f.a.expireRecordsLocked(context.Background()))
	require.NoError(t, f.a.closeStore())
	f.a.options.stateTTL, f.gh.checks = 0, nil
	f.reconcile(t, nil)
	_, state = f.gate(t)
	require.Nil(t, state.Plan)
	f.plan(t, "job-b")
	f.reconcile(t, f.command(600, "remaining"))
	require.Equal(t, 2, f.jobs.creates)
}

func TestAgenticRetentionPreservesCorruption(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.a.options.stateTTL = time.Hour
	f.reconcile(t, nil)
	path := ageAgenticRecord(t, f, f.now.Add(-time.Hour))
	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0600))
	ageAgenticRecord(t, f, f.now.Add(-time.Hour))
	require.Error(t, f.a.expireRecordsLocked(context.Background()))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "invalid", string(data))
}
