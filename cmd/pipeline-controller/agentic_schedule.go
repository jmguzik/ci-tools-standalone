package main

import (
	"context"
	"errors"
	"reflect"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

// number == 0 routes a status event's SHA through local records, never a
// repository-wide GitHub scan.
type agenticWork struct {
	org, repo string
	number    int
	sha       string
}

type agenticWakeup struct {
	timer     *time.Timer
	deadline  time.Time
	backoff   time.Duration
	notBefore time.Time
	comment   *github.IssueComment
}

type agenticScheduler struct {
	ctx     context.Context
	pending map[agenticWork]*agenticWakeup
}

// An invalid plan waits for another comment or the existing Chai deadline;
// repeatedly fetching the same invalid input would not repair it.
type agenticPlanPendingError struct{ error }

func (a *agenticController) startScheduling(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scheduler = &agenticScheduler{ctx: ctx, pending: map[agenticWork]*agenticWakeup{}}
	a.startStateMaintenanceLocked()
}

func (a *agenticController) reconcile(ctx context.Context, org, repo string, number int, comment *github.IssueComment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reconcileWork(ctx, agenticWork{org: org, repo: repo, number: number}, comment, 0)
}

// Caller holds mu, including timer callbacks. Failed operations retry only
// their own PR/SHA with capped backoff; idle work has no timer.
func (a *agenticController) reconcileWork(ctx context.Context, work agenticWork, comment *github.IssueComment, backoff time.Duration) (err error) {
	if a.stopped {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := a.expireWorkLocked(ctx, work); err != nil {
		return err
	}
	if a.scheduler != nil {
		if previous := a.scheduler.pending[work]; previous != nil {
			if previous.comment != nil {
				comment = previous.comment
			}
			if previous.notBefore.After(a.currentTime()) {
				if previous.comment == nil {
					previous.comment = comment
				}
				return nil // A new event must not shorten this work's server cooldown.
			}
			backoff = previous.backoff
		}
	}
	var deadline time.Time
	defer func() { a.scheduleWakeup(work, deadline, comment, backoff, err) }()
	if work.number != 0 {
		return a.reconcilePull(ctx, work.org, work.repo, work.number, comment, &deadline)
	}
	if work.sha == "" || !a.hasRepo(work.org, work.repo) {
		return nil
	}
	records, err := a.listRecords(ctx, work)
	if err != nil {
		return err
	}
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.State.Org != work.org || r.State.Repo != work.repo || r.State.HeadSHA != work.sha {
			continue
		}
		if r.State.Inactive && r.Desired == nil && r.Wakeup == nil {
			continue // Statuses must not wake ordinary-branch departure records.
		}
		pull := agenticWork{org: work.org, repo: work.repo, number: r.State.Number}
		if err := a.reconcileWork(ctx, pull, nil, 0); err != nil {
			a.logger.WithError(err).WithField("pr", r.State.Number).Error("Agentic recovery failed")
		}
	}
	return nil
}

func (a *agenticController) retryReconciliation(org, repo string, number int, comment *github.IssueComment, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scheduleWakeup(agenticWork{org: org, repo: repo, number: number}, time.Time{}, comment, 0, err)
}

// Issue-comment routing reads the PR before reconciliation. Preserve a failed
// command and respect its work item's cooldown before repeating that read.
func (a *agenticController) deferCommentRouting(org, repo string, number int, comment *github.IssueComment) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return true
	}
	if _, err := a.expireWorkLocked(context.Background(), agenticWork{org: org, repo: repo, number: number}); err != nil {
		a.logger.WithError(err).Warn("Cannot expire local agentic record before comment routing")
		return true
	}
	if a.scheduler != nil {
		if pending := a.scheduler.pending[agenticWork{org: org, repo: repo, number: number}]; pending != nil && pending.notBefore.After(a.currentTime()) {
			if pending.comment == nil {
				pending.comment = comment
			}
			return true
		}
	}
	return false
}

