package main

import (
	"context"
	"sort"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

func (a *agenticController) ensureRevision(gate *github.CheckRun, state *agenticState, comments []github.IssueComment) error {
	if !state.RevisionPending {
		return nil
	}
	floor := 0
	for _, comment := range comments {
		floor = max(floor, comment.ID)
	}
	// A sequential same-SHA owner's cached failure may be stale.
	state.record.Reopen = state.record.Reopen || gate.ID != 0
	state.LastCommandID = max(state.LastCommandID, floor)
	if state.PlanNotBefore != nil {
		state.PlanCommentFloor, state.PlanNotBefore = floor, &state.ObservedAt
	}
	state.RevisionPending = false
	return a.saveState(gate, state, "", "Tracking the current HEAD/base; waiting for dispatch prerequisites.")
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
		// Persist departure without a gate on the ordinary branch; reentry
		// must start a fresh revision.
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
		if err := a.recordCommand(gate, state, pr, comment, comments); err != nil {
			return err
		}
		if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
			return err
		}
	}
	return nil
}
