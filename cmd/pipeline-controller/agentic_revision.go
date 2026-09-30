package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

// The CheckRun journal lives on a commit; this small edited PR comment records
// which revision the controller is currently observing across pushes/retargets.
// It distinguishes observed A -> B -> A transitions from a restart at A and
// bounds command replay; missed intermediate revisions remain undetected here.
type agenticRevision struct {
	HeadSHA      string    `json:"head_sha"`
	BaseBranch   string    `json:"base_branch"`
	ObservedAt   time.Time `json:"observed_at"`
	RevisionID   string    `json:"revision_id"`
	CommentFloor int       `json:"comment_floor"`
	PlanFloor    int       `json:"plan_floor,omitempty"`
	Inactive     bool      `json:"inactive,omitempty"`
}

func formatAgenticRevision(revision agenticRevision) string {
	metadata, _ := agenticMetadata(agenticRevisionMarker, revision)
	short := revision.HeadSHA
	if len(short) > 12 {
		short = short[:12]
	}
	status := "tracking"
	if revision.Inactive {
		status = "inactive for"
	}
	return fmt.Sprintf("Agentic tests: %s `%s` → `%s`.\n\n%s", status, short, revision.BaseBranch, metadata)
}

func (a *agenticController) ensureRevision(gate *github.CheckRun, state *agenticState, pr *github.PullRequest, comments []github.IssueComment) (*agenticState, error) {
	isBot, err := a.gh.BotUserChecker()
	if err != nil {
		return state, err
	}
	var marker *github.IssueComment
	var floor int
	for i := range comments {
		comment := &comments[i]
		if comment.ID > floor {
			floor = comment.ID
		}
		if isBot(comment.User.Login) && strings.Contains(comment.Body, agenticRevisionMarker) && (marker == nil || comment.ID > marker.ID) {
			marker = comment
		}
	}
	var revision agenticRevision
	if state.RevisionUpdate != nil && !state.resetRevision {
		return state, a.publishRevision(gate, state, marker)
	}
	if marker == nil && gate.ID != 0 && !state.resetRevision {
		// Recreate a deleted marker from the trusted journal. Losing the comment
		// must not erase a frozen selection, timeout decision or execution proof.
		state.RevisionUpdate = &agenticRevision{HeadSHA: state.HeadSHA, BaseBranch: state.BaseBranch, ObservedAt: state.ObservedAt,
			RevisionID: state.RevisionID, CommentFloor: state.LastCommandID, PlanFloor: state.PlanCommentFloor}
		if err := a.saveState(gate, state, "", "Restoring the PR revision marker from the dispatch journal."); err != nil {
			return state, err
		}
		return state, a.publishRevision(gate, state, nil)
	}
	if marker != nil {
		if err := parseAgenticMetadata(marker.Body, agenticRevisionMarker, &revision); err != nil {
			return state, fmt.Errorf("invalid controller revision marker: %w", err)
		}
		if revision.HeadSHA == "" || revision.BaseBranch == "" || revision.ObservedAt.IsZero() || revision.RevisionID == "" || revision.CommentFloor < 0 {
			return state, fmt.Errorf("invalid controller revision identity")
		}
		if revision.HeadSHA == pr.Head.SHA && revision.BaseBranch == pr.Base.Ref && !revision.Inactive && !state.resetRevision {
			if state.RevisionID != revision.RevisionID {
				state = newAgenticState(state.Org, state.Repo, pr, revision.ObservedAt)
				state.RevisionID = revision.RevisionID
				state.LastCommandID, state.PlanCommentFloor = revision.CommentFloor, revision.PlanFloor
				if revision.PlanFloor != 0 {
					state.PlanNotBefore = &revision.ObservedAt
				}
			}
			return state, nil
		}
	}
	// An existing run on this SHA may refer to an older base or an earlier visit
	// to the same SHA. Those plans need a fresh comment, unlike an early plan on
	// a HEAD the controller has never seen before.
	planFloor := 0
	if revision.Inactive || (gate.ID != 0 && (marker != nil || state.resetRevision)) {
		planFloor = floor
	}
	observed := a.currentTime().Truncate(time.Second)
	if marker == nil && gate.ID != 0 {
		// Recover a lost CreateComment response or an initial gate write without
		// moving the revision boundary on every retry.
		observed = state.ObservedAt.Truncate(time.Second)
	}
	id := agenticID(revision.RevisionID, state.Org, state.Repo, pr.Head.SHA, pr.Base.Ref, observed.Format(time.RFC3339Nano))
	if marker == nil && gate.ID != 0 {
		id = state.RevisionID
	}
	revision = agenticRevision{HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref, ObservedAt: observed, RevisionID: id, CommentFloor: floor, PlanFloor: planFloor}
	state = newAgenticState(state.Org, state.Repo, pr, observed)
	state.RevisionID = id
	state.LastCommandID, state.PlanCommentFloor = floor, planFloor
	state.RevisionUpdate = &revision
	if planFloor != 0 {
		state.PlanNotBefore = &observed
	}
	// Close a reused successful gate before changing its PR/base ownership.
	if err := a.saveState(gate, state, "", "Tracking the current HEAD/base; waiting for dispatch prerequisites."); err != nil {
		return state, err
	}
	return state, a.publishRevision(gate, state, marker)
}

