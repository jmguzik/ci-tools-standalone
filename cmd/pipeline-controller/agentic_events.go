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

func validAgenticCommand(command string) bool {
	switch command {
	case "required", "remaining", "auto", "agent-review", "skip-agent-review":
		return true
	default:
		return false
	}
}

func (a *agenticController) recordCommand(gate *github.CheckRun, state *agenticState, comment github.IssueComment) error {
	matches := agenticCommandRE.FindAllStringSubmatch(comment.Body, -1)
	if len(matches) != 1 || comment.ID <= state.LastCommandID || comment.ID <= 0 {
		return nil
	}
	// Once a gate observes a new revision, delayed deliveries of older commands
	// cannot authorize it. Early commands already recorded on disk survive
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
	state.LastCommandID = comment.ID
	state.Command = &agenticCommand{ID: comment.ID, Command: strings.ToLower(matches[0][1])}
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
		switch command.Command {
		case "required":
			state.ManualRequestID, state.ForceRequestID = command.ID, command.ID
			state.Dispatch = nil
		case "remaining":
			state.ManualRequestID = command.ID
		case "auto":
			if cfg.Trigger != "lgtm" {
				body := fmt.Sprintf("`/pipeline auto` is available only in LGTM mode.\n\n<!-- pipeline-controller:command:%d -->", command.ID)
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
				body := fmt.Sprintf("Normal selection is enabled for future pushes. The dispatched selection for `%s` is already fixed.\n\n<!-- pipeline-controller:command:%d -->", state.HeadSHA, command.ID)
				if err := a.ensureComment(state, comments, body); err != nil {
					return err
				}
			}
		case "agent-review":
			if state.Frozen {
				body := fmt.Sprintf("The selection for `%s` is already dispatched and cannot be replaced. Push a new commit to request a new selection.\n\n<!-- pipeline-controller:command:%d -->", state.HeadSHA, command.ID)
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
				state.Plan, state.WaitingSince = nil, nil
				state.Review = &agenticReviewRequest{HeadSHA: state.HeadSHA, BaseBranch: state.BaseBranch,
					RequestID: agenticID(state.Org, state.Repo, strconv.Itoa(state.Number), state.HeadSHA, state.BaseBranch, strconv.Itoa(command.ID))}
				state.ReviewPosted = false
			}
		}
		command.Applied = true
		state.PendingDispatch = true // The command's resulting decision is not yet reconciled.
		if err := a.saveState(gate, state, "", "Pipeline request accepted; waiting for dispatch prerequisites."); err != nil {
			return err
		}
	}
	if state.Review != nil && !state.ReviewPosted {
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
	var command *github.IssueComment
	if event.Action == github.IssueCommentActionCreated {
		command = &event.Comment
	}
	if a.deferCommentRouting(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command) {
		return true
	}
	pr, err := a.gh.GetPullRequest(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number)
	if err != nil {
		logger.WithError(err).Error("Cannot determine agentic configuration")
		a.retryReconciliation(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command, err)
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
	if err := a.reconcile(context.Background(), event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, command); err != nil {
		logger.WithError(err).Error("Agentic comment reconciliation failed")
	}
	return true
}

func (a *agenticController) handlePullRequest(logger *logrus.Entry, event github.PullRequestEvent) {
	if !a.hasRepo(event.Repo.Owner.Login, event.Repo.Name) {
		return
	}
	if err := a.reconcile(context.Background(), event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, nil); err != nil {
		logger.WithError(err).Error("Agentic pull request reconciliation failed")
	}
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

// Legacy events carry snapshots. In a mixed-mode repository a delayed event
// from a normal branch must not publish placeholders or /test commands after
// the PR moves to an agentic branch. Normal-only repositories need no extra API
// call and retain their existing behavior.
func (a *agenticController) allowLegacySnapshot(org, repo string, number int, head, base string) (bool, error) {
	if !a.hasRepo(org, repo) {
		return true, nil
	}
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		return false, nil
	}
	if _, enabled := a.repoConfig(org, repo, base); enabled {
		return false, nil
	}
	pr, err := a.gh.GetPullRequest(org, repo, number)
	if err != nil {
		return false, err
	}
	_, enabled := a.repoConfig(org, repo, pr.Base.Ref)
	return !enabled && pr.State == github.PullRequestStateOpen && !pr.Draft && pr.Head.SHA == head && pr.Base.Ref == base, nil
}

func (cw *clientWrapper) allowLegacyPullRequest(logger *logrus.Entry, event github.PullRequestEvent) bool {
	allowed, err := cw.agentic.allowLegacySnapshot(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, event.PullRequest.Head.SHA, event.PullRequest.Base.Ref)
	if err != nil {
		logger.WithError(err).Error("Cannot safely route legacy pull request event")
	}
	return allowed && err == nil
}

func (a *agenticController) reconcileRepo(ctx context.Context, org, repo, sha string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reconcileWork(ctx, agenticWork{org: org, repo: repo, sha: sha}, nil, 0); err != nil {
		a.logger.WithError(err).WithField("repo", org+"/"+repo).Error("Cannot recover agentic pull requests")
	}
}

func (a *agenticController) handleStatus(_ *logrus.Entry, event github.StatusEvent) {
	if !a.shouldHandleAgenticStatus(event) {
		return
	}
	a.reconcileRepo(context.Background(), event.Repo.Owner.Login, event.Repo.Name, event.SHA)
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
	a.startScheduling(ctx)
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stopped = true
		if a.scheduler != nil {
			for _, pending := range a.scheduler.pending {
				pending.timer.Stop()
			}
			a.scheduler = nil
		}
		if err := a.closeStoreLocked(); err != nil {
			a.logger.WithError(err).Error("Cannot close agentic state directory")
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	if !a.hasAgenticEnrollment() || a.dryRun {
		<-ctx.Done()
		return nil
	}
	if err := a.restoreRecords(ctx); err != nil {
		return fmt.Errorf("restoring agentic records: %w", err)
	}
	<-ctx.Done()
	return nil
}

// Reevaluate already tracked PRs on explicit configuration changes, without
// discovering new PRs or scanning GitHub repositories.
func (a *agenticController) configurationChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.scheduler
	if !a.hasAgenticEnrollment() && len(a.statusContexts) == 0 {
		return
	}
	if s != nil && s.ctx.Err() == nil {
		if a.dryRun {
			return
		}
		if err := a.prepareStoreLocked(); err != nil {
			a.logger.WithError(err).Error("Cannot reload tracked agentic PRs")
			return
		}
		if a.store == nil {
			return
		}
		for _, name := range a.store.recordNames() {
			if s.ctx.Err() != nil {
				return
			}
			r, err := a.readStoredRecord(s.ctx, name)
			if err != nil {
				a.logger.WithError(err).Error("Cannot reload tracked agentic PR")
				return
			}
			if r == nil {
				continue
			}
			if err := a.reconcileWork(s.ctx, agenticWork{org: r.State.Org, repo: r.State.Repo, number: r.State.Number}, nil, 0); err != nil {
				a.logger.WithError(err).Error("Cannot reevaluate tracked agentic PR")
			}
		}
	}
}
