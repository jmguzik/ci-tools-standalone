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
	pipelineCheckClient
	pipelineCommandClient
	GetPullRequests(org, repo string) ([]github.PullRequest, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
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
	Job       string `json:"job"`
	Context   string `json:"context"`
	Requested bool   `json:"requested,omitempty"`
}

type agenticDispatch struct {
	ID                string             `json:"id"`
	ContextsPublished bool               `json:"contexts_published,omitempty"`
	Posted            bool               `json:"posted,omitempty"`
	Executions        []agenticExecution `json:"executions"`
}

// The per-PR filesystem record is authoritative. No dispatch is allowed
// before its selection and comment request have been saved there. Hook owns
// ProwJob creation; posting its scheduling comment completes dispatch.
type agenticState struct {
	Version            int                                 `json:"version"`
	Org                string                              `json:"org"`
	Repo               string                              `json:"repo"`
	Number             int                                 `json:"number"`
	HeadSHA            string                              `json:"head_sha"`
	BaseBranch         string                              `json:"base_branch"`
	ObservedAt         time.Time                           `json:"observed_at"`
	RevisionID         string                              `json:"revision_id"`
	Inactive           bool                                `json:"inactive,omitempty"`
	PlanNotBefore      *time.Time                          `json:"plan_not_before,omitempty"`
	PlanCommentFloor   int                                 `json:"plan_comment_floor,omitempty"`
	LastCommandID      int                                 `json:"last_command_id,omitempty"`
	ManualRequestID    int                                 `json:"manual_request_id,omitempty"`
	ForceRequestID     int                                 `json:"force_request_id,omitempty"`
	DispatchOverrideID int                                 `json:"dispatch_override_id,omitempty"`
	Command            *agenticCommand                     `json:"command,omitempty"`
	Review             *agenticReviewRequest               `json:"review,omitempty"`
	ReviewPosted       bool                                `json:"review_posted,omitempty"`
	Plan               *agenticSelection                   `json:"plan,omitempty"`
	Frozen             bool                                `json:"frozen,omitempty"`
	ActivatedAt        *time.Time                          `json:"activated_at,omitempty"`
	WaitingSince       *time.Time                          `json:"waiting_since,omitempty"`
	Dispatch           *agenticDispatch                    `json:"dispatch,omitempty"`
	FirstStage         map[string]firstStageSuccessWitness `json:"first_stage,omitempty"`
	PendingDispatch    bool                                `json:"pending_dispatch,omitempty"`
	RevisionPending    bool                                `json:"revision_pending,omitempty"`
	record             *agenticRecord
}

