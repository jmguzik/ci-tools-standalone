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
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

const agenticRecordLimit = 192 * 1024

var agenticRecordFilename = regexp.MustCompile(`^pipeline-agentic-[0-9a-f]{32}\.json$`)

// One authoritative record holds each PR's recovery state.
// Desired is persisted before publishing the gate; Wakeup uses the existing
// scheduler rather than introducing a second recovery queue.
type agenticRecord struct {
	State   *agenticState       `json:"state"`
	Gate    github.CheckRun     `json:"gate"`
	Desired *github.CheckRun    `json:"desired,omitempty"`
	Reopen  bool                `json:"reopen,omitempty"`
	Wakeup  *agenticSavedWakeup `json:"wakeup,omitempty"`
}

type agenticSavedWakeup struct {
	At        time.Time            `json:"at"`
	Deadline  time.Time            `json:"deadline,omitempty"`
	Backoff   time.Duration        `json:"backoff,omitempty"`
	NotBefore time.Time            `json:"not_before,omitempty"`
	Comment   *github.IssueComment `json:"comment,omitempty"`
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
	routes           map[agenticWork]map[string]bool
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
	s := &agenticStore{dir: dir, entries: map[string]agenticWork{}, routes: map[agenticWork]map[string]bool{}}
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
	s.unindex(name)
	work := agenticWork{org: state.Org, repo: state.Repo, number: state.Number, sha: state.HeadSHA}
	s.entries[name] = work
	key := agenticWork{org: work.org, repo: work.repo, sha: work.sha}
	if s.routes[key] == nil {
		s.routes[key] = map[string]bool{}
	}
	s.routes[key][name] = true
}

func (s *agenticStore) unindex(name string) {
	if old, ok := s.entries[name]; ok {
		key := agenticWork{org: old.org, repo: old.repo, sha: old.sha}
		delete(s.routes[key], name)
		if len(s.routes[key]) == 0 {
			delete(s.routes, key)
		}
		delete(s.entries, name)
	}
}

func (s *agenticStore) recordNames(work ...agenticWork) []string {
	var names []string
	if len(work) == 0 {
		for name := range s.entries {
			names = append(names, name)
		}
	} else {
		key := agenticWork{org: work[0].org, repo: work[0].repo, sha: work[0].sha}
		for name := range s.routes[key] {
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
	if s == nil || s.Version != 1 || s.Org == "" || s.Repo == "" || s.Number <= 0 || s.HeadSHA == "" || s.BaseBranch == "" || s.RevisionID == "" || s.ObservedAt.IsZero() || name != agenticRecordName(s.Org, s.Repo, s.Number)+".json" {
		return nil, fmt.Errorf("invalid agentic record identity: %s", name)
	}
	if err := validateAgenticState(s); err != nil {
		return nil, fmt.Errorf("invalid agentic record %s: %w", name, err)
	}
	for _, gate := range []*github.CheckRun{&r.Gate, r.Desired} {
		if gate != nil && (gate.ID < 0 || ((gate.Name != "" || gate.ID != 0) && (gate.Name != agenticGate || gate.HeadSHA != s.HeadSHA || gate.ExternalID != agenticExternalID(s.Org, s.Repo, s.Number)))) {
			return nil, fmt.Errorf("invalid gate identity in %s", name)
		}
	}
	if r.Wakeup != nil && (r.Wakeup.At.IsZero() || r.Wakeup.Backoff < 0 || r.Wakeup.NotBefore.After(r.Wakeup.At)) {
		return nil, fmt.Errorf("invalid wakeup in %s", name)
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
	previous, known := s.entries[name]
	s.index(name, r.State)
	if known {
		key := agenticWork{org: previous.org, repo: previous.repo, sha: previous.sha}
		if len(s.routes[key]) == 0 {
			delete(a.statusContexts, key)
		}
	}
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
	a.store.unindex(name)
	work := agenticWork{org: r.State.Org, repo: r.State.Repo, number: r.State.Number}
	if a.scheduler != nil {
		if pending := a.scheduler.pending[work]; pending != nil {
			pending.timer.Stop()
			delete(a.scheduler.pending, work)
		}
	}
	key := agenticWork{org: work.org, repo: work.repo, sha: r.State.HeadSHA}
	if len(a.store.routes[key]) == 0 {
		delete(a.statusContexts, key)
	}
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
		old.Gate, old.Desired, old.Wakeup, old.Reopen = github.CheckRun{}, nil, nil, false
		if err := a.writeRecord(ctx, old); err != nil {
			return err
		}
		work := agenticWork{org: state.Org, repo: state.Repo, number: old.State.Number}
		if a.scheduler != nil && a.scheduler.pending[work] != nil {
			a.scheduler.pending[work].timer.Stop()
			delete(a.scheduler.pending, work)
		}
	}
	return nil
}