// Leaving enrollment invalidates the prior authorization even if the PR later
// returns to the same SHA/base. Record the tombstone in the existing gate before
// editing the PR marker: a failed or deleted comment cannot resurrect old state.
// No gate or ProwJob is created for the ordinary branch.
func (a *agenticController) invalidateDeparture(org, repo string, pr *github.PullRequest) error {
	comments, err := a.gh.ListIssueComments(org, repo, pr.Number)
	if err != nil {
		return err
	}
	isBot, err := a.gh.BotUserChecker()
	if err != nil {
		return err
	}
	var marker *github.IssueComment
	var floor int
	for i := range comments {
		comment := &comments[i]
		if comment.ID > floor {
			floor = comment.ID
		}
		if isBot(comment.User.Login) && strings.Contains(comment.Body, agenticRevisionMarker) && (marker == nil || comment.ID > marker.ID) {
			marker = comment
		}
	}
	var previous agenticRevision
	if marker != nil {
		if err := parseAgenticMetadata(marker.Body, agenticRevisionMarker, &previous); err != nil {
			return fmt.Errorf("cannot invalidate controller revision: %w", err)
		}
		if previous.Inactive {
			return nil // The departure was already durably recorded.
		}
	}
	heads := []string{pr.Head.SHA}
	if previous.HeadSHA != "" && previous.HeadSHA != pr.Head.SHA {
		heads = []string{previous.HeadSHA, pr.Head.SHA}
	}
	for _, head := range heads {
		checks, err := a.gh.ListCheckRuns(org, repo, head)
		if err != nil {
			return err
		}
		if checks == nil {
			return fmt.Errorf("empty check-run response while invalidating departure")
		}
		var gate *github.CheckRun
		for i := range checks.CheckRuns {
			check := &checks.CheckRuns[i]
			if check.Name == agenticGate && check.App.ID == a.appID && check.ExternalID == agenticExternalID(org, repo, pr.Number) && (gate == nil || check.ID > gate.ID) {
				gate = check
			}
		}
		if gate == nil {
			continue // Ordinary PR, or a same-SHA gate owned by another PR.
		}
		var state agenticState
		if err := parseAgenticMetadata(gate.Output.Text, agenticStateMarker, &state); err != nil {
			return fmt.Errorf("cannot invalidate controller gate: %w", err)
		}
		if state.Version != 1 || state.Org != org || state.Repo != repo || state.Number != pr.Number || state.HeadSHA != head || gate.HeadSHA != head || state.RevisionID == "" {
			return fmt.Errorf("invalid controller gate identity during departure")
		}
		if err := validateAgenticState(&state); err != nil {
			return err
		}
		if state.Departure == nil {
			observed := a.currentTime().Truncate(time.Second)
			state.Departure = &agenticRevision{HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref, ObservedAt: observed,
				RevisionID:   agenticID(state.RevisionID, "inactive", pr.Head.SHA, pr.Base.Ref, observed.Format(time.RFC3339Nano)),
				CommentFloor: floor, PlanFloor: floor, Inactive: true}
		}
		if err := a.saveState(gate, &state, "failure", "PR left agentic enrollment; returning requires a fresh selection and authorization."); err != nil {
			return err
		}
		body := formatAgenticRevision(*state.Departure)
		if marker == nil {
			return a.gh.CreateComment(org, repo, pr.Number, body)
		}
		if marker.Body != body {
			return a.gh.EditComment(org, repo, marker.ID, body)
		}
		return nil
	}
	return nil
}

func (a *agenticController) publishRevision(gate *github.CheckRun, state *agenticState, marker *github.IssueComment) error {
	body := formatAgenticRevision(*state.RevisionUpdate)
	var err error
	if marker == nil {
		err = a.gh.CreateComment(state.Org, state.Repo, state.Number, body)
	} else if marker.Body != body {
		err = a.gh.EditComment(state.Org, state.Repo, marker.ID, body)
	}
	if err != nil {
		return err
	}
	state.RevisionUpdate = nil
	return a.saveState(gate, state, "", "Tracking the current HEAD/base; waiting for dispatch prerequisites.")
}

func (a *agenticController) recoverCommands(gate *github.CheckRun, state *agenticState, cfg RepoConfig, pr *github.PullRequest, comments []github.IssueComment) error {
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	for _, comment := range comments {
		// Edited historical prose is not a new command. Commands posted before
		// the controller observed this revision cannot be safely rebound to it.
		if comment.ID <= state.LastCommandID || comment.CreatedAt.Before(state.ObservedAt) || comment.CreatedAt.IsZero() || comment.UpdatedAt.After(comment.CreatedAt) {
			continue
		}
		if err := a.recordCommand(gate, state, cfg, pr, comment); err != nil {
			return err
		}
		if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
			return err
		}
	}
	return nil
}
