package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/prow/pkg/github"
)

var agenticCommandRE = regexp.MustCompile(`(?im)^/pipeline[\t ]+(required|remaining|auto|agent-review|skip-agent-review)[\t ]*$`)

// Preserve ordinary-mode command prefixes while filtering unrelated comments
// before any GitHub read, including in mixed-mode repositories.
var pipelineCommentRE = regexp.MustCompile(`(?im)^/pipeline\s+(required|remaining|auto|agent-review|skip-agent-review)`)

func validAgenticCommand(command string) bool {
	switch command {
	case "required", "remaining", "auto", "agent-review", "skip-agent-review":
		return true
	default:
		return false
	}
}

func (a *agenticController) recordCommand(gate *github.CheckRun, state *agenticState, pr *github.PullRequest, comment github.IssueComment, comments []github.IssueComment) error {
	matches := agenticCommandRE.FindAllStringSubmatch(comment.Body, -1)
	if len(matches) != 1 || comment.ID <= state.LastCommandID || comment.ID <= 0 {
		return nil
	}
	// Once a gate observes a new revision, delayed deliveries of older commands
	// cannot authorize it. Recorded command decisions survive
	// restarts without reinterpreting comment text against another HEAD.
	if comment.CreatedAt.IsZero() || comment.CreatedAt.Before(state.ObservedAt) {
		return nil
	}
	trusted, err := a.trustedCommandAuthor(state.Org, state.Repo, comment.User.Login)
	if err != nil {
		return err
	}
	if !trusted {
		return nil
	}
	command := &agenticCommand{Command: strings.ToLower(matches[0][1])}
	if command.Command == "required" || command.Command == "remaining" {
		if err := a.refreshPlan(state, pr, comments); err != nil {
			if _, pending := err.(agenticPlanPendingError); !pending {
				return err
			}
		}
		// A missed early command must not be revived by a later Chai comment.
		command.Rejected = state.Plan == nil || (state.Plan.Source == "chai" && state.Plan.CommentID > comment.ID)
	}
	state.LastCommandID = comment.ID
	state.Command = command
	return a.saveState(gate, state, "", "Pipeline request recorded for this HEAD/base.")
}

func (a *agenticController) trustedCommandAuthor(org, repo, login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	member, err := a.gh.IsMember(org, login)
	if err != nil || member {
		return member, err
	}
	return a.gh.IsCollaborator(org, repo, login)
}

func (a *agenticController) explainUnboundCommand(state *agenticState, comment github.IssueComment, comments []github.IssueComment) error {
	if state.Command != nil || state.ManualRequestID != 0 || comment.ID > state.LastCommandID || !agenticCommandRE.MatchString(comment.Body) {
		return nil
	}
	trusted, err := a.trustedCommandAuthor(state.Org, state.Repo, comment.User.Login)
	if err != nil || !trusted {
		return err
	}
	body := fmt.Sprintf("This command predates tracking of `%s` → `%s`. Please post it again for this revision.\n\n<!-- pipeline-controller:command:%d -->", state.HeadSHA, state.BaseBranch, comment.ID)
	return a.ensureComment(state, comments, body)
}

func (a *agenticController) ensureComment(state *agenticState, comments []github.IssueComment, body string) error {
	isBot, err := a.gh.BotUserChecker()
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if isBot(comment.User.Login) && comment.Body == body {
			return nil
		}
	}
	return a.gh.CreateComment(state.Org, state.Repo, state.Number, body)
}