type agenticController struct {
	gh          agenticGitHubClient
	reader      ctrlruntimeclient.Reader // shared ProwJob cache
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
				if check.Name == pipelineGate && check.App.ID == a.appID && check.ExternalID == pipelineExternalID(org, repo, pr.Number) && (gate == nil || check.ID > gate.ID) {
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
	return findPipelineCheck(a.gh, a.appID, org, repo, pr.Head.SHA)
}

func validateAgenticState(state *agenticState) error {
	if state.ManualRequestID < 0 || state.ManualRequestID > state.LastCommandID || state.ForceRequestID < 0 || state.ForceRequestID > state.ManualRequestID || state.PlanCommentFloor < 0 || state.DispatchOverrideID < 0 || state.DispatchOverrideID > state.LastCommandID {
		return fmt.Errorf("invalid persisted request identity")
	}
	for name, witness := range state.FirstStage {
		if name == "" || witness.Context == "" || witness.Context == pipelineGate {
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
		if job.Name == "" || job.Context == "" || job.Context == pipelineGate || jobs[job.Name] != "" || contexts[job.Context] {
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
	if state.DispatchOverrideID > 0 && conclusion == "" {
		conclusion, summary = "success", dispatchOverrideSummary(state.DispatchOverrideID)
	}
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
	next := github.CheckRun{Name: pipelineGate, HeadSHA: state.HeadSHA, ExternalID: pipelineExternalID(state.Org, state.Repo, state.Number), Status: status, Conclusion: conclusion,
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
	if gate.ID == 0 && state.DispatchOverrideID == 0 && (state.ActivatedAt == nil || status != "in_progress") {
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
	if err := writePipelineCheck(a.gh, a.appID, state.Org, state.Repo, gate, next, r.Reopen); err != nil {
		return err
	}
	r.Gate, r.Dirty, r.Reopen = snapshotAgenticGate(*gate), false, false
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
	if gate.ID == 0 || gate.ExternalID == pipelineExternalID(state.Org, state.Repo, state.Number) {
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
	var comments []github.IssueComment
	dispatched := agenticDispatchComplete(state)
	commentsLoaded := state.RevisionPending || (state.Command != nil && !state.Command.Applied) ||
		(!dispatched && (state.PendingDispatch || (!state.Frozen && state.ActivatedAt != nil) ||
			(state.Review != nil && !state.ReviewPosted) || (state.Dispatch != nil && !state.Dispatch.Posted))) ||
		(comment != nil && (!dispatched || agenticCommandRE.MatchString(comment.Body)))
	if commentsLoaded {
		comments, err = a.gh.ListIssueComments(org, repo, number)
		if err != nil {
			if dispatched {
				return err // A failed read cannot undo dispatch; retry the new command without changing the gate.
			}
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
	if agenticDispatchComplete(state) {
		state.PendingDispatch, state.WaitingSince = false, nil
		if gate.Status != "completed" || gate.Conclusion != "success" {
			if err := checkCollision(); err != nil {
				return a.failExistingGate(gate, state, err)
			}
		}
		summary := dispatchSuccessSummary
		if state.DispatchOverrideID > 0 {
			summary = dispatchOverrideSummary(state.DispatchOverrideID)
		}
		return a.saveState(gate, state, "success", summary)
	}
	var pjs v1.ProwJobList
	if err := a.reader.List(ctx, &pjs, ctrlruntimeclient.InNamespace(a.config().ProwJobNamespace), ctrlruntimeclient.MatchingLabels{
		kube.OrgLabel: org, kube.RepoLabel: repo, kube.PullLabel: strconv.Itoa(number), kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}); err != nil {
		return a.failExistingGate(gate, state, err)
	}
	latest := latestAgenticJobs(pjs.Items, org, repo, pr)
	if state.FirstStage == nil {
		state.FirstStage = map[string]firstStageSuccessWitness{}
	}
	ready, failed := agenticFirstStageComplete(static, latest, state.FirstStage, pr.Base.Ref)
	if ready && !state.Frozen && !commentsLoaded {
		comments, err = a.gh.ListIssueComments(org, repo, number)
		if err != nil {
			return a.failExistingGate(gate, state, err)
		}
	}
	invalidPlan := a.refreshPlan(state, pr, comments)
	if invalidPlan != nil {
		if _, pending := invalidPlan.(agenticPlanPendingError); !pending {
			return a.failState(gate, state, invalidPlan)
		}
	}
	if state.ActivatedAt == nil && ready && !pr.Draft && (pr.Mergable == nil || *pr.Mergable) {
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
		return a.saveState(gate, state, "", "Waiting for all required first-stage tests to pass.")
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
		a.prepareDispatch(state, latest)
		if err := a.saveState(gate, state, "", "Dispatching selected jobs."); err != nil {
			return err
		}
	}
	return a.dispatch(gate, state, static, comments, checkCollision)
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

func agenticDispatchComplete(state *agenticState) bool {
	return state.DispatchOverrideID > 0 || (state.Dispatch != nil && state.Dispatch.Posted && state.Dispatch.ID == agenticDispatchID(state))
}

func (a *agenticController) prepareDispatch(state *agenticState, latest map[string]*v1.ProwJob) {
	force := state.ForceRequestID > 0
	dispatch := &agenticDispatch{ID: agenticDispatchID(state), Executions: []agenticExecution{}}
	for _, job := range state.Plan.Jobs {
		execution := agenticExecution{Job: job.Name, Context: job.Context}
		pj := latest[job.Name]
		execution.Requested = force || pj == nil || pj.Spec.Context != job.Context
		dispatch.Executions = append(dispatch.Executions, execution)
	}
	state.Dispatch = dispatch
	state.Frozen = true
	state.PendingDispatch = true
	state.WaitingSince = nil
}

func (a *agenticController) dispatch(gate *github.CheckRun, state *agenticState, static []config.Presubmit, comments []github.IssueComment, ensureFreshCollision func() error) error {
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
			body := fmt.Sprintf("Scheduling selected tests for `%s` → `%s`:%s\n\n<!-- pipeline-controller:dispatch:%s -->", state.HeadSHA, state.BaseBranch, testCommands(commands), state.Dispatch.ID)
			exists, err := a.hasOwnedComment(comments, body)
			if err != nil {
				return a.failState(gate, state, err)
			}
			if !exists {
				if !state.Dispatch.ContextsPublished {
					if err := publishPendingContexts(a.gh, state.Org, state.Repo, state.HeadSHA, commands); err != nil {
						return a.failState(gate, state, err)
					}
					state.Dispatch.ContextsPublished = true
					if err := a.writeRecord(context.Background(), state.record); err != nil {
						return err // Never repeat status publication after the comment can run jobs.
					}
				}
				current, err := a.gh.GetPullRequest(state.Org, state.Repo, state.Number)
				if err != nil {
					return a.failState(gate, state, err)
				}
				cfg, enabled := a.repoConfig(state.Org, state.Repo, current.Base.Ref)
				if current.Head.SHA != state.HeadSHA || current.Base.Ref != state.BaseBranch || current.State != github.PullRequestStateOpen || current.Draft || !enabled || !agenticAuthorized(cfg, state, current) || (current.Mergable != nil && !*current.Mergable) {
					return nil
				}
				if err := a.gh.CreateComment(state.Org, state.Repo, state.Number, body); err != nil {
					return a.failState(gate, state, err)
				}
			}
		}
		state.Dispatch.Posted = true
	}
	state.PendingDispatch = false
	return a.saveState(gate, state, "success", dispatchSuccessSummary)
}
