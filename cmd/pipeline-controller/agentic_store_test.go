package main

import (
	"context"
	"io"
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

func TestAgenticUnimportedDepartureWriteFailureBlocksReentry(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.gh.pr.Base.Ref = "release"
	require.NoError(t, f.a.prepareStore())
	// Persisting an unimported departure has the same durability boundary
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

func TestAgenticMalformedRecordPreservedAndRoutingIsIndexed(t *testing.T) {
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
			for _, sha := range []string{strings.Repeat("c", 40), strings.Repeat("d", 40)} {
				old := agenticWork{org: "org", repo: "repo", sha: f.gh.pr.Head.SHA}
				f.gh.pr.Head.SHA = sha
				f.reconcile(t, nil)
				records, err := f.a.listRecords(context.Background(), old)
				require.NoError(t, err)
				require.Empty(t, records)
				require.NotContains(t, f.a.statusContexts, old)
				records, err = f.a.listRecords(context.Background(), agenticWork{org: "org", repo: "repo", sha: sha})
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Len(t, f.a.statusContexts, 1)
			}
			if cleanup == "ttl" {
				f.a.options.stateTTL = time.Hour
				ageAgenticRecord(t, f, 42, f.now.Add(-time.Hour))
				require.NoError(t, f.a.expireRecordsLocked(context.Background()))
			} else {
				require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 42))
			}
			require.Empty(t, f.a.store.entries)
			require.Empty(t, f.a.store.routes)
			require.Empty(t, f.a.statusContexts)
		})
	}
}

func TestAgenticHeadChangePreservesOtherPRStatusInterests(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	old := agenticWork{org: "org", repo: "repo", sha: f.gh.pr.Head.SHA}
	other := f.gh.pr
	other.Number = 43
	require.NoError(t, f.a.writeRecord(context.Background(), &agenticRecord{State: newAgenticState("org", "repo", &other, f.now)}))
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.reconcile(t, nil)
	require.True(t, f.a.statusContexts[old]["ci/first"], "changing one PR hid another PR's reports")
	require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 43))
	require.NotContains(t, f.a.statusContexts, old)
	require.Len(t, f.a.statusContexts, 1)
	require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 42))
	require.Empty(t, f.a.statusContexts)
}

func TestAgenticUnchangedGatePersistsStateWithoutPublication(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	gate, state := f.gate(t)
	writes := len(f.gh.checkWrites)
	state.LastCommandID = 500
	require.NoError(t, f.a.saveState(&gate, state, gate.Conclusion, gate.Output.Summary))
	r, err := f.a.readRecord(context.Background(), agenticWork{org: "org", repo: "repo", number: 42})
	require.NoError(t, err)
	require.Equal(t, 500, r.State.LastCommandID)
	require.Nil(t, r.Desired, "unchanged publication left unfinished intent")
	require.False(t, r.Reopen)
	require.Len(t, f.gh.checkWrites, writes)
}

func TestAgenticMalformedRecordClosesOwnedSuccess(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	name := filepath.Join(f.a.options.stateDir, agenticRecordName("org", "repo", 42)+".json")
	require.NoError(t, os.WriteFile(name, []byte("invalid"), 0600))
	require.Error(t, f.tryReconcile(nil))
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	require.Zero(t, f.jobs.creates)
	preserved, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, "invalid", string(preserved))
}

func TestAgenticLegacyImportPreservesOnlyCurrentSelection(t *testing.T) {
	for _, marker := range []string{"matching", "new-command", "other-head", "inactive", "wrong-generation", "interrupted-import"} {
		t.Run(marker, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			f.reconcile(t, f.command(500, "remaining"))
			gate, original := f.gate(t)
			original.PlanCommentFloor, original.PlanNotBefore = 50, &original.ObservedAt
			revision := agenticRevision{HeadSHA: original.HeadSHA, BaseBranch: original.BaseBranch, ObservedAt: original.ObservedAt, RevisionID: original.RevisionID, CommentFloor: 500, PlanFloor: 50}
			// Include the old protocol fields, not only the replacement schema.
			legacy := struct {
				*agenticState
				RevisionUpdate *agenticRevision `json:"revision_update,omitempty"`
				Departure      *agenticRevision `json:"departure,omitempty"`
			}{agenticState: original, RevisionUpdate: &revision}
			metadata, err := agenticMetadata(agenticStateMarker, legacy)
			require.NoError(t, err)
			f.gh.checks[0].Output.Text = metadata
			switch marker {
			case "other-head", "interrupted-import":
				revision.HeadSHA = strings.Repeat("c", 40)
			case "inactive":
				revision.Inactive = true
			case "wrong-generation":
				revision.RevisionID = "previous-observed-visit"
			}
			body, err := agenticMetadata(agenticRevisionMarker, revision)
			require.NoError(t, err)
			f.gh.comments = append(f.gh.comments, github.IssueComment{ID: 501, Body: body, User: github.User{Login: "controller[bot]"}})
			if marker == "new-command" {
				f.command(502, "remaining")
			}
			require.NoError(t, f.a.deleteRecord(context.Background(), "org", "repo", 42))
			if marker == "interrupted-import" {
				f.gh.listCommentsError = io.ErrUnexpectedEOF
				require.Error(t, f.tryReconcile(nil))
				f.gh.listCommentsError = nil
			}
			f.reconcile(t, nil)
			recoveredGate, state := f.gate(t)
			require.Equal(t, gate.ID, recoveredGate.ID)
			require.Equal(t, 1, f.jobs.creates, "migration created another execution")
			require.NotContains(t, f.gh.checks[0].Output.Text, agenticStateMarker, "GitHub still stores recovery metadata")
			if marker == "matching" || marker == "new-command" {
				commandID := 500
				if marker == "new-command" {
					commandID = 502
				}
				require.True(t, state.Frozen)
				require.Equal(t, original.Dispatch, state.Dispatch)
				require.Equal(t, commandID, state.LastCommandID)
				require.NotNil(t, state.Command)
				require.Equal(t, commandID, state.Command.ID)
				require.True(t, state.Command.Applied)
				require.Equal(t, commandID, state.ManualRequestID)
				require.Equal(t, original.PlanCommentFloor, state.PlanCommentFloor)
				require.Equal(t, original.PlanNotBefore, state.PlanNotBefore)
			} else {
				require.False(t, state.Frozen)
				require.Nil(t, state.Plan)
				require.Nil(t, state.Dispatch)
				require.Zero(t, state.ManualRequestID)
				require.NotEqual(t, original.RevisionID, state.RevisionID)
			}
			// The imported record is authoritative after the one-time lookup.
			reads := f.gh.listCheckRunsCalls
			_, _, err = f.a.loadState("org", "repo", &f.gh.pr)
			require.NoError(t, err)
			require.Equal(t, reads, f.gh.listCheckRunsCalls)
		})
	}
}
