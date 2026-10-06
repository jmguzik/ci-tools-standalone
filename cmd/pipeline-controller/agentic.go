package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/util/workqueue"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
)

type agenticGitHubClient interface {
	minimalGhClient
	GetPullRequests(org, repo string) ([]github.PullRequest, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	ListCheckRuns(org, repo, ref string) (*github.CheckRunList, error)
	CreateCheckRun(org, repo string, check github.CheckRun) (int64, error)
	UpdateCheckRun(org, repo string, id int64, check github.CheckRun) error
	IsMember(org, user string) (bool, error)
	IsCollaborator(org, repo, user string) (bool, error)
	BotUserChecker() (func(string) bool, error)
	RemoveLabel(org, repo string, number int, label string) error
}

type agenticSelection struct {
	Source    string       `json:"source"`
	CommentID int          `json:"comment_id,omitempty"`
	Rationale string       `json:"rationale"`
	Jobs      []agenticJob `json:"jobs"`
}

type agenticCommand struct {
	Command  string `json:"command"`
	Applied  bool   `json:"applied,omitempty"`
	Rejected bool   `json:"rejected,omitempty"`
}

type agenticExecution struct {
	Job       string   `json:"job"`
	Context   string   `json:"context"`
	Previous  []string `json:"previous,omitempty"`
	Requested bool     `json:"requested,omitempty"`
	Passed    bool     `json:"passed,omitempty"`
}

type agenticDispatch struct {
	ID         string             `json:"id"`
	Posted     bool               `json:"posted,omitempty"`
	Executions []agenticExecution `json:"executions"`
}

// The per-PR filesystem record is authoritative. No dispatch is allowed
// before its selection and comment request have been saved there. Hook owns
// ProwJob creation; the controller observes the resulting executions.
type agenticState struct {
	Version          int                                 `json:"version"`
	Org              string                              `json:"org"`
	Repo             string                              `json:"repo"`
	Number           int                                 `json:"number"`
	HeadSHA          string                              `json:"head_sha"`
	BaseBranch       string                              `json:"base_branch"`
	ObservedAt       time.Time                           `json:"observed_at"`
	RevisionID       string                              `json:"revision_id"`
	Inactive         bool                                `json:"inactive,omitempty"`
	PlanNotBefore    *time.Time                          `json:"plan_not_before,omitempty"`
	PlanCommentFloor int                                 `json:"plan_comment_floor,omitempty"`
	LastCommandID    int                                 `json:"last_command_id,omitempty"`
	ManualRequestID  int                                 `json:"manual_request_id,omitempty"`
	ForceRequestID   int                                 `json:"force_request_id,omitempty"`
	Command          *agenticCommand                     `json:"command,omitempty"`
	Review           *agenticReviewRequest               `json:"review,omitempty"`
	ReviewPosted     bool                                `json:"review_posted,omitempty"`
	Plan             *agenticSelection                   `json:"plan,omitempty"`
	Frozen           bool                                `json:"frozen,omitempty"`
	ActivatedAt      *time.Time                          `json:"activated_at,omitempty"`
	WaitingSince     *time.Time                          `json:"waiting_since,omitempty"`
	Dispatch         *agenticDispatch                    `json:"dispatch,omitempty"`
	FirstStage       map[string]firstStageSuccessWitness `json:"first_stage,omitempty"`
	PendingDispatch  bool                                `json:"pending_dispatch,omitempty"`
	RevisionPending  bool                                `json:"revision_pending,omitempty"`
	record           *agenticRecord
}

type agenticController struct {
	gh          agenticGitHubClient
	reader      ctrlruntimeclient.Reader // shared ProwJob cache
	apiReader   ctrlruntimeclient.Reader // live snapshot only for forced-rerun exclusions
	config      config.Getter
	watcher     *watcher
	lgtmWatcher *watcher
	appID       int64
	dryRun      bool
	now         func() time.Time
	logger      *logrus.Entry
	options     agenticOptions
	mu          sync.Mutex
	queueMu     sync.Mutex // short event/queue transitions, never GitHub I/O
	queue       workqueue.TypedRateLimitingInterface[agenticWork]
	queueCtx    context.Context
	inputs      map[agenticWork]*agenticInput // protected by queueMu
	retryAt     map[agenticWork]time.Time     // protected by queueMu; server cooldowns only
	store       *agenticStore                 // protected by mu; process lock retained until shutdown
	stopped     bool                          // protected by mu; late events cannot reopen after shutdown
	maintenance *time.Timer                   // protected by mu; one local record-expiry timer
}

func (a *agenticController) repoConfig(org, repo, branch string) (RepoConfig, bool) {
	if a == nil {
		return RepoConfig{}, false
	}
	if a.watcher != nil {
		if cfg, ok := a.watcher.getConfig()[org][repo]; ok && isBranchEnabled(cfg.Branches, branch) {
			return cfg, cfg.Agentic.enabled()
		}
	}
	if a.lgtmWatcher != nil {
		if cfg, ok := a.lgtmWatcher.getConfig()[org][repo]; ok && isBranchEnabled(cfg.Branches, branch) {
			cfg.Trigger = "lgtm"
			return cfg, cfg.Agentic.enabled()
		}
	}
	return RepoConfig{}, false
}

func (a *agenticController) currentTime() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func agenticID(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))[:32]
}