func (a *agenticController) applyCommand(gate *github.CheckRun, state *agenticState, cfg RepoConfig, pr *github.PullRequest, comments []github.IssueComment) error {
	if command := state.Command; command != nil && !command.Applied {
		if command.Rejected {
			body := fmt.Sprintf("Cannot run `/pipeline %s`: test selection is not ready. Retry after Chai or the controller selects jobs. This request is not queued.\n\n<!-- pipeline-controller:command:%d -->", command.Command, state.LastCommandID)
			if err := a.ensureComment(state, comments, body); err != nil {
				return err
			}
			command.Applied = true
			return a.saveState(gate, state, "", "Pipeline request rejected: waiting for test selection.")
		}
		switch command.Command {
		case "required":
			state.ManualRequestID, state.ForceRequestID = state.LastCommandID, state.LastCommandID
			state.Dispatch = nil
		case "remaining":
			state.ManualRequestID = state.LastCommandID
			state.ForceRequestID = 0
		case "auto":
			if cfg.Trigger != "lgtm" {
				body := fmt.Sprintf("`/pipeline auto` is available only in LGTM mode.\n\n<!-- pipeline-controller:command:%d -->", state.LastCommandID)
				if err := a.ensureComment(state, comments, body); err != nil {
					return err
				}
			} else if !hasAgenticLabel(pr, PipelineAutoLabel) {
				if err := a.gh.AddLabel(state.Org, state.Repo, state.Number, PipelineAutoLabel); err != nil {
					return err
				}
				pr.Labels = append(pr.Labels, github.Label{Name: PipelineAutoLabel})
			}
		case "skip-agent-review":
			if !hasAgenticLabel(pr, agenticSkipLabel) {
				if err := a.gh.AddLabel(state.Org, state.Repo, state.Number, agenticSkipLabel); err != nil {
					return err
				}
				pr.Labels = append(pr.Labels, github.Label{Name: agenticSkipLabel})
			}
			if !state.Frozen {
				state.Plan, state.WaitingSince = nil, nil
			} else {
				body := fmt.Sprintf("Normal selection is enabled for future pushes. The dispatched selection for `%s` is already fixed.\n\n<!-- pipeline-controller:command:%d -->", state.HeadSHA, state.LastCommandID)
				if err := a.ensureComment(state, comments, body); err != nil {
					return err
				}
			}
		case "agent-review":
			if state.Frozen {
				body := fmt.Sprintf("The selection for `%s` is already dispatched and cannot be replaced. Push a new commit to request a new selection.\n\n<!-- pipeline-controller:command:%d -->", state.HeadSHA, state.LastCommandID)
				if err := a.ensureComment(state, comments, body); err != nil {
					return err
				}
			} else {
				if hasAgenticLabel(pr, agenticSkipLabel) {
					if err := a.gh.RemoveLabel(state.Org, state.Repo, state.Number, agenticSkipLabel); err != nil {
						return err
					}
					var labels []github.Label
					for _, label := range pr.Labels {
						if label.Name != agenticSkipLabel {
							labels = append(labels, label)
						}
					}
					pr.Labels = labels
				}
				state.Plan = nil
				state.WaitingSince = nil
				if state.ActivatedAt != nil {
					now := a.currentTime()
					state.WaitingSince = &now
				}
				state.Review = &agenticReviewRequest{HeadSHA: state.HeadSHA, BaseBranch: state.BaseBranch,
					RequestID: agenticID(state.Org, state.Repo, strconv.Itoa(state.Number), state.HeadSHA, state.BaseBranch, strconv.Itoa(state.LastCommandID))}
				state.ReviewPosted = false
			}
		}
		command.Applied = true
		state.PendingDispatch = true // The command's resulting decision is not yet reconciled.
		if err := a.saveState(gate, state, "", "Pipeline request accepted; waiting for dispatch prerequisites."); err != nil {
			return err
		}
	}
	if state.Review != nil && !state.ReviewPosted && state.ActivatedAt != nil {
		if err := a.ensureComment(state, comments, formatAgenticReview(*state.Review)); err != nil {
			return err
		}
		state.ReviewPosted = true
		return a.saveState(gate, state, "", "Fresh Chai selection requested through the PR comment.")
	}
	return nil
}

func (a *agenticController) handleIssueComment(logger *logrus.Entry, event github.IssueCommentEvent) bool {
	if !event.Issue.IsPullRequest() || a == nil || !a.hasRepo(event.Repo.Owner.Login, event.Repo.Name) {
		return false
	}
	if !isAgenticPlanComment(event.Comment.Body) && !pipelineCommentRE.MatchString(event.Comment.Body) {
		return true
	}
	a.queueMu.Lock()
	shutdown := a.queue != nil && a.queue.ShuttingDown()
	cooldown := a.retryAt[agenticWork{org: event.Repo.Owner.Login, repo: event.Repo.Name, number: event.Issue.Number}].After(a.currentTime())
	a.queueMu.Unlock()
	if shutdown {
		return true
	}
	var command *github.IssueComment
	if event.Action == github.IssueCommentActionCreated {
		command = &event.Comment
	}
	if cooldown {
		a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command)
		return true // The worker honors the cooldown and rereads live PR metadata.
	}
	pr, err := a.gh.GetPullRequest(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number)
	if err != nil {
		logger.WithError(err).Error("Cannot determine agentic configuration")
		if retry := agenticRetryFor(err); retry.after > 0 {
			a.queueMu.Lock()
			a.rememberCooldownLocked(agenticWork{org: event.Repo.Owner.Login, repo: event.Repo.Name, number: event.Issue.Number}, retry.after)
			a.queueMu.Unlock()
		}
		a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command)
		return true // fail closed instead of falling through to /test dispatch
	}
	if _, enabled := a.repoConfig(event.Repo.Owner.Login, event.Repo.Name, pr.Base.Ref); !enabled {
		return false
	}
	if event.Action == github.IssueCommentActionDeleted {
		return true
	}
	if !isAgenticPlanComment(event.Comment.Body) && !agenticCommandRE.MatchString(event.Comment.Body) {
		return true
	}
	a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command)
	return true
}