func (a *agenticController) scheduleWakeup(work agenticWork, deadline time.Time, comment *github.IssueComment, backoff time.Duration, err error) {
	s := a.scheduler
	if s == nil || s.ctx.Err() != nil {
		return
	}
	retry := agenticRetryFor(err)
	var cooldown time.Time
	if previous := s.pending[work]; previous != nil {
		previous.timer.Stop()
		// An API error before reading the record must not lose a known deadline.
		if retry.transient && deadline.IsZero() {
			deadline = previous.deadline
		}
		if retry.transient {
			backoff = previous.backoff
			cooldown = previous.notBefore
			if previous.comment != nil {
				comment = previous.comment
			}
		}
		delete(s.pending, work)
	}
	now := a.currentTime()
	var at time.Time
	next := &agenticWakeup{deadline: deadline, notBefore: cooldown}
	var invalidPlan agenticPlanPendingError
	if retry.transient {
		next.backoff, next.comment = nextAgenticBackoff(backoff), comment
		next.backoff = max(next.backoff, min(retry.after, agenticMaxBackoff))
		at = now.Add(max(next.backoff, retry.after))
		// A future Chai deadline can wake ordinary failed work sooner, but an
		// expired one must not cause a tight loop during an outage.
		if deadline.After(now) && deadline.Before(at) {
			at = deadline
		}
		if retry.after > 0 {
			if suggested := now.Add(retry.after); suggested.After(next.notBefore) {
				next.notBefore = suggested
			}
		}
		if at.Before(next.notBefore) {
			at = next.notBefore
		}
	} else if err == nil || errors.As(err, &invalidPlan) {
		// Chai gets one deadline, not recurring reads of an unchanged plan.
		at = deadline
	} else {
		a.logger.WithError(err).WithFields(map[string]interface{}{"org": work.org, "repo": work.repo, "pr": work.number}).Warn("Agentic recovery awaits a relevant event")
	}
	if at.IsZero() {
		a.persistWakeup(s.ctx, work, nil)
		return
	}
	if a.persistWakeup(s.ctx, work, &agenticSavedWakeup{At: at, Deadline: deadline, Backoff: next.backoff, NotBefore: next.notBefore, Comment: next.comment}) {
		return // A slow reconciliation's record expired before saving its retry.
	}
	a.installWakeup(s, work, next, at)
}

func (a *agenticController) installWakeup(s *agenticScheduler, work agenticWork, next *agenticWakeup, at time.Time) {
	s.pending[work] = next
	next.timer = time.AfterFunc(max(at.Sub(a.currentTime()), 0), func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		// A newer event, changed revision, or completed dispatch may have
		// canceled this callback even if its timer had already fired.
		if a.scheduler != s || s.pending[work] != next || s.ctx.Err() != nil {
			return
		}
		// A queued retry for an expired record must not revive its command or
		// read GitHub merely because its one-shot timer raced maintenance.
		if expired, err := a.expireWorkLocked(s.ctx, work); err != nil || expired {
			if err != nil {
				a.logger.WithError(err).Error("Cannot expire scheduled agentic record")
			}
			return
		}
		if err := a.reconcileWork(s.ctx, work, next.comment, next.backoff); err != nil {
			a.logger.WithError(err).WithField("pr", work.number).Error("Scheduled agentic reconciliation failed")
		}
	})
}

func (a *agenticController) persistWakeup(ctx context.Context, work agenticWork, wakeup *agenticSavedWakeup) (expired bool) {
	if a.dryRun || work.number == 0 {
		return false
	}
	known := false
	if a.store != nil {
		_, known = a.store.entries[agenticRecordName(work.org, work.repo, work.number)+".json"]
	}
	r, err := a.readRecord(ctx, work)
	if err == nil && r == nil && known {
		return true
	}
	if err == nil && r != nil && !reflect.DeepEqual(r.Wakeup, wakeup) {
		r.Wakeup = wakeup
		err = a.writeRecord(ctx, r)
	}
	if err != nil {
		a.logger.WithError(err).Warn("Cannot persist agentic wakeup; live retry remains scheduled")
	}
	return false
}

func (a *agenticController) restoreRecords(ctx context.Context) error {
	// Serialize restoration with events so a snapshot cannot overwrite a new
	// event's cooldown or revision. Persistent storage is required at start.
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.prepareStoreLocked(); err != nil {
		return err
	}
	if a.dryRun || a.store == nil {
		return nil
	}
	for _, name := range a.store.recordNames() {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := a.readStoredRecord(ctx, name)
		if err != nil {
			return err
		}
		if r == nil {
			continue
		}
		state := r.State
		a.rememberStatusContexts(state)
		if state.Inactive && r.Desired == nil && r.Wakeup == nil {
			continue
		}
		work := agenticWork{org: state.Org, repo: state.Repo, number: state.Number}
		if a.scheduler.pending[work] != nil {
			continue
		}
		unfinished := r.Desired != nil || state.PendingDispatch || state.RevisionPending || (state.Command != nil && !state.Command.Applied) || (state.Review != nil && !state.ReviewPosted)
		waiting := state.WaitingSince != nil && !state.Frozen && state.Dispatch == nil
		if saved := r.Wakeup; saved != nil && (saved.Backoff > 0 || !saved.NotBefore.IsZero() || (!unfinished && waiting)) {
			a.installWakeup(a.scheduler, work, &agenticWakeup{deadline: saved.Deadline, backoff: saved.Backoff, notBefore: saved.NotBefore, comment: saved.Comment}, saved.At)
			continue
		}
		if unfinished {
			a.installWakeup(a.scheduler, work, &agenticWakeup{}, a.currentTime())
		} else if waiting {
			a.installWakeup(a.scheduler, work, &agenticWakeup{deadline: state.WaitingSince.Add(a.options.timeout)}, state.WaitingSince.Add(a.options.timeout))
		}
	}
	return nil
}