func agenticExternalID(org, repo string, number int) string {
	return fmt.Sprintf("pipeline-controller:%s/%s#%d", org, repo, number)
}

func newAgenticState(org, repo string, pr *github.PullRequest, now time.Time) *agenticState {
	return &agenticState{Version: agenticStateVersion, Org: org, Repo: repo, Number: pr.Number, HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref, ObservedAt: now,
		RevisionID: agenticID(org, repo, strconv.Itoa(pr.Number), pr.Head.SHA, pr.Base.Ref, now.Format(time.RFC3339Nano)), RevisionPending: true}
}

func (a *agenticController) loadState(org, repo string, pr *github.PullRequest) (*github.CheckRun, *agenticState, error) {
	r, err := a.readRecord(context.Background(), agenticWork{org: org, repo: repo, number: pr.Number})
	if err != nil {
		if a.store == nil || a.stopped {
			return nil, nil, err // No owned lock: never publish another writer's gate.
		}
		// An unreadable local record must not leave an owned successful gate
		// green. This exceptional lookup never replaces the record.
		checks, lookupErr := a.gh.ListCheckRuns(org, repo, pr.Head.SHA)
		if lookupErr != nil {
			return nil, nil, errors.Join(err, lookupErr)
		}
		var gate *github.CheckRun
		if checks != nil {
			for i := range checks.CheckRuns {
				check := &checks.CheckRuns[i]
				if check.Name == agenticGate && check.App.ID == a.appID && check.ExternalID == agenticExternalID(org, repo, pr.Number) && (gate == nil || check.ID > gate.ID) {
					gate = check
				}
			}
		}
		return gate, nil, err
	}
	if r != nil {
		state := r.State
		gate := r.Gate.checkRun(a.appID)
		if state.HeadSHA == pr.Head.SHA && state.BaseBranch == pr.Base.Ref && !state.Inactive {
			return &gate, state, nil
		}
		fresh := newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
		fresh.RevisionID = agenticID(state.RevisionID, fresh.RevisionID)
		fresh.LastCommandID, fresh.PlanCommentFloor = state.LastCommandID, state.PlanCommentFloor
		fresh.PlanNotBefore, fresh.record = &fresh.ObservedAt, r
		r.State, r.Dirty = fresh, false
		if state.HeadSHA == pr.Head.SHA && gate.ID != 0 {
			return &gate, fresh, nil
		}
		// A return to an earlier SHA must reopen its existing gate, not create
		// another same-name run beside an old success.
		r.Gate = agenticGateSnapshot{}
		found, err := a.findGate(org, repo, pr)
		if err != nil {
			return found, nil, err
		}
		r.Gate = snapshotAgenticGate(*found)
		if !state.Inactive && state.HeadSHA != pr.Head.SHA && found.ID == 0 {
			// A plan already posted for a never-observed HEAD is still bound
			// by its SHA/base heading. Reused gates and inactive returns need
			// a fresh plan instead; commands always retain their boundary.
			fresh.PlanNotBefore, fresh.PlanCommentFloor = nil, 0
		}
		return found, fresh, nil
	}
	gate, err := a.findGate(org, repo, pr)
	if err != nil {
		return gate, nil, err
	}
	state := newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
	if gate.ID != 0 || (a.store != nil && a.store.requireFreshPlan) {
		state.PlanNotBefore = &state.ObservedAt
	}
	r = &agenticRecord{State: state, Gate: snapshotAgenticGate(*gate)}
	state.record = r
	return gate, state, nil
}

