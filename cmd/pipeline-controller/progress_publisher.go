package main

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	// progressPrefix identifies a dashboard owned by the original notification author.
	progressPrefix = pipelineNotificationHeading + "\n\n<!-- pipeline-controller:progress:v1 -->\n"
	// progressBot is the account that owns the opening pipeline notification in production.
	progressBot = "openshift-merge-bot[bot]"
	// progressCacheLimit bounds idle PR bookkeeping in long-lived controllers.
	progressCacheLimit = 2048
)

// progressCommentClient deliberately cannot create or delete a comment.
type progressCommentClient interface {
	ListIssueCommentsWithContext(context.Context, string, string, int) ([]github.IssueComment, error)
	EditCommentWithContext(context.Context, string, string, int, string) error
}

// progressComment is the last known original comment, or a short-lived negative lookup.
type progressComment struct {
	id     int
	hash   [32]byte
	looked time.Time
	gone   bool
}

// progressEntry coalesces events for one PR rather than adding one API request per event.
type progressEntry struct {
	base, head string
	baseline   bool
	dirty      bool
	body       string
	touched    time.Time
	welcome    time.Time
	comment    progressComment
}

// progressPublisher keeps GitHub comment I/O off the scheduling workers.
type progressPublisher struct {
	gh     progressCommentClient
	logger *logrus.Entry
	wake   chan struct{}
	mu     sync.Mutex
	prs    map[agenticWork]*progressEntry
}

// newProgressPublisher starts with an empty cache and no GitHub reads.
func newProgressPublisher(gh progressCommentClient, logger *logrus.Entry) *progressPublisher {
	return &progressPublisher{gh: gh, logger: logger, wake: make(chan struct{}, 1), prs: make(map[agenticWork]*progressEntry)}
}

// submit accepts a current revision from a PR event or live read (follow=true).
// Dispatch observations alone cannot replace a different known revision.
// A baseline waiting view must not replace progress for the same revision.
func (p *progressPublisher) submit(refs *v1.Refs, body string, follow, baseline bool) {
	if p == nil || refs == nil || len(refs.Pulls) != 1 || refs.Pulls[0].SHA == "" {
		return
	}
	work := agenticWork{org: refs.Org, repo: refs.Repo, number: refs.Pulls[0].Number}
	head := refs.Pulls[0].SHA
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.prs[work]
	if e == nil {
		if body == "" {
			return
		}
		if len(p.prs) >= progressCacheLimit {
			var oldest agenticWork
			var when time.Time
			for key, entry := range p.prs {
				if entry.dirty || (!when.IsZero() && !entry.touched.Before(when)) {
					continue
				}
				oldest, when = key, entry.touched
			}
			if when.IsZero() {
				return // A saturated display queue never delays scheduling.
			}
			delete(p.prs, oldest)
		}
		e = &progressEntry{head: head, base: refs.BaseRef, baseline: true}
		p.prs[work] = e
	}
	if e.head != head || e.base != refs.BaseRef {
		if !follow {
			return
		}
		e.head, e.base, e.baseline = head, refs.BaseRef, true
	}
	if baseline && !e.baseline {
		return
	}
	if e.body == body && e.baseline == baseline {
		return
	}
	e.body, e.baseline, e.dirty, e.touched = body, baseline, body != "", time.Now()
	if e.dirty {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// welcomeCreated unblocks a lookup that raced the normal opening comment.
func (p *progressPublisher) welcomeCreated(work agenticWork) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.prs[work]; e != nil && e.comment.id == 0 && e.body != "" {
		e.welcome = time.Now()
		e.comment.looked = time.Time{}
		e.dirty = true
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// knowsRevision skips verification for repeated legacy PR deliveries.
func (p *progressPublisher) knowsRevision(refs *v1.Refs) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.prs[agenticWork{org: refs.Org, repo: refs.Repo, number: refs.Pulls[0].Number}]
	return e != nil && e.body != "" && e.head == refs.Pulls[0].SHA && e.base == refs.BaseRef
}

// run publishes the newest available snapshots; there is no polling or replay on startup.
func (p *progressPublisher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			for ctx.Err() == nil && p.publishNext(ctx) {
			}
		}
	}
}

// publishNext updates at most one comment, with no lock held during API calls.
func (p *progressPublisher) publishNext(ctx context.Context) bool {
	p.mu.Lock()
	var work agenticWork
	var entry *progressEntry
	for key, candidate := range p.prs {
		if candidate.dirty {
			work, entry = key, candidate
			break
		}
	}
	if entry == nil {
		p.mu.Unlock()
		return false
	}
	body, comment := entry.body, entry.comment
	welcome := entry.welcome
	entry.dirty = false
	p.mu.Unlock()
	if comment.gone {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if comment.id == 0 {
		if time.Since(comment.looked) < 5*time.Minute {
			return true
		}
		comments, err := p.gh.ListIssueCommentsWithContext(ctx, work.org, work.repo, work.number)
		if err != nil {
			p.logFailure(work, err)
			return true
		}
		comment = originalProgressComment(comments, time.Now())
	}
	hash := sha256.Sum256([]byte(body))
	p.mu.Lock()
	current := p.prs[work] == entry && entry.body == body
	if comment.id != 0 || entry.welcome.Equal(welcome) {
		entry.comment = comment
	}
	p.mu.Unlock()
	if !current || comment.id == 0 || comment.hash == hash {
		return true
	}
	if err := p.gh.EditCommentWithContext(ctx, work.org, work.repo, comment.id, body); err != nil {
		p.logFailure(work, err)
		if strings.HasPrefix(err.Error(), "status code 404 ") {
			// The original was deleted or is no longer writable. Do not adopt a later comment.
			p.mu.Lock()
			entry.comment = progressComment{gone: true}
			p.mu.Unlock()
		}
		return true
	}
	p.mu.Lock()
	entry.comment.hash = hash
	p.mu.Unlock()
	return true
}

// originalProgressComment finds the earliest welcome comment, not a scheduling or help reply.
func originalProgressComment(comments []github.IssueComment, now time.Time) progressComment {
	result := progressComment{looked: now}
	for _, comment := range comments {
		if comment.ID <= 0 || !strings.EqualFold(comment.User.Login, progressBot) || (result.id != 0 && comment.ID >= result.id) {
			continue
		}
		body := normalizePipelineComment(comment.Body)
		if body == pullRequestInfoComment || strings.HasPrefix(body, progressPrefix) {
			result.id, result.hash = comment.ID, sha256.Sum256([]byte(body))
		}
	}
	return result
}

// logFailure surfaces display-only errors without retrying scheduling or leaking comment contents.
func (p *progressPublisher) logFailure(work agenticWork, err error) {
	p.logger.WithError(err).WithFields(logrus.Fields{"org": work.org, "repo": work.repo, "pr": work.number}).Warn("Could not update pipeline progress comment")
}
