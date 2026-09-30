package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const agenticLegacyImportRetired = ".legacy-import-retired"

func (s *agenticStore) loadLegacyImportRetirement() error {
	info, err := os.Lstat(filepath.Join(s.dir, agenticLegacyImportRetired))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return fmt.Errorf("invalid agentic legacy-import retirement marker")
	}
	s.legacyImportRetired = true
	return nil
}

// This constant-size marker outlives expired departure records. Missing local
// state can no longer import stale manual authorization from GitHub, including
// after a later restart with TTL disabled. Caller holds mu and the store flock.
func (s *agenticStore) retireLegacyImport() error {
	if s.fault != nil {
		return s.fault
	}
	if s.legacyImportRetired {
		return nil
	}
	file, err := os.OpenFile(filepath.Join(s.dir, agenticLegacyImportRetired), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		s.fault = fmt.Errorf("syncing agentic legacy-import retirement: %w", errors.Join(syncErr, closeErr))
		return s.fault
	}
	if err := s.directory.Sync(); err != nil {
		s.fault = fmt.Errorf("syncing agentic legacy-import retirement directory: %w", err)
		return s.fault
	}
	s.legacyImportRetired = true
	return nil
}

// Fresh records need metadata only. Decode expired candidates before deleting:
// corruption remains a fail-stop condition, never a reason to discard a file.
func (a *agenticController) expireRecordLocked(ctx context.Context, name string, now time.Time) (bool, error) {
	if a.options.stateTTL <= 0 || a.dryRun || a.store == nil || !a.hasAgenticEnrollment() {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := a.store.recordInfo(name)
	if os.IsNotExist(err) {
		if _, known := a.store.entries[name]; !known {
			return false, nil
		}
		return false, fmt.Errorf("tracked agentic record disappeared: %w", err)
	}
	if err != nil {
		return false, err
	}
	if now.Sub(info.ModTime()) < a.options.stateTTL {
		return false, nil
	}
	if err := a.store.retireLegacyImport(); err != nil {
		return false, err
	}
	r, err := a.store.read(name)
	if err != nil {
		return false, err
	}
	if err := a.deleteStoredRecord(ctx, name, r); err != nil {
		return false, err
	}
	return true, nil
}

func (a *agenticController) expireWorkLocked(ctx context.Context, work agenticWork) (bool, error) {
	if work.number == 0 {
		return false, nil
	}
	return a.expireRecordLocked(ctx, agenticRecordName(work.org, work.repo, work.number)+".json", a.currentTime())
}

// Iterate the routing index without retaining record payloads or a directory
// snapshot. Even large stores require at most one decoded candidate at a time.
func (a *agenticController) expireRecordsLocked(ctx context.Context) error {
	if a.options.stateTTL <= 0 || a.dryRun || a.store == nil {
		return nil
	}
	now := a.currentTime()
	for name := range a.store.entries {
		if _, err := a.expireRecordLocked(ctx, name, now); err != nil {
			return err
		}
	}
	return nil
}

// Every authoritative read checks expiry too: an event between maintenance
// sweeps must establish a fresh revision rather than reuse an expired plan.
func (a *agenticController) readStoredRecord(ctx context.Context, name string) (*agenticRecord, error) {
	if expired, err := a.expireRecordLocked(ctx, name, a.currentTime()); err != nil || expired {
		return nil, err
	}
	r, err := a.store.read(name)
	if os.IsNotExist(err) {
		if _, known := a.store.entries[name]; !known {
			return nil, nil
		}
		return nil, fmt.Errorf("tracked agentic record disappeared: %w", err)
	}
	return r, err
}

// One maintenance timer serves the entire controller, using only local file
// metadata. Normal-only and dry-run controllers never open or clean the store.
func (a *agenticController) startStateMaintenanceLocked() {
	s := a.scheduler
	if a.maintenance != nil || a.store == nil || a.options.stateTTL <= 0 || a.dryRun || !a.hasAgenticEnrollment() || s == nil || s.ctx.Err() != nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(max(min(a.options.stateTTL, time.Hour), time.Minute), func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.maintenance != timer || a.scheduler != s || a.stopped || s.ctx.Err() != nil {
			return
		}
		a.maintenance = nil
		if err := a.expireRecordsLocked(s.ctx); err != nil {
			a.logger.WithError(err).Error("Cannot expire local agentic records")
		}
		a.startStateMaintenanceLocked()
	})
	a.maintenance = timer
}