// Used only when the local record has no gate for this SHA.
func (a *agenticController) findGate(org, repo string, pr *github.PullRequest) (*github.CheckRun, error) {
	checks, err := a.gh.ListCheckRuns(org, repo, pr.Head.SHA)
	if err != nil {
		return nil, err
	}
	gate := &github.CheckRun{}
	if checks == nil {
		return nil, fmt.Errorf("empty check-run response")
	}
	for _, check := range checks.CheckRuns {
		if check.Name == agenticGate && check.App.ID == a.appID && check.ID > gate.ID {
			copy := check
			gate = &copy
		}
	}
	return gate, nil
}

func validateAgenticState(state *agenticState) error {
	if state.ManualRequestID < 0 || state.ManualRequestID > state.LastCommandID || state.ForceRequestID < 0 || state.ForceRequestID > state.ManualRequestID || state.PlanCommentFloor < 0 {
		return fmt.Errorf("invalid persisted request identity")
	}
	for name, witness := range state.FirstStage {
		if name == "" || witness.Context == "" || witness.Context == agenticGate {
			return fmt.Errorf("invalid first-stage witness")
		}
	}
	if state.LastCommandID < 0 || (state.Command != nil && (state.LastCommandID <= 0 || !validAgenticCommand(state.Command.Command))) {
		return fmt.Errorf("invalid persisted command")
	}
	if state.Review != nil && (state.Review.HeadSHA != state.HeadSHA || state.Review.BaseBranch != state.BaseBranch || state.Review.RequestID == "") {
		return fmt.Errorf("invalid persisted review request")
	}
	if state.Plan == nil {
		if state.Dispatch != nil || state.Frozen {
			return fmt.Errorf("dispatch without a selection")
		}
		return nil
	}
	if state.Plan.Jobs == nil || (state.Plan.Source != "chai" && state.Plan.Source != "timeout" && state.Plan.Source != "opt-out") {
		return fmt.Errorf("invalid persisted selection")
	}
	jobs, contexts := map[string]string{}, map[string]bool{}
	for _, job := range state.Plan.Jobs {
		if job.Name == "" || job.Context == "" || job.Context == agenticGate || jobs[job.Name] != "" || contexts[job.Context] {
			return fmt.Errorf("invalid persisted job/context list")
		}
		jobs[job.Name], contexts[job.Context] = job.Context, true
	}
	if state.Dispatch == nil {
		return nil
	}
	if !state.Frozen || state.Dispatch.ID == "" || state.Dispatch.Executions == nil || len(state.Dispatch.Executions) != len(jobs) {
		return fmt.Errorf("invalid persisted dispatch")
	}
	seen := map[string]bool{}
	for _, execution := range state.Dispatch.Executions {
		if jobs[execution.Job] != execution.Context || seen[execution.Job] {
			return fmt.Errorf("invalid persisted execution")
		}
		seen[execution.Job] = true
	}
	return nil
}

