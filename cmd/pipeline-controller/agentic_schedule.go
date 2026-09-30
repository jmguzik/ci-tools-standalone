package main

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

// number == 0 denotes a repository lookup, optionally restricted to a status
// event's SHA. These lookups happen on startup/events, never on a scan interval.
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
}

func (a *agenticController) stopScheduling() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scheduler != nil {
		for _, pending := range a.scheduler.pending {
			pending.timer.Stop()
		}
		a.scheduler = nil
	}
}

func (a *agenticController) reconcile(ctx context.Context, org, repo string, number int, comment *github.IssueComment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reconcileWork(ctx, agenticWork{org: org, repo: repo, number: number}, comment, 0)
}

// Caller holds mu, including timer callbacks. Failed operations retry only
// their own PR/repository with capped backoff; idle work has no timer.
func (a *agenticController) reconcileWork(ctx context.Context, work agenticWork, comment *github.IssueComment, backoff time.Duration) (err error) {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.number != 0 {
		return a.reconcilePull(ctx, work.org, work.repo, work.number, comment, &deadline)
	}
	if !a.hasRepo(work.org, work.repo) {
		return nil
	}
	prs, err := a.gh.GetPullRequests(work.org, work.repo)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if work.sha != "" && pr.Head.SHA != work.sha {
			continue
		}
		pull := agenticWork{org: work.org, repo: work.repo, number: pr.Number}
		if err := a.reconcileWork(ctx, pull, nil, 0); err != nil {
			a.logger.WithError(err).WithField("pr", pr.Number).Error("Agentic recovery failed")
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
		// An API error before reading the journal must not lose a known deadline.
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
		a.logger.WithError(err).WithFields(map[string]interface{}{"org": work.org, "repo": work.repo, "pr": work.number}).Warn("Agentic recovery awaits a relevant event or restart")
	}
	if at.IsZero() {
		return
	}
	s.pending[work] = next
	next.timer = time.AfterFunc(max(at.Sub(now), 0), func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		// A newer event, changed revision, or completed dispatch may have
		// canceled this callback even if its timer had already fired.
		if a.scheduler != s || s.pending[work] != next || s.ctx.Err() != nil {
			return
		}
		if err := a.reconcileWork(s.ctx, work, next.comment, next.backoff); err != nil {
			a.logger.WithError(err).WithField("pr", work.number).Error("Scheduled agentic reconciliation failed")
		}
	})
}
