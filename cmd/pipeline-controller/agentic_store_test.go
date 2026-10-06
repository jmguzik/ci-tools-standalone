package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticStateWriteFailureBlocksSideEffects(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			if operation == "update" {
				f.reconcile(t, nil)
			}
			f.plan(t, "job-a")
			f.passFirstStage(t)
			writes := len(f.gh.checkWrites)
			require.NoError(t, f.a.prepareStore())
			// The rename succeeds but syncing its directory fails. Publication
			// must await confirmed durability, even for this uncertain write.
			require.NoError(t, f.a.store.directory.Close())
			require.Error(t, f.tryReconcile(nil))
			require.Zero(t, f.jobs.creates)
			require.Len(t, f.gh.checkWrites, writes, "gate publication preceded durable state")
			_ = f.a.closeStore()
			f.reconcile(t, nil)
			require.Equal(t, 1, f.jobs.creates)
		})
	}
}

func TestAgenticMissingStateDepartureWriteFailureBlocksReentry(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.gh.pr.Base.Ref = "release"
	require.NoError(t, f.a.prepareStore())
	// Persisting a missing-state departure has the same durability boundary
	// as an active transition, even though it publishes no GitHub gate.
	require.NoError(t, f.a.store.directory.Close())
	require.Error(t, f.tryReconcile(nil))
	f.gh.pr.Base.Ref = "main"
	f.plan(t, "job-a")
	f.passFirstStage(t)
	require.Error(t, f.tryReconcile(nil), "uncertain departure durability allowed reentry")
	require.Empty(t, f.gh.checkWrites)
	require.Zero(t, f.jobs.creates)
	_ = f.a.closeStore() // The injected directory handle was already closed.
	f.reconcile(t, nil)
	_, state := f.gate(t)
	require.Nil(t, state.Plan, "restart lost the departure boundary")
	require.Zero(t, f.jobs.creates)
}

func TestAgenticLostCheckCreationResponseDoesNotDuplicateGate(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.gh.failCheckAfterCreate = true
	require.Error(t, f.tryReconcile(nil))
	require.Len(t, f.gh.checks, 1)
	require.Zero(t, f.jobs.creates)
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, f.gh.checks[0].ID, gate.ID)
	require.Equal(t, 1, f.jobs.creates)
}

func TestAgenticMalformedRecordPreservedAndRoutingIsScoped(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	healthySHA := f.gh.pr.Head.SHA
	f.gh.pr.Number, f.gh.pr.Head.SHA = 999, strings.Repeat("c", 40)
	f.reconcile(t, nil)
	name := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", 999)+".json")
	require.NoError(t, os.WriteFile(name, []byte("invalid"), 0600))
	records, err := f.a.listRecords(context.Background(), agenticWork{org: "org", repo: "repo", sha: healthySHA})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, 42, records[0].State.Number)
	_, err = f.a.listRecords(context.Background())
	require.Error(t, err)
	_, err = f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 999})
	require.Error(t, err)
	require.Error(t, f.a.deleteRecord(context.Background(), "org", "repo", 999))
	require.NoError(t, f.a.closeStore())
	require.Error(t, f.a.prepareStore(), "startup must reject malformed recovery state")
	preserved, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, "invalid", string(preserved))
}

func TestAgenticStoreExcludesAnotherWriterAndIgnoresTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".agentic-abandoned.tmp"), []byte("partial"), 0600))
	first, err := openAgenticStore(dir)
	require.NoError(t, err)
	_, err = openAgenticStore(dir)
	require.Error(t, err, "a second process must not read or write the store")
	require.NoError(t, first.close())
	second, err := openAgenticStore(dir)
	require.NoError(t, err)
	require.Empty(t, second.entries)
	require.NoError(t, second.close())
	data, err := os.ReadFile(filepath.Join(dir, ".agentic-abandoned.tmp"))
	require.NoError(t, err)
	require.Equal(t, "partial", string(data))
}

func TestAgenticStoreUpdatesHeadAndDeletionRouting(t *testing.T) {
	for _, cleanup := range []string{"delete", "ttl"} {
		t.Run(cleanup, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			f.reconcile(t, nil)
			for i, sha := range []string{strings.Repeat("c", 40), strings.Repeat("d", 40)} {
				old := agenticWork{org: "org", repo: "repo", sha: f.gh.pr.Head.SHA}
				if i == 0 {
					other := f.gh.pr
					other.Number = 43
					require.NoError(t, f.a.writeRecord(context.Background(), &agenticRecord{State: newAgenticState("org", "repo", &other, f.now)}))
				}
				f.gh.pr.Head.SHA = sha
				f.reconcile(t, nil)
				records, err := f.a.listRecords(context.Background(), old)
				require.NoError(t, err)
				if i == 0 {
					// The shared SHA stays routed until its remaining PR is deleted.
					require.Len(t, records, 1)
					require.Equal(t, 43, records[0].State.Number)
					require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 43))
					records, err = f.a.listRecords(context.Background(), old)
					require.NoError(t, err)
				}
				// After the head changes, the old SHA must no longer match.
				require.Empty(t, records)
				records, err = f.a.listRecords(context.Background(), agenticWork{org: "org", repo: "repo", sha: sha})
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Len(t, f.a.store.entries, 1)
			}
			if cleanup == "ttl" {
				f.a.options.stateTTL = time.Hour
				ageAgenticRecord(t, f, f.now.Add(-time.Hour))
				require.NoError(t, f.a.expireRecordsLocked(context.Background()))
			} else {
				other := f.gh.pr
				other.Number = 43
				require.NoError(t, f.a.writeRecord(context.Background(), &agenticRecord{State: newAgenticState("org", "repo", &other, f.now)}))
				gates := len(f.gh.checks)
				f.gh.pr.State = github.PullRequestStateClosed
				f.reconcile(t, nil)
				records, err := f.a.listRecords(context.Background())
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, 43, records[0].State.Number, "observed closure deleted another PR's record")
				require.Len(t, f.gh.checks, gates)
				require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 43))
			}
			require.Empty(t, f.a.store.entries)
		})
	}
}