func (a *agenticController) saveState(gate *github.CheckRun, state *agenticState, conclusion, summary string) error {
	status := "queued"
	if state.ActivatedAt != nil {
		status = "in_progress"
	}
	if conclusion != "" {
		status = "completed"
	}
	summary = fmt.Sprintf("Pipeline for `%s` → `%s`.\n\n%s", state.HeadSHA, state.BaseBranch, summary)
	if state.Plan != nil {
		summary += fmt.Sprintf("\n\nSelection: %s; %d jobs. %s", state.Plan.Source, len(state.Plan.Jobs), state.Plan.Rationale)
	}
	if state.Frozen {
		summary += "\n\nTest selection is locked for this commit. Later Chai plans are ignored; push a new commit to change the selection."
	}
	next := github.CheckRun{Name: agenticGate, HeadSHA: state.HeadSHA, ExternalID: agenticExternalID(state.Org, state.Repo, state.Number), Status: status, Conclusion: conclusion,
		Output: github.CheckRunOutput{Title: "Pipeline", Summary: summary}}
	if state.Plan == nil && status == "in_progress" {
		next.Output.Title = "Waiting for Chai"
	}
	if state.ActivatedAt != nil {
		next.StartedAt = state.ActivatedAt.Format(time.RFC3339)
	}
	r := state.record
	if r == nil {
		return fmt.Errorf("agentic state has no persistent record")
	}
	r.Reopen = r.Reopen || (status != "completed" && (gate.Conclusion == "success" || r.Dirty))
	r.State, r.Gate = state, snapshotAgenticGate(*gate)
	r.Gate.ExternalID = next.ExternalID // Commit-scoped gate ownership can transfer after collision validation.
	// Creation is the Chai wake-up. Never consume it on a queued check or an
	// early error: GitHub does not emit queued -> in_progress updates.
	if gate.ID == 0 && (state.ActivatedAt == nil || status != "in_progress") {
		r.Dirty = false
		return a.writeRecord(context.Background(), r)
	}
	if !r.Dirty && !r.Reopen && gate.ID != 0 && gate.ExternalID == next.ExternalID && gate.Status == next.Status &&
		(gate.Conclusion == next.Conclusion || (status != "completed" && gate.Conclusion != "success")) && gate.Output.Summary == next.Output.Summary {
		return a.writeRecord(context.Background(), r)
	}
	r.Dirty = true
	if err := a.writeRecord(context.Background(), r); err != nil {
		return err // Dispatch and publication must never precede durable intent.
	}
	if r.Reopen && gate.ID != 0 {
		if err := a.retirePreviousOwners(context.Background(), state, gate.ID); err != nil {
			return err
		}
	}
	if gate.ID == 0 {
		// An earlier create may have succeeded despite a lost response. This
		// lookup is exceptional: records with an ID never list CheckRuns.
		existing, err := a.findGate(state.Org, state.Repo, &github.PullRequest{Number: state.Number, Head: github.PullRequestBranch{SHA: state.HeadSHA}, Base: github.PullRequestBranch{Ref: state.BaseBranch}})
		if err != nil {
			return err
		}
		if existing.ID != 0 {
			*gate = *existing
			if status != "completed" && gate.Conclusion == "success" && !r.Reopen {
				r.Reopen = true
				if err := a.writeRecord(context.Background(), r); err != nil {
					return err
				}
				if err := a.retirePreviousOwners(context.Background(), state, gate.ID); err != nil {
					return err
				}
			}
		}
	}
	if gate.ID == 0 {
		id, err := a.gh.CreateCheckRun(state.Org, state.Repo, next)
		if err != nil {
			return err
		}
		next.ID = id
	} else {
		// Keep one CheckRun: Tide can prefer an older successful same-name run
		// over a new pending one. Remove the old success before reopening, even
		// when the client cannot express conclusion:null in the following PATCH.
		if r.Reopen && status != "completed" {
			if err := a.gh.UpdateCheckRun(state.Org, state.Repo, gate.ID, github.CheckRun{Status: "completed", Conclusion: "failure", Output: next.Output}); err != nil {
				return err
			}
		}
		if err := a.gh.UpdateCheckRun(state.Org, state.Repo, gate.ID, next); err != nil {
			return err
		}
		next.ID = gate.ID
	}
	next.App.ID = a.appID
	*gate = next
	r.Gate, r.Dirty, r.Reopen = snapshotAgenticGate(next), false, false
	return a.writeRecord(context.Background(), r)
}

func (a *agenticController) failState(gate *github.CheckRun, state *agenticState, err error) error {
	if agenticRetryFor(err).githubRateLimit {
		return err // Do not immediately spend another request on a rate-limited API.
	}
	if saveErr := a.saveState(gate, state, "failure", err.Error()); saveErr != nil {
		return errors.Join(err, fmt.Errorf("recording failure: %w", saveErr))
	}
	return err
}

func (a *agenticController) failExistingGate(gate *github.CheckRun, state *agenticState, err error) error {
	if agenticRetryFor(err).githubRateLimit {
		return err
	}
	if gate.ID == 0 || gate.ExternalID == agenticExternalID(state.Org, state.Repo, state.Number) {
		return a.failState(gate, state, err)
	}
	// A collision closes the shared gate without overwriting its owner's plan.
	// Update the owner's projection too, so clearing the collision republishes.
	records, listErr := a.listRecords(context.Background(), agenticWork{org: state.Org, repo: state.Repo, sha: state.HeadSHA})
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	for _, owner := range records {
		if owner.State.Org == state.Org && owner.State.Repo == state.Repo && owner.Gate.ID == gate.ID {
			owned := owner.Gate.checkRun(a.appID)
			return a.failState(&owned, owner.State, err)
		}
	}
	if writeErr := a.gh.UpdateCheckRun(state.Org, state.Repo, gate.ID, github.CheckRun{Status: "completed", Conclusion: "failure", Output: github.CheckRunOutput{Title: "Dispatch blocked", Summary: err.Error(), Text: gate.Output.Text}}); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return err
}

