package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"sigs.k8s.io/prow/pkg/github"
)

const agenticRecordLimit = 192 * 1024
const agenticStateVersion = 2

var agenticRecordFilename = regexp.MustCompile(`^pipeline-agentic-[0-9a-f]{32}\.json$`)

// Keep intent and the last published gate identity, not GitHub projections or
// retry timers. Job results come from ProwJobs, with compact success witnesses
// retained for jobs Sinker has already removed.
type agenticRecord struct {
	State  *agenticState       `json:"state"`
	Gate   agenticGateSnapshot `json:"gate"`
	Dirty  bool                `json:"dirty,omitempty"`
	Reopen bool                `json:"reopen,omitempty"`
}

type agenticGateSnapshot struct {
	ID         int64  `json:"id,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

func snapshotAgenticGate(gate github.CheckRun) agenticGateSnapshot {
	return agenticGateSnapshot{ID: gate.ID, HeadSHA: gate.HeadSHA, ExternalID: gate.ExternalID,
		Status: gate.Status, Conclusion: gate.Conclusion, Summary: gate.Output.Summary}
}

func (g agenticGateSnapshot) checkRun(appID int64) github.CheckRun {
	gate := github.CheckRun{ID: g.ID, HeadSHA: g.HeadSHA, ExternalID: g.ExternalID, Status: g.Status, Conclusion: g.Conclusion,
		Output: github.CheckRunOutput{Title: "Pipeline", Summary: g.Summary}}
	if g.ID != 0 {
		gate.Name, gate.App.ID = agenticGate, appID
	}
	return gate
}

func agenticRecordName(org, repo string, number int) string {
	return "pipeline-agentic-" + agenticID(org, repo, strconv.Itoa(number))
}

// Only routing metadata is retained in memory. Every transition reads its
// authoritative record from disk while the controller holds mu and the lock.
type agenticStore struct {
	dir              string
	lock             *os.File
	directory        *os.File
	entries          map[string]agenticWork
	fault            error // A failed directory sync leaves durability uncertain until restart.
	requireFreshPlan bool
}

func openAgenticStore(dir string) (_ *agenticStore, err error) {
	if dir == "" {
		return nil, fmt.Errorf("agentic mode requires --agentic-state-dir")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &agenticStore{dir: dir, entries: map[string]agenticWork{}}
	defer func() {
		if err != nil {
			_ = s.close()
		}
	}()
	s.directory, err = os.Open(dir)
	if err != nil {
		return nil, err
	}
	s.lock, err = os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("locking agentic state directory: %w", err)
	}
	if err = s.loadFreshPlanRequirement(); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		name := file.Name()
		if !strings.HasPrefix(name, "pipeline-agentic-") {
			continue // Includes abandoned .agentic-*.tmp files and the lock.
		}
		r, err := s.read(name)
		if err != nil {
			return nil, err
		}
		s.index(name, r.State)
	}
	return s, nil
}

func (s *agenticStore) close() error {
	var err error
	if s.lock != nil {
		err = s.lock.Close() // Closing releases flock, including failed lock attempts.
		s.lock = nil
	}
	if s.directory != nil {
		if closeErr := s.directory.Close(); err == nil {
			err = closeErr
		}
		s.directory = nil
	}
	return err
}

func (s *agenticStore) index(name string, state *agenticState) {
	s.entries[name] = agenticWork{org: state.Org, repo: state.Repo, number: state.Number, sha: state.HeadSHA}
}

func (s *agenticStore) recordNames(work ...agenticWork) []string {
	var names []string
	for name, entry := range s.entries {
		if len(work) == 0 || (entry.org == work[0].org && entry.repo == work[0].repo && entry.sha == work[0].sha) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *agenticStore) recordInfo(name string) (os.FileInfo, error) {
	if s.fault != nil {
		return nil, s.fault
	}
	if !agenticRecordFilename.MatchString(name) {
		return nil, fmt.Errorf("invalid agentic record filename %q", name)
	}
	info, err := os.Lstat(filepath.Join(s.dir, name))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > agenticRecordLimit {
		return nil, fmt.Errorf("invalid agentic record file %s", name)
	}
	return info, nil
}

func (s *agenticStore) read(name string) (*agenticRecord, error) {
	if _, err := s.recordInfo(name); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, agenticRecordLimit+1))
	if err != nil {
		return nil, err
	}
	return decodeAgenticRecord(name, data)
}

func decodeAgenticRecord(name string, data []byte) (*agenticRecord, error) {
	var r agenticRecord
	if len(data) > agenticRecordLimit {
		return nil, fmt.Errorf("agentic state exceeds the 192 KiB record limit")
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("invalid agentic record %s: %w", name, err)
	}
	s := r.State
	if s == nil || s.Version != agenticStateVersion || s.Org == "" || s.Repo == "" || s.Number <= 0 || s.HeadSHA == "" || s.BaseBranch == "" || s.RevisionID == "" || s.ObservedAt.IsZero() || name != agenticRecordName(s.Org, s.Repo, s.Number)+".json" {
		return nil, fmt.Errorf("invalid agentic record identity: %s", name)
	}
	if err := validateAgenticState(s); err != nil {
		return nil, fmt.Errorf("invalid agentic record %s: %w", name, err)
	}
	if r.Gate.ID < 0 || (r.Gate.ID != 0 && (r.Gate.HeadSHA != s.HeadSHA || r.Gate.ExternalID != agenticExternalID(s.Org, s.Repo, s.Number))) {
		return nil, fmt.Errorf("invalid gate identity in %s", name)
	}
	s.record = &r
	return &r, nil
}

// Normal-only and dry-run operation never opens the directory.
func (a *agenticController) prepareStore() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prepareStoreLocked()
}

func (a *agenticController) prepareStoreLocked() error {
	if a.stopped {
		return fmt.Errorf("agentic controller has stopped")
	}
	if a.dryRun || a.store != nil || !a.hasAgenticEnrollment() {
		return nil
	}
	store, err := openAgenticStore(a.options.stateDir)
	if err != nil {
		return err
	}
	a.store = store
	if a.options.stateTTL > 0 {
		if err = store.requireFreshPlans(); err == nil {
			err = a.expireRecordsLocked(context.Background())
		}
		if err != nil {
			_ = store.close()
			a.store = nil
			return err
		}
	}
	a.startStateMaintenanceLocked()
	return nil
}

func (a *agenticController) closeStore() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeStoreLocked()
}

func (a *agenticController) closeStoreLocked() error {
	if a.maintenance != nil {
		a.maintenance.Stop()
		a.maintenance = nil
	}
	if a.store == nil {
		return nil
	}
	err := a.store.close()
	a.store = nil
	return err
}

func (a *agenticController) readRecord(ctx context.Context, work agenticWork) (*agenticRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.prepareStoreLocked(); err != nil {
		return nil, err
	}
	if a.dryRun || a.store == nil {
		return nil, nil
	}
	name := agenticRecordName(work.org, work.repo, work.number) + ".json"
	r, err := a.readStoredRecord(ctx, name)
	if err == nil && r != nil && (r.State.Org != work.org || r.State.Repo != work.repo || r.State.Number != work.number) {
		err = fmt.Errorf("agentic record identity does not match its PR")
	}
	return r, err
}

func (a *agenticController) writeRecord(ctx context.Context, r *agenticRecord) error {
	if a.dryRun {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.prepareStoreLocked(); err != nil {
		return err
	}
	s := a.store
	if s == nil {
		return fmt.Errorf("agentic storage is not initialized")
	}
	if s.fault != nil {
		return s.fault
	}
	if r == nil || r.State == nil {
		return fmt.Errorf("agentic record has no state")
	}
	name := agenticRecordName(r.State.Org, r.State.Repo, r.State.Number) + ".json"
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := decodeAgenticRecord(name, data); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".agentic-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(s.dir, name)); err != nil {
		return err
	}
	if err := s.directory.Sync(); err != nil {
		s.fault = fmt.Errorf("syncing agentic state directory: %w", err)
		return s.fault
	}
	s.index(name, r.State)
	return nil
}

func (a *agenticController) listRecords(ctx context.Context, work ...agenticWork) ([]*agenticRecord, error) {
	if err := a.prepareStoreLocked(); err != nil {
		return nil, err
	}
	if a.dryRun || a.store == nil {
		return nil, nil
	}
	var records []*agenticRecord
	for _, name := range a.store.recordNames(work...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := a.readStoredRecord(ctx, name)
		if err != nil {
			return nil, err // Preserve malformed records and block their routing subset.
		}
		if r == nil {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

func (a *agenticController) deleteRecord(ctx context.Context, org, repo string, number int) error {
	if a.dryRun {
		return nil
	}
	r, err := a.readRecord(ctx, agenticWork{org: org, repo: repo, number: number})
	if err != nil || r == nil {
		return err
	}
	name := agenticRecordName(org, repo, number) + ".json"
	return a.deleteStoredRecord(ctx, name, r)
}

func (a *agenticController) deleteStoredRecord(ctx context.Context, name string, r *agenticRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(a.store.dir, name)); err != nil {
		return err
	}
	if err := a.store.directory.Sync(); err != nil {
		a.store.fault = fmt.Errorf("syncing agentic state deletion: %w", err)
		return a.store.fault
	}
	delete(a.store.entries, name)
	work := agenticWork{org: r.State.Org, repo: r.State.Repo, number: r.State.Number}
	a.queueMu.Lock()
	delete(a.inputs, work)
	delete(a.retryAt, work)
	a.queueMu.Unlock()
	return nil
}

// A gate can change PR owners after the old PR closes. Retire its local record
// before publishing the transfer, including when the close webhook was missed.
func (a *agenticController) retirePreviousOwners(ctx context.Context, state *agenticState, gateID int64) error {
	records, err := a.listRecords(ctx, agenticWork{org: state.Org, repo: state.Repo, sha: state.HeadSHA})
	if err != nil {
		return err
	}
	for _, old := range records {
		if old.State.Number == state.Number || old.Gate.ID != gateID {
			continue
		}
		old.State.Inactive, old.State.PendingDispatch = true, false
		old.Gate, old.Dirty, old.Reopen = agenticGateSnapshot{}, false, false
		if err := a.writeRecord(ctx, old); err != nil {
			return err
		}
		work := agenticWork{org: state.Org, repo: state.Repo, number: old.State.Number}
		a.queueMu.Lock()
		delete(a.inputs, work)
		delete(a.retryAt, work)
		a.queueMu.Unlock()
	}
	return nil
}
