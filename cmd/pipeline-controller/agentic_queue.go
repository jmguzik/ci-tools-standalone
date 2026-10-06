package main

import (
	"context"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/prow/pkg/github"
)

// Queue keys are PRs. sha is used only by the local store's ownership index.
type agenticWork struct {
	org, repo string
	number    int
	sha       string
}

type agenticInput struct {
	generation uint64
	comments   []github.IssueComment
}

type agenticPlanPendingError struct{ error }

func (a *agenticController) startQueue(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queueMu.Lock()
	a.queueCtx = ctx
	a.queue = workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[agenticWork](5*time.Second, 5*time.Minute))
	for work := range a.inputs {
		a.queue.Add(work)
	}
	a.queueMu.Unlock()
	a.startStateMaintenanceLocked()
}

func (a *agenticController) enqueue(org, repo string, number int, comment *github.IssueComment) {
	if number <= 0 {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.queue != nil && a.queue.ShuttingDown() {
		return
	}
	work := agenticWork{org: org, repo: repo, number: number}
	if a.inputs == nil {
		a.inputs = map[agenticWork]*agenticInput{}
	}
	if a.inputs[work] == nil {
		a.inputs[work] = &agenticInput{}
	}
	input := a.inputs[work]
	input.generation++
	if comment != nil {
		duplicate := false
		for _, pending := range input.comments {
			duplicate = duplicate || pending.ID == comment.ID
		}
		if !duplicate {
			input.comments = append(input.comments, *comment)
		}
	}
	if a.queue != nil {
		a.queue.Add(work)
	}
}

func agenticUnfinished(r *agenticRecord) bool {
	s := r.State
	return r.Dirty || (!s.Inactive && (s.PendingDispatch || s.RevisionPending ||
		(s.Command != nil && !s.Command.Applied) || (s.Review != nil && !s.ReviewPosted && s.ActivatedAt != nil)))
}

// Caller holds queueMu. Concurrent events cannot shorten a server cooldown.
func (a *agenticController) rememberCooldownLocked(work agenticWork, after time.Duration) {
	if a.retryAt == nil {
		a.retryAt = map[agenticWork]time.Time{}
	}
	if at := a.currentTime().Add(after); at.After(a.retryAt[work]) {
		a.retryAt[work] = at
	}
}

func (a *agenticController) processNext(ctx context.Context) bool {
	q := a.queue
	work, shutdown := q.Get()
	if shutdown {
		return false
	}
	defer q.Done(work)
	a.queueMu.Lock()
	input := a.inputs[work]
	var generation uint64
	var comment *github.IssueComment
	if input != nil {
		generation = input.generation
		if len(input.comments) > 0 {
			copy := input.comments[0]
			comment = &copy
		}
	}
	if at := a.retryAt[work]; at.After(a.currentTime()) {
		q.AddAfter(work, at.Sub(a.currentTime()))
		a.queueMu.Unlock()
		return true
	}
	a.queueMu.Unlock()
	a.mu.Lock()
	var deadline time.Time
	var err error
	if a.stopped || ctx.Err() != nil {
		a.mu.Unlock()
		return false
	}
	// Delayed queue items cannot be cancelled. A stale deadline is a local
	// read only, not another GitHub reconciliation of an idle/completed PR.
	if input == nil {
		var r *agenticRecord
		r, err = a.readRecord(ctx, work)
		if err == nil && (r == nil || (!agenticUnfinished(r) && r.State.WaitingSince == nil)) {
			q.Forget(work)
			a.mu.Unlock()
			return true
		}
		if err == nil && !agenticUnfinished(r) && r.State.WaitingSince != nil {
			at := r.State.WaitingSince.Add(a.options.timeout)
			if at.After(a.currentTime()) {
				q.AddAfter(work, at.Sub(a.currentTime()))
				a.mu.Unlock()
				return true
			}
		}
	}
	if err == nil {
		err = a.reconcilePull(ctx, work.org, work.repo, work.number, comment, &deadline)
	}
	a.mu.Unlock()
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	retry := agenticRetryFor(err)
	if retry.transient {
		// Keep the event/command across live retries, but do not journal timers.
		if a.inputs == nil {
			a.inputs = map[agenticWork]*agenticInput{}
		}
		if a.inputs[work] == nil {
			a.inputs[work] = &agenticInput{}
		}
		if retry.after > 0 {
			a.rememberCooldownLocked(work, retry.after)
			q.AddAfter(work, a.retryAt[work].Sub(a.currentTime()))
		} else {
			q.AddRateLimited(work)
		}
		return true
	}
	if !a.retryAt[work].After(a.currentTime()) {
		delete(a.retryAt, work)
	}
	q.Forget(work)
	if current := a.inputs[work]; current != nil {
		if comment != nil && len(current.comments) > 0 && current.comments[0].ID == comment.ID {
			current.comments = current.comments[1:]
		}
		if len(current.comments) == 0 && current.generation == generation {
			delete(a.inputs, work)
		} else {
			q.Add(work)
		}
	}
	if !deadline.IsZero() {
		q.AddAfter(work, max(deadline.Sub(a.currentTime()), 0))
	}
	if err != nil {
		a.logger.WithError(err).WithField("pr", work.number).Warn("Agentic reconciliation awaits its deadline or a relevant event")
	}
	return true
}

func (a *agenticController) restoreRecords(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.prepareStoreLocked(); err != nil {
		return err
	}
	if a.store == nil || a.dryRun {
		return nil
	}
	for _, name := range a.store.recordNames() {
		r, err := a.readStoredRecord(ctx, name)
		if err != nil {
			return err
		}
		if r == nil {
			continue
		}
		work := agenticWork{org: r.State.Org, repo: r.State.Repo, number: r.State.Number}
		if agenticUnfinished(r) {
			a.queue.Add(work)
		} else if r.State.WaitingSince != nil {
			a.queue.AddAfter(work, max(r.State.WaitingSince.Add(a.options.timeout).Sub(a.currentTime()), 0))
		}
	}
	return nil
}