// Called with mu held. A deadline is returned only while actually waiting for
// Chai, never merely because first-stage jobs or a trigger are still pending.
func (a *agenticController) reconcilePull(ctx context.Context, org, repo string, number int, comment *github.IssueComment, deadline *time.Time) error {
	pr, err := a.gh.GetPullRequest(org, repo, number)
	if err != nil {
		return err
	}
	cfg, enabled := a.repoConfig(org, repo, pr.Base.Ref)
	if pr.State != github.PullRequestStateOpen || pr.Head.SHA == "" || pr.Base.Ref == "" {
		if a.dryRun {
			return nil
		}
		return a.deleteRecord(ctx, org, repo, number)
	}
	if a.dryRun {
		a.logger.WithFields(logrus.Fields{"org": org, "repo": repo, "pr": number}).Info("Dry run: skipping agentic mutations")
		return nil
	}
	if a.appID <= 0 {
		if !enabled {
			return nil
		}
		return fmt.Errorf("agentic selection requires GitHub App authentication")
	}
	if !enabled {
		return a.invalidateDeparture(org, repo, pr)
	}
	gate, state, err := a.loadState(org, repo, pr)
	if err != nil {
		// Do not turn an unreadable record into a fresh, empty selection. Close
		// our own check, preserving the malformed data for diagnosis.
		if gate != nil && gate.ID != 0 {
			if writeErr := a.gh.UpdateCheckRun(org, repo, gate.ID, github.CheckRun{Status: "completed", Conclusion: "failure", Output: github.CheckRunOutput{Title: "Cannot recover dispatch", Summary: err.Error(), Text: gate.Output.Text}}); writeErr != nil {
				return errors.Join(err, fmt.Errorf("recording failure: %w", writeErr))
			}
		}
		return err
	}
	// Recheck on reconciliation too: enrollment can change after startup.
	if err := a.options.validateEnabled(); err != nil {
		return a.failExistingGate(gate, state, err)
	}
	static := a.config().GetPresubmitsStatic(org + "/" + repo)
	var pjs v1.ProwJobList
	listOptions := []ctrlruntimeclient.ListOption{ctrlruntimeclient.InNamespace(a.config().ProwJobNamespace), ctrlruntimeclient.MatchingLabels{
		kube.OrgLabel: org, kube.RepoLabel: repo, kube.PullLabel: strconv.Itoa(number), kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}}
	if err := a.reader.List(ctx, &pjs, listOptions...); err != nil {
		return a.failExistingGate(gate, state, err)
	}
	latest := latestAgenticJobs(pjs.Items, org, repo, pr)
	if state.FirstStage == nil {
		state.FirstStage = map[string]firstStageSuccessWitness{}
	}
	eligible, ready, failed := agenticFirstStageProgress(static, latest, state.FirstStage, pr.Base.Ref)
	var comments []github.IssueComment
	if state.RevisionPending || state.PendingDispatch || (!state.Frozen && (state.ActivatedAt != nil || eligible)) || comment != nil ||
		(state.Command != nil && !state.Command.Applied) || (state.Review != nil && !state.ReviewPosted) ||
		(state.Dispatch != nil && !state.Dispatch.Posted) {
		comments, err = a.gh.ListIssueComments(org, repo, number)
		if err != nil {
			return a.failExistingGate(gate, state, err)
		}
	}
	// CheckRuns are commit-scoped, so two enrolled PRs with the same HEAD
	// cannot safely own independent instances of this required gate.
	collisionFresh := false
	checkCollision := func() error {
		if collisionFresh {
			return nil
		}
		prs, err := a.gh.GetPullRequests(org, repo)
		if err != nil {
			return err
		}
		collisionFresh = true
		for _, other := range prs {
			if other.Number == number || other.Head.SHA != pr.Head.SHA {
				continue
			}
			if _, enabled := a.repoConfig(org, repo, other.Base.Ref); enabled {
				return fmt.Errorf("PRs #%d and #%d share a commit-scoped dispatch gate; use distinct HEAD commits", number, other.Number)
			}
		}
		return nil
	}
	// Gate ownership changes and dispatch transitions require a fresh live
	// collision check. Stable waiting events need no repository listing.
	if gate.ID != 0 && state.RevisionPending {
		if err := checkCollision(); err != nil {
			// Preserve the owner's record, including frozen selections/executions.
			return a.failExistingGate(gate, state, err)
		}
	}
	if err := a.ensureRevision(gate, state, comments); err != nil {
		return a.failState(gate, state, err)
	}
	// Finish already-recorded side effects before accepting a later command.
	if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	if comment != nil {
		if err := a.explainUnboundCommand(state, *comment, comments); err != nil {
			return err
		}
		if err := a.recordCommand(gate, state, pr, *comment, comments); err != nil {
			return err
		}
	}
	if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	invalidPlan := a.refreshPlan(state, pr, comments)
	if invalidPlan != nil {
		if _, pending := invalidPlan.(agenticPlanPendingError); !pending {
			return a.failState(gate, state, invalidPlan)
		}
	}
	if state.ActivatedAt == nil && eligible && !pr.Draft && (pr.Mergable == nil || *pr.Mergable) {
		if err := checkCollision(); err != nil {
			return a.failExistingGate(gate, state, err)
		}
		now := a.currentTime()
		state.ActivatedAt = &now
		state.PendingDispatch = true
		if state.Plan == nil {
			state.WaitingSince = &now
		}
		// Persist activation before the one-shot creation event is emitted.
		if err := a.saveState(gate, state, "", "Waiting for Chai's selected jobs (bounded by the configured timeout)."); err != nil {
			return err
		}
	}
	if state.ActivatedAt == nil {
		state.PendingDispatch = false
		return a.saveState(gate, state, "", "Waiting for at least half of first-stage tests to pass without failures.")
	}
	if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	if state.Plan == nil || invalidPlan != nil {
		if state.WaitingSince == nil {
			started := *state.ActivatedAt
			state.WaitingSince = &started
		}
		if a.currentTime().Sub(*state.WaitingSince) >= a.options.timeout {
			jobs, err := normalAgenticSelection(static, pr, a.gh, org, repo)
			if err != nil {
				return a.failState(gate, state, err)
			}
			state.Plan = &agenticSelection{Source: "timeout", Jobs: jobs, Rationale: "Chai timed out; normal selection is locked for this HEAD/base."}
			state.WaitingSince = nil
			state.PendingDispatch = true
			// Persist the fallback decision before doing anything that can create
			// executions. Late replies cannot change it, even after a restart.
			if err := a.saveState(gate, state, "", "Chai timed out; preparing normal selection."); err != nil {
				return err
			}
		} else {
			*deadline = state.WaitingSince.Add(a.options.timeout)
			state.PendingDispatch = false
			if invalidPlan != nil {
				if err := a.saveState(gate, state, "failure", invalidPlan.Error()); err != nil {
					return err // Transient persistence failures still need recovery.
				}
				return invalidPlan
			}
			if failed {
				return a.saveState(gate, state, "failure", "First-stage tests failed; the selection deadline remains unchanged.")
			}
			return a.saveState(gate, state, "", "Waiting for Chai's selected jobs (bounded by the configured timeout).")
		}
	} else {
		state.WaitingSince = nil
	}
	if pr.Draft || (pr.Mergable != nil && !*pr.Mergable) || !ready {
		state.PendingDispatch = false
		if failed {
			return a.saveState(gate, state, "failure", "First-stage tests failed.")
		}
		return a.saveState(gate, state, "", "Waiting for first-stage success and the configured trigger.")
	}
	if !agenticAuthorized(cfg, state, pr) {
		state.PendingDispatch = false
		return a.saveState(gate, state, "", "Test selection is ready; waiting for the configured trigger.")
	}
	prepare := state.Dispatch == nil || state.Dispatch.ID != agenticDispatchID(state)
	if prepare || !state.Dispatch.Posted {
		names := make([]string, 0, len(state.Plan.Jobs))
		for _, job := range state.Plan.Jobs {
			names = append(names, job.Name)
		}
		resolved, err := resolveAgenticJobs(names, static, state.BaseBranch)
		if err != nil || !reflect.DeepEqual(resolved, state.Plan.Jobs) {
			return a.failState(gate, state, errors.Join(errors.New("selected job definitions changed; refusing to dispatch"), err))
		}
	}
	if prepare {
		if err := checkCollision(); err != nil {
			return a.failState(gate, state, err)
		}
		existing := pjs.Items
		if state.ForceRequestID > 0 {
			// Informer lag must not let a pre-existing green run satisfy a
			// forced rerun. Only this generation boundary needs a live list.
			var snapshot v1.ProwJobList
			if err := a.apiReader.List(ctx, &snapshot, listOptions...); err != nil {
				return a.failState(gate, state, err)
			}
			existing = append(existing, snapshot.Items...)
		}
		a.prepareDispatch(state, pr, latest, existing)
		if err := a.saveState(gate, state, "", "Dispatching selected jobs; waiting for their results."); err != nil {
			return err
		}
	}
	return a.dispatch(gate, state, pr, pjs.Items, static, comments, checkCollision)
}

