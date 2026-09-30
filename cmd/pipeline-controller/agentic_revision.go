package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

// Legacy import only. New revisions are tracked exclusively in the PR record.
type agenticRevision struct {
	HeadSHA      string    `json:"head_sha"`
	BaseBranch   string    `json:"base_branch"`
	ObservedAt   time.Time `json:"observed_at"`
	RevisionID   string    `json:"revision_id"`
	CommentFloor int       `json:"comment_floor"`
	PlanFloor    int       `json:"plan_floor,omitempty"`
	Inactive     bool      `json:"inactive,omitempty"`
}

func (a *agenticController) ensureRevision(gate *github.CheckRun, state *agenticState, pr *github.PullRequest, comments []github.IssueComment) (*agenticState, error) {
	floor := 0
	for _, comment := range comments {
		floor = max(floor, comment.ID)
	}
	if state.LegacyImport {
		isBot, err := a.gh.BotUserChecker()
		if err != nil {
			return state, err
		}
		var marker *github.IssueComment
		for i := range comments {
			comment := &comments[i]
			if isBot(comment.User.Login) && strings.Contains(comment.Body, agenticRevisionMarker) && (marker == nil || comment.ID > marker.ID) {
				marker = comment
			}
		}
		if marker != nil {
			var revision agenticRevision
			if err := parseAgenticMetadata(marker.Body, agenticRevisionMarker, &revision); err != nil {
				return state, fmt.Errorf("invalid legacy revision marker: %w", err)
			}
			if revision.RevisionID == "" || revision.HeadSHA == "" || revision.BaseBranch == "" || revision.ObservedAt.IsZero() {
				return state, fmt.Errorf("invalid legacy revision identity")
			}
			if revision.Inactive || revision.HeadSHA != state.HeadSHA || revision.BaseBranch != state.BaseBranch || revision.RevisionID != state.RevisionID {
				fresh := newAgenticState(state.Org, state.Repo, pr, a.currentTime().Truncate(time.Second))
				fresh.RevisionID, fresh.record = agenticID(state.RevisionID, revision.RevisionID, fresh.RevisionID), state.record
				fresh.PlanNotBefore = &fresh.ObservedAt
				fresh.record.State, state = fresh, fresh
			}
		}
		state.LegacyImport = false
	}
	if !state.record.persisted || state.RevisionPending {
		if state.RevisionPending {
			// Imported revisions already have bound comment cursors.
			// A sequential same-SHA owner's cached failure may be stale.
			state.record.Reopen = state.record.Reopen || gate.ID != 0
			state.LastCommandID = max(state.LastCommandID, floor)
			if state.PlanNotBefore != nil {
				state.PlanCommentFloor, state.PlanNotBefore = floor, &state.ObservedAt
			}
			state.RevisionPending = false
		}
		if err := a.saveState(gate, state, "", "Tracking the current HEAD/base; waiting for dispatch prerequisites."); err != nil {
			return state, err
		}
	}
	return state, nil
}

func (a *agenticController) invalidateDeparture(org, repo string, pr *github.PullRequest) error {
	r, err := a.readRecord(context.Background(), agenticWork{org: org, repo: repo, number: pr.Number})
	if err != nil {
		return err
	}
	if r == nil {
		if !a.hasRepo(org, repo) {
			return nil
		}
		// A first post-upgrade event can observe departure before importing
		// the old GitHub journal. Persist the boundary without creating a
		// gate on the ordinary branch; reentry must start a fresh revision.
		state := newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
		state.Inactive, state.RevisionPending = true, false
		return a.writeRecord(context.Background(), &agenticRecord{State: state})
	}
	if r.State.Inactive && r.Desired == nil {
		return nil
	}
	r.State.Inactive, r.State.PendingDispatch, r.Wakeup = true, false, nil
	gate := r.Gate
	return a.saveState(&gate, r.State, "failure", "PR left agentic enrollment; returning requires a fresh selection and authorization.")
}

func (a *agenticController) recoverCommands(gate *github.CheckRun, state *agenticState, cfg RepoConfig, pr *github.PullRequest, comments []github.IssueComment) error {
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	for _, comment := range comments {
		if comment.ID <= state.LastCommandID || comment.CreatedAt.Before(state.ObservedAt) || comment.CreatedAt.IsZero() || comment.UpdatedAt.After(comment.CreatedAt) {
			continue
		}
		if err := a.recordCommand(gate, state, comment); err != nil {
			return err
		}
		if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
			return err
		}
	}
	return nil
}
