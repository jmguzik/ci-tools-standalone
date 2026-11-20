package main

import (
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"time"

	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

const (
	// progressPrefix identifies a dashboard owned by the original notification author.
	progressPrefix = pipelineNotificationHeading + "\n\n<!-- pipeline-controller:progress:v1 -->\n"
	// progressBot is the GraphQL login of the bot that owns the opening notification.
	progressBot = "openshift-merge-bot"
	// progressCacheLimit bounds idle PR bookkeeping in long-lived controllers.
	progressCacheLimit = 2048
	// progressRetryDelay bounds retries from noisy events after an API failure.
	progressRetryDelay = 30 * time.Second
	// progressMissTTL avoids repeatedly looking for a missing opening comment.
	progressMissTTL = 5 * time.Minute
	// progressAPITimeout bounds optional display I/O.
	progressAPITimeout = 15 * time.Second
)

// progressCommentClient deliberately cannot create or delete a comment.
type progressCommentClient interface {
	QueryWithGitHubAppsSupport(context.Context, interface{}, map[string]interface{}, string) error
	EditCommentWithContext(context.Context, string, string, int, string) error
}

// progressCommentAuthor distinguishes the bot from users with a similar login.
type progressCommentAuthor struct {
	Login string
	Type  string `graphql:"__typename"`
}

// progressCommentNode contains only the fields needed to identify and edit the original comment.
type progressCommentNode struct {
	// fullDatabaseId is a string on the wire and can exceed the GraphQL Int range.
	FullDatabaseID string `graphql:"fullDatabaseId"`
	Body           string
	Author         progressCommentAuthor
}

// progressCommentQuery verifies the PR and optionally reads its first ten comments.
type progressCommentQuery struct {
	Repository struct {
		PullRequest struct {
			HeadRefOID  string `graphql:"headRefOid"`
			BaseRefName string
			State       string
			IsDraft     bool
			Comments    struct {
				Nodes []progressCommentNode
			} `graphql:"comments(first: 10) @include(if: $comments)"`
		} `graphql:"pullRequest(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

// progressRevision identifies a rendering cycle independently of the comment ID.
type progressRevision struct{ head, base string }

// progressSnapshot returns authoritative revision metadata without a second request.
func (q *progressCommentQuery) progressSnapshot() (progressRevision, bool) {
	pr := q.Repository.PullRequest
	return progressRevision{head: pr.HeadRefOID, base: pr.BaseRefName}, pr.State == "OPEN" && !pr.IsDraft
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
	verified   progressRevision
	paused     bool
	request    progressRequest
	dirty      bool
	generation uint64
	touched    time.Time
	welcome    time.Time
	comment    progressComment
	retryAt    time.Time
}

// progressRequest is a pending render, not proof that its revision is current.
type progressRequest struct {
	revision progressRevision
	body     string
	baseline bool
}

// progressGeneration prevents an in-flight lookup from rolling back a more recent observation.
type progressGeneration struct {
	entry   *progressEntry
	version uint64
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

// submit queues a render. Only an observation from the live Chai worker may
// follow a different known revision, which still requires verification before editing.
func (p *progressPublisher) submit(refs *v1.Refs, body string, follow, baseline bool) {
	if p == nil || refs == nil || len(refs.Pulls) != 1 || refs.Pulls[0].SHA == "" || body == "" {
		return
	}
	work := agenticWork{org: refs.Org, repo: refs.Repo, number: refs.Pulls[0].Number}
	rev := progressRevision{head: refs.Pulls[0].SHA, base: refs.BaseRef}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.prs[work]
	if e != nil && !follow {
		if e.paused || (e.verified.head != "" && e.verified != rev) ||
			(e.dirty && e.request.revision != rev) {
			return
		}
	}
	p.queue(work, e, progressRequest{revision: rev, body: body, baseline: baseline})
}

// newEntry makes room for a PR without evicting an update waiting to be published. Caller holds mu.
func (p *progressPublisher) newEntry(work agenticWork) *progressEntry {
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
			return nil // A saturated display queue never delays scheduling.
		}
		delete(p.prs, oldest)
	}
	e := &progressEntry{}
	p.prs[work] = e
	return e
}

// queue coalesces pending renders but lets a later event retry an unsuccessful one. Caller holds mu.
func (p *progressPublisher) queue(work agenticWork, e *progressEntry, request progressRequest) {
	if e == nil {
		e = p.newEntry(work)
		if e == nil {
			return
		}
	}
	if request.baseline && e.request.revision == request.revision && e.request.body != "" && !e.request.baseline {
		return
	}
	if e.comment.gone {
		return
	}
	now := time.Now()
	if e.request == request && (e.dirty || now.Before(e.retryAt) ||
		(e.verified == request.revision && !e.paused && e.comment.hash == sha256.Sum256([]byte(request.body))) ||
		(e.comment.id == 0 && now.Sub(e.comment.looked) < progressMissTTL)) {
		return
	}
	e.request, e.dirty, e.touched = request, true, now
	e.generation++
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// welcomeCreated unblocks a lookup that raced the normal opening comment.
func (p *progressPublisher) welcomeCreated(work agenticWork) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.prs[work]; e != nil && e.comment.id == 0 && !e.comment.gone && e.request.body != "" {
		e.welcome = time.Now()
		e.comment.looked, e.retryAt = time.Time{}, time.Time{}
		e.dirty = true
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// lifecycleGeneration decides whether a lifecycle event is worth verifying and captures a guard for its result.
func (p *progressPublisher) lifecycleGeneration(work agenticWork, revision progressRevision, inactive bool) (progressGeneration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.prs[work]
	if e == nil {
		return progressGeneration{}, !inactive
	}
	return progressGeneration{e, e.generation}, !e.comment.gone && (e.verified != revision || e.paused != inactive)
}

// applyLifecycle changes the verified revision or pause state only if no newer submission intervened.
// A false return asks the caller to recheck rather than apply a stale lookup result.
func (p *progressPublisher) applyLifecycle(work agenticWork, generation progressGeneration, revision progressRevision, body string, comment progressComment) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.prs[work]
	if e != generation.entry || (e != nil && e.generation != generation.version) {
		return false
	}
	if e == nil {
		if body == "" {
			return true
		}
		e = p.newEntry(work)
		if e == nil {
			return true
		}
		e.comment = comment
	}
	request := progressRequest{revision: revision, body: body, baseline: true}
	if body != "" && e.request.revision == revision && e.request.body != "" && !e.request.baseline {
		request = e.request // Keep a more informative observation for this revision.
	}
	e.verified, e.paused = revision, body == ""
	if body == "" {
		e.request, e.dirty = progressRequest{}, false
		e.generation++
		return true
	}
	p.queue(work, e, request)
	return true
}

// lookup bounds a progress-only query, optionally finding the original comment in the same request.
func (p *progressPublisher) lookup(ctx context.Context, work agenticWork, comments bool) (progressCommentQuery, error) {
	ctx, cancel := context.WithTimeout(ctx, progressAPITimeout)
	defer cancel()
	var query progressCommentQuery
	err := p.gh.QueryWithGitHubAppsSupport(ctx, &query, map[string]interface{}{
		"owner": githubql.String(work.org), "name": githubql.String(work.repo), "number": githubql.Int(work.number),
		"comments": githubql.Boolean(comments),
	}, work.org)
	return query, err
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
	request, comment := entry.request, entry.comment
	verified, welcome, generation := entry.verified, entry.welcome, entry.generation
	paused := entry.paused
	entry.dirty = false
	entry.retryAt = time.Now().Add(progressRetryDelay)
	p.mu.Unlock()
	if comment.gone {
		return true
	}
	if comment.id == 0 && time.Since(comment.looked) < progressMissTTL {
		return true
	}
	if comment.id == 0 || verified != request.revision || paused {
		query, err := p.lookup(ctx, work, comment.id == 0)
		if err != nil {
			p.logFailure(work, err)
			return true
		}
		var active bool
		verified, active = query.progressSnapshot()
		paused = !active
		if comment.id == 0 {
			comment = originalProgressComment(query.Repository.PullRequest.Comments.Nodes, time.Now())
		}
	}
	hash := sha256.Sum256([]byte(request.body))
	p.mu.Lock()
	current := p.prs[work] == entry && entry.generation == generation
	if comment.id != 0 || entry.welcome.Equal(welcome) {
		entry.comment = comment
	}
	if current {
		entry.verified, entry.paused = verified, paused
	}
	p.mu.Unlock()
	if !current || paused || verified != request.revision || comment.id == 0 || comment.hash == hash {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, progressAPITimeout)
	defer cancel()
	if err := p.gh.EditCommentWithContext(ctx, work.org, work.repo, comment.id, request.body); err != nil {
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
func originalProgressComment(comments []progressCommentNode, now time.Time) progressComment {
	result := progressComment{looked: now}
	for _, comment := range comments {
		if comment.Author.Type != "Bot" || !strings.EqualFold(comment.Author.Login, progressBot) {
			continue
		}
		id, err := strconv.Atoi(comment.FullDatabaseID)
		if err != nil || id <= 0 || (result.id != 0 && id >= result.id) {
			continue
		}
		body := normalizePipelineComment(comment.Body)
		if body == pullRequestInfoComment || strings.HasPrefix(body, progressPrefix) {
			result.id, result.hash = id, sha256.Sum256([]byte(body))
		}
	}
	return result
}

// logFailure surfaces display-only errors without retrying scheduling or leaking comment contents.
func (p *progressPublisher) logFailure(work agenticWork, err error) {
	p.logger.WithError(err).WithFields(logrus.Fields{"org": work.org, "repo": work.repo, "pr": work.number}).Warn("Could not update pipeline progress comment")
}