func hasAgenticLabel(pr *github.PullRequest, name string) bool {
	for _, label := range pr.Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func agenticAuthorized(cfg RepoConfig, state *agenticState, pr *github.PullRequest) bool {
	return cfg.Trigger == "auto" || (cfg.Trigger == "lgtm" && (hasAgenticLabel(pr, "lgtm") || hasAgenticLabel(pr, PipelineAutoLabel))) || state.ManualRequestID > 0
}

func (a *agenticController) refreshPlan(state *agenticState, pr *github.PullRequest, comments []github.IssueComment) error {
	if state.Frozen {
		return nil
	}
	static := a.config().GetPresubmitsStatic(state.Org + "/" + state.Repo)
	if hasAgenticLabel(pr, agenticSkipLabel) {
		if state.Plan == nil || state.Plan.Source != "opt-out" {
			jobs, err := normalAgenticSelection(static, pr, a.gh, state.Org, state.Repo)
			if err != nil {
				return err
			}
			state.Plan = &agenticSelection{Source: "opt-out", Jobs: jobs, Rationale: "Normal selection requested by pipeline-skip-agent-review."}
		}
	} else if state.Plan == nil || state.Plan.Source == "chai" {
		if err := a.readPlan(state, static, comments); err != nil {
			state.Plan = nil // A newer invalid plan must not authorize an older selection.
			return agenticPlanPendingError{err}
		}
	}
	return nil
}

func (a *agenticController) readPlan(state *agenticState, static []config.Presubmit, comments []github.IssueComment) error {
	comments = slices.Clone(comments)
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID > comments[j].ID })
	for _, comment := range comments {
		if !trustedAgenticAuthor(a.options.trustedAuthors.Strings(), comment.User) || !isAgenticPlanComment(comment.Body) {
			continue
		}
		if comment.ID <= state.PlanCommentFloor {
			continue
		}
		if state.PlanNotBefore != nil && comment.CreatedAt.Before(*state.PlanNotBefore) {
			continue
		}
		plan, err := parseAgenticPlan(comment.Body)
		if (plan.HeadSHA != "" && plan.HeadSHA != state.HeadSHA) || (plan.BaseBranch != "" && plan.BaseBranch != state.BaseBranch) {
			continue
		}
		if err != nil {
			return fmt.Errorf("invalid Chai plan in comment %d: %w", comment.ID, err)
		}
		if state.Review != nil && plan.RequestID != state.Review.RequestID {
			continue
		}
		if state.Review == nil && plan.RequestID != "" {
			continue
		}
		jobs, err := resolveAgenticJobs(plan.Jobs, static, state.BaseBranch)
		if err != nil {
			return fmt.Errorf("invalid Chai plan in comment %d: %w", comment.ID, err)
		}
		state.Plan = &agenticSelection{Source: "chai", CommentID: comment.ID, Jobs: jobs, Rationale: plan.Rationale}
		return nil
	}
	return nil
}