func (a *agenticController) handlePullRequest(_ *logrus.Entry, event github.PullRequestEvent) {
	if !a.hasRepo(event.Repo.Owner.Login, event.Repo.Name) {
		return
	}
	a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, nil)
}

func (a *agenticController) hasRepo(org, repo string) bool {
	if a == nil {
		return false
	}
	for _, watcher := range []*watcher{a.watcher, a.lgtmWatcher} {
		if watcher != nil && watcher.getConfig()[org][repo].Agentic.enabled() {
			return true
		}
	}
	return false
}

func (a *agenticController) hasAgenticEnrollment() bool {
	for _, watcher := range []*watcher{a.watcher, a.lgtmWatcher} {
		if watcher == nil {
			continue
		}
		for _, repos := range watcher.getConfig() {
			for _, cfg := range repos {
				if cfg.Agentic.enabled() {
					return true
				}
			}
		}
	}
	return false
}

// Mixed-mode events must match the live PR. Universal notifications may reach
// agentic branches; legacy placeholders and dispatch must not. Normal-only
// repositories retain their existing behavior without an extra GitHub read.
func (a *agenticController) allowSnapshot(org, repo string, number int, head, base string, allowAgentic bool) (bool, error) {
	if !a.hasRepo(org, repo) {
		return true, nil
	}
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped || (allowAgentic && a.dryRun) {
		return false, nil
	}
	if _, enabled := a.repoConfig(org, repo, base); enabled && !allowAgentic {
		return false, nil
	}
	pr, err := a.gh.GetPullRequest(org, repo, number)
	if err != nil {
		return false, err
	}
	_, enabled := a.repoConfig(org, repo, pr.Base.Ref)
	return (allowAgentic || !enabled) && pr.State == github.PullRequestStateOpen && !pr.Draft && pr.Head.SHA == head && pr.Base.Ref == base, nil
}

func (cw *clientWrapper) allowPullRequestSnapshot(logger *logrus.Entry, event github.PullRequestEvent, allowAgentic bool) bool {
	allowed, err := cw.agentic.allowSnapshot(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, event.PullRequest.Head.SHA, event.PullRequest.Base.Ref, allowAgentic)
	if err != nil {
		logger.WithError(err).Error("Cannot safely route pull request event")
	}
	return allowed && err == nil
}

// Restart restores local deadlines and explicitly unfinished actions only.
// Missed events are accepted; there is no startup or periodic GitHub sweep.
func (a *agenticController) Run(ctx context.Context) error {
	a.mu.Lock()
	a.stopped = false
	a.mu.Unlock()
	if err := a.prepareStore(); err != nil {
		return fmt.Errorf("opening agentic state: %w", err)
	}
	a.startQueue(ctx)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			a.queue.ShutDown()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		a.queue.ShutDown()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stopped = true
		if err := a.closeStoreLocked(); err != nil {
			a.logger.WithError(err).Error("Cannot close agentic state directory")
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	if err := a.restoreRecords(ctx); err != nil {
		return fmt.Errorf("restoring agentic records: %w", err)
	}
	for a.processNext(ctx) {
	}
	return nil
}

// Reevaluate already tracked PRs on explicit configuration changes, without
// discovering new PRs or scanning GitHub repositories.
func (a *agenticController) configurationChanged() {
	a.mu.Lock()
	ctx := a.queueCtx
	if a.stopped || a.dryRun || ctx == nil || ctx.Err() != nil {
		a.mu.Unlock()
		return
	}
	if err := a.prepareStoreLocked(); err != nil {
		a.mu.Unlock()
		a.logger.WithError(err).Error("Cannot reload tracked agentic PRs")
		return
	}
	if a.store == nil {
		a.mu.Unlock()
		return
	}
	names := a.store.recordNames()
	a.mu.Unlock()
	for _, name := range names {
		a.mu.Lock()
		if a.stopped || ctx.Err() != nil {
			a.mu.Unlock()
			return
		}
		r, err := a.readStoredRecord(ctx, name)
		if err != nil {
			a.mu.Unlock()
			a.logger.WithError(err).Error("Cannot reload tracked agentic PR")
			return
		}
		if r != nil {
			a.enqueue(r.State.Org, r.State.Repo, r.State.Number, nil)
		}
		a.mu.Unlock()
	}
}