func agenticDispatchID(state *agenticState) string {
	return agenticID(state.Org, state.Repo, strconv.Itoa(state.Number), state.HeadSHA, state.BaseBranch, state.RevisionID, strconv.Itoa(state.ManualRequestID))
}

func (a *agenticController) prepareDispatch(state *agenticState, pr *github.PullRequest, latest map[string]*v1.ProwJob, existing []v1.ProwJob) {
	force := state.ForceRequestID > 0
	dispatch := &agenticDispatch{ID: agenticDispatchID(state), Executions: []agenticExecution{}}
	for _, job := range state.Plan.Jobs {
		execution := agenticExecution{Job: job.Name, Context: job.Context}
		if force {
			// Names, not timestamps: Kubernetes creation times have second
			// precision, so a forced rerun must exclude every existing run.
			for _, pj := range existing {
				if pj.Spec.Job == job.Name && matchesAgenticPull(&pj, state.Org, state.Repo, pr) && !slices.Contains(execution.Previous, pj.Name) {
					execution.Previous = append(execution.Previous, pj.Name)
				}
			}
			execution.Requested = true
		} else {
			if state.Dispatch != nil {
				for _, old := range state.Dispatch.Executions {
					if old.Job == job.Name && old.Context == job.Context {
						execution = old // Keep success and any outstanding forced-rerun boundary.
						break
					}
				}
			}
			pj := latest[job.Name]
			present := pj != nil && pj.Spec.Context == job.Context && !slices.Contains(execution.Previous, pj.Name)
			execution.Requested = !present && !execution.Passed
		}
		dispatch.Executions = append(dispatch.Executions, execution)
	}
	state.Dispatch = dispatch
	state.Frozen = true
	state.PendingDispatch = true
	state.WaitingSince = nil
}

func (a *agenticController) dispatch(gate *github.CheckRun, state *agenticState, pr *github.PullRequest, pjs []v1.ProwJob, static []config.Presubmit, comments []github.IssueComment, ensureFreshCollision func() error) error {
	if !state.Dispatch.Posted {
		var commands []config.Presubmit
		for _, execution := range state.Dispatch.Executions {
			if !execution.Requested {
				continue
			}
			for _, definition := range static {
				if definition.Name == execution.Job && definition.CouldRun(state.BaseBranch) {
					commands = append(commands, definition)
					break
				}
			}
		}
		if len(commands) != 0 {
			if err := ensureFreshCollision(); err != nil {
				return a.failState(gate, state, err)
			}
			current, err := a.gh.GetPullRequest(state.Org, state.Repo, state.Number)
			if err != nil {
				return a.failState(gate, state, err)
			}
			cfg, enabled := a.repoConfig(state.Org, state.Repo, current.Base.Ref)
			if current.Head.SHA != state.HeadSHA || current.Base.Ref != state.BaseBranch || current.State != github.PullRequestStateOpen || current.Draft || !enabled || !agenticAuthorized(cfg, state, current) || (current.Mergable != nil && !*current.Mergable) {
				return nil
			}
			body := fmt.Sprintf("Scheduling selected tests for `%s` → `%s`:%s\n\n<!-- pipeline-controller:dispatch:%s -->", state.HeadSHA, state.BaseBranch, testCommands(commands), state.Dispatch.ID)
			if err := a.ensureComment(state, comments, body); err != nil {
				return a.failState(gate, state, err)
			}
		}
		state.Dispatch.Posted = true
	}
	allPassed, failed := true, false
	for i := range state.Dispatch.Executions {
		execution := &state.Dispatch.Executions[i]
		var pj *v1.ProwJob
		for j := range pjs {
			candidate := &pjs[j]
			if !matchesAgenticPull(candidate, state.Org, state.Repo, pr) || candidate.Spec.Job != execution.Job || candidate.Spec.Context != execution.Context || slices.Contains(execution.Previous, candidate.Name) {
				continue
			}
			if preferAgenticJob(candidate, pj) {
				pj = candidate
			}
		}
		if pj != nil {
			execution.Passed = pj.Status.State == v1.SuccessState
			failed = failed || pj.Status.State == v1.FailureState || pj.Status.State == v1.ErrorState || pj.Status.State == v1.AbortedState
		}
		allPassed = allPassed && execution.Passed
	}
	state.PendingDispatch = false
	if allPassed {
		if gate.Status != "completed" || gate.Conclusion != "success" {
			if err := ensureFreshCollision(); err != nil {
				return a.failState(gate, state, err)
			}
		}
		current, err := a.gh.GetPullRequest(state.Org, state.Repo, state.Number)
		if err != nil {
			return a.failState(gate, state, err)
		}
		currentConfig, enabled := a.repoConfig(state.Org, state.Repo, current.Base.Ref)
		if current.Head.SHA != state.HeadSHA || current.Base.Ref != state.BaseBranch || current.State != github.PullRequestStateOpen || current.Draft || !enabled || !agenticAuthorized(currentConfig, state, current) {
			return nil
		}
		return a.saveState(gate, state, "success", "First-stage and all selected second-stage tests passed.")
	}
	if failed {
		return a.saveState(gate, state, "failure", "Selected second-stage tests failed; rerun the failed jobs or use `/pipeline required`.")
	}
	return a.saveState(gate, state, "", "Waiting for all selected second-stage tests to pass.")
}
