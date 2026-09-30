package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
	"sigs.k8s.io/prow/pkg/pjutil"
)

type agenticGitHubClient interface {
	minimalGhClient
	GetPullRequests(org, repo string) ([]github.PullRequest, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	ListCheckRuns(org, repo, ref string) (*github.CheckRunList, error)
	CreateCheckRun(org, repo string, check github.CheckRun) (int64, error)
	UpdateCheckRun(org, repo string, id int64, check github.CheckRun) error
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	ListStatuses(org, repo, ref string) ([]github.Status, error)
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
	ID      int    `json:"id"`
	Command string `json:"command"`
	Applied bool   `json:"applied,omitempty"`
}

type agenticExecution struct {
	Job      string `json:"job"`
	Context  string `json:"context"`
	Name     string `json:"prowjob"`
	Adopted  bool   `json:"adopted,omitempty"`
	Reported bool   `json:"reported,omitempty"`
	URL      string `json:"url,omitempty"`
}

type agenticDispatch struct {
	ID         string             `json:"id"`
	BaseSHA    string             `json:"base_sha"`
	Executions []agenticExecution `json:"executions"`
}

type agenticFirstStageWitness struct {
	Context string `json:"context"`
	ProwJob string `json:"prowjob"`
	URL     string `json:"url"`
}

// The per-PR filesystem record is authoritative. No dispatch is allowed
// before its exact execution names have been saved there. A single active
// controller replica serializes transitions; deterministic names additionally
// make retries safe after a lost Kubernetes or GitHub response.
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
	WaitingSince     *time.Time                          `json:"waiting_since,omitempty"`
	Dispatch         *agenticDispatch                    `json:"dispatch,omitempty"`
	FirstStage       map[string]agenticFirstStageWitness `json:"first_stage,omitempty"`
	StatusInterests  []string                            `json:"status_interests,omitempty"`
	PendingDispatch  bool                                `json:"pending_dispatch,omitempty"`
	RevisionPending  bool                                `json:"revision_pending,omitempty"`
	LegacyImport     bool                                `json:"legacy_import,omitempty"`
	record           *agenticRecord
}

type agenticController struct {
	gh          agenticGitHubClient
	jobs        ctrlruntimeclient.Client
	reader      ctrlruntimeclient.Reader // uncached: dispatch recovery must see newly created jobs
	config      config.Getter
	watcher     *watcher
	lgtmWatcher *watcher
	appID       int64
	dryRun      bool
	now         func() time.Time
	logger      *logrus.Entry
	options     agenticOptions
	mu          sync.Mutex
	scheduler   *agenticScheduler // protected by mu; only deadlines and transient-error retries
	store       *agenticStore     // protected by mu; process lock retained until shutdown
	stopped     bool              // protected by mu; late events cannot reopen after shutdown
	maintenance *time.Timer       // protected by mu; one local record-expiry timer

	statusContexts map[agenticWork]map[string]bool // positive interests only; protected by mu
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
	return &agenticState{Version: 1, Org: org, Repo: repo, Number: pr.Number, HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref, ObservedAt: now,
		RevisionID: agenticID(org, repo, strconv.Itoa(pr.Number), pr.Head.SHA, pr.Base.Ref, now.Format(time.RFC3339Nano)), RevisionPending: true}
}

func (a *agenticController) loadState(org, repo string, pr *github.PullRequest) (*github.CheckRun, *agenticState, error) {
	r, err := a.readRecord(context.Background(), agenticWork{org: org, repo: repo, number: pr.Number})
	if err != nil {
		if a.store == nil || a.stopped {
			return nil, nil, err // No owned lock: never publish another writer's gate.
		}
		// An unreadable local record must not leave an owned successful gate
		// green. This exceptional lookup never imports or replaces the record.
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
		gate := r.Gate
		if state.HeadSHA == pr.Head.SHA && state.BaseBranch == pr.Base.Ref && !state.Inactive {
			return &gate, state, nil
		}
		fresh := newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
		fresh.RevisionID = agenticID(state.RevisionID, fresh.RevisionID)
		fresh.LastCommandID, fresh.PlanCommentFloor = state.LastCommandID, state.PlanCommentFloor
		fresh.PlanNotBefore, fresh.record = &fresh.ObservedAt, r
		r.State, r.Desired, r.Wakeup = fresh, nil, nil
		if state.HeadSHA == pr.Head.SHA && gate.ID != 0 {
			return &gate, fresh, nil
		}
		// A return to an earlier SHA must reopen its existing gate, not create
		// another same-name run beside an old success.
		r.Gate = github.CheckRun{}
		found, _, err := a.findGate(org, repo, pr)
		if err != nil {
			return found, nil, err
		}
		r.Gate = *found
		if !state.Inactive && state.HeadSHA != pr.Head.SHA && found.ID == 0 {
			// A plan already posted for a never-observed HEAD is still bound
			// by its SHA/base heading. Reused gates and inactive returns need
			// a fresh plan instead; commands always retain their boundary.
			fresh.PlanNotBefore, fresh.PlanCommentFloor = nil, 0
		}
		return found, fresh, nil
	}
	gate, imported, err := a.findGate(org, repo, pr)
	if err != nil {
		return gate, nil, err
	}
	state := imported
	if state == nil {
		state = newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
		if gate.ID != 0 || (a.store != nil && a.store.legacyImportRetired) {
			state.PlanNotBefore = &state.ObservedAt
		}
	}
	r = &agenticRecord{State: state, Gate: *gate}
	state.record = r
	return gate, state, nil
}

// Used only when the local record has no gate for this SHA. Old GitHub journals
// are imported lazily on an actual PR event, never in a startup migration scan.
func (a *agenticController) findGate(org, repo string, pr *github.PullRequest) (*github.CheckRun, *agenticState, error) {
	checks, err := a.gh.ListCheckRuns(org, repo, pr.Head.SHA)
	if err != nil {
		return nil, nil, err
	}
	gate := &github.CheckRun{}
	if checks == nil {
		return nil, nil, fmt.Errorf("empty check-run response")
	}
	for _, check := range checks.CheckRuns {
		if check.Name == agenticGate && check.App.ID == a.appID && check.ID > gate.ID {
			copy := check
			gate = &copy
		}
	}
	if gate.ID == 0 {
		return gate, nil, nil
	}
	if gate.ExternalID != agenticExternalID(org, repo, pr.Number) {
		fresh := newAgenticState(org, repo, pr, a.currentTime().Truncate(time.Second))
		fresh.RevisionPending = true
		return gate, fresh, nil // Live collision validation precedes takeover.
	}
	if a.store != nil && a.store.legacyImportRetired {
		// An expired departure boundary must never resurrect authorization
		// from a legacy GitHub journal, even if TTL is later disabled.
		return gate, nil, nil
	}
	if !strings.Contains(gate.Output.Text, agenticStateMarker) {
		return gate, nil, nil // Presentation-only gate; local state was retired.
	}
	var legacy struct {
		agenticState
		RevisionUpdate *agenticRevision `json:"revision_update,omitempty"`
		Departure      *agenticRevision `json:"departure,omitempty"`
	}
	if err := parseAgenticMetadata(gate.Output.Text, agenticStateMarker, &legacy); err != nil {
		return gate, nil, fmt.Errorf("cannot recover controller gate: %w", err)
	}
	state := legacy.agenticState
	if state.Version != 1 || state.Org != org || state.Repo != repo || state.HeadSHA != pr.Head.SHA || gate.HeadSHA != pr.Head.SHA || state.ObservedAt.IsZero() || state.RevisionID == "" || gate.ExternalID != agenticExternalID(org, repo, state.Number) {
		return gate, nil, fmt.Errorf("controller gate has invalid revision identity")
	}
	if state.BaseBranch != pr.Base.Ref || state.Number != pr.Number || legacy.Departure != nil {
		fresh := newAgenticState(org, repo, pr, a.currentTime())
		fresh.RevisionID = agenticID(state.RevisionID, fresh.RevisionID)
		fresh.PlanNotBefore = &fresh.ObservedAt
		fresh.RevisionPending = true
		return gate, fresh, nil
	}
	if err := validateAgenticState(&state); err != nil {
		return gate, nil, fmt.Errorf("cannot recover controller gate: %w", err)
	}
	state.LegacyImport = true
	return gate, &state, nil
}

func validateAgenticState(state *agenticState) error {
	if state.ManualRequestID < 0 || state.ManualRequestID > state.LastCommandID || state.ForceRequestID < 0 || state.ForceRequestID > state.ManualRequestID || state.PlanCommentFloor < 0 {
		return fmt.Errorf("invalid persisted request identity")
	}
	for name, witness := range state.FirstStage {
		if name == "" || witness.Context == "" || witness.Context == agenticGate || witness.ProwJob == "" || witness.URL == "" {
			return fmt.Errorf("invalid first-stage witness")
		}
	}
	if state.LastCommandID < 0 || (state.Command != nil && (state.Command.ID <= 0 || state.Command.ID != state.LastCommandID || !validAgenticCommand(state.Command.Command))) {
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
	if !state.Frozen || state.Dispatch.ID == "" || state.Dispatch.BaseSHA == "" || len(state.Dispatch.Executions) != len(jobs) {
		return fmt.Errorf("invalid persisted dispatch")
	}
	seen := map[string]bool{}
	for _, execution := range state.Dispatch.Executions {
		if execution.Name == "" || jobs[execution.Job] != execution.Context || seen[execution.Job] || (execution.Reported && execution.URL == "") {
			return fmt.Errorf("invalid persisted execution")
		}
		seen[execution.Job] = true
	}
	return nil
}

func (a *agenticController) saveState(gate *github.CheckRun, state *agenticState, conclusion, summary string) error {
	a.rememberStatusContexts(state)
	status := "in_progress"
	if conclusion != "" {
		status = "completed"
	}
	if state.Plan != nil {
		summary += fmt.Sprintf("\n\nSelection: %s; %d jobs. %s", state.Plan.Source, len(state.Plan.Jobs), state.Plan.Rationale)
	}
	if state.Frozen {
		summary += "\n\nTest selection is locked for this commit. Later Chai plans are ignored; push a new commit to change the selection."
	}
	next := github.CheckRun{Name: agenticGate, HeadSHA: state.HeadSHA, ExternalID: agenticExternalID(state.Org, state.Repo, state.Number), Status: status, Conclusion: conclusion,
		Output: github.CheckRunOutput{Title: "Second-stage dispatch", Summary: summary, Text: "Recovery state is stored on the controller's persistent volume."}}
	r := state.record
	if r == nil {
		return fmt.Errorf("agentic state has no persistent record")
	}
	hadPending := r.Desired != nil
	previousSuccess := hadPending && r.Desired.Conclusion == "success"
	r.Reopen = r.Reopen || (status == "in_progress" && (gate.Conclusion == "success" || previousSuccess))
	r.State, r.Gate = state, *gate
	if strings.Contains(r.Gate.Output.Text, agenticStateMarker) {
		r.Gate.Output.Text = ""
	} // Do not duplicate the imported journal.
	r.Gate.ExternalID = next.ExternalID // Commit-scoped gate ownership can transfer after collision validation.
	if !hadPending && !r.Reopen && gate.ID != 0 && gate.ExternalID == next.ExternalID && gate.Status == next.Status && (gate.Conclusion == next.Conclusion || (status == "in_progress" && gate.Conclusion != "success")) &&
		gate.Output.Title == next.Output.Title && gate.Output.Summary == next.Output.Summary && gate.Output.Text == next.Output.Text {
		return a.writeRecord(context.Background(), r)
	}
	r.Desired = &next
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
		existing, _, err := a.findGate(state.Org, state.Repo, &github.PullRequest{Number: state.Number, Head: github.PullRequestBranch{SHA: state.HeadSHA}, Base: github.PullRequestBranch{Ref: state.BaseBranch}})
		if err != nil {
			return err
		}
		if existing.ID != 0 {
			*gate = *existing
			if status == "in_progress" && gate.Conclusion == "success" && !r.Reopen {
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
		if r.Reopen && status == "in_progress" {
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
	r.Gate, r.Desired, r.Reopen = next, nil, false
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
			owned := owner.Gate
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
		// Do not turn an unreadable journal into a fresh, empty selection. Close
		// our own check, preserving the malformed data for diagnosis.
		if gate != nil && gate.ID != 0 {
			if writeErr := a.gh.UpdateCheckRun(org, repo, gate.ID, github.CheckRun{Status: "completed", Conclusion: "failure", Output: github.CheckRunOutput{Title: "Cannot recover dispatch", Summary: err.Error(), Text: gate.Output.Text}}); writeErr != nil {
				return errors.Join(err, fmt.Errorf("recording failure: %w", writeErr))
			}
		}
		return err
	}
	// Recheck on reconciliation too: enrollment can change after startup.
	a.rememberStatusContexts(state)
	if err := a.options.validateEnabled(); err != nil {
		return a.failExistingGate(gate, state, err)
	}
	comments, err := a.gh.ListIssueComments(org, repo, number)
	if err != nil {
		return a.failExistingGate(gate, state, err)
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
	if gate.ID == 0 || state.RevisionPending {
		if err := checkCollision(); err != nil {
			// Preserve the owner's journal, including frozen selections/executions.
			return a.failExistingGate(gate, state, err)
		}
	}
	state, err = a.ensureRevision(gate, state, pr, comments)
	if err != nil {
		return a.failState(gate, state, err)
	}
	// Finish already-recorded side effects before accepting a later command.
	if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	if err := a.recoverCommands(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	if comment != nil {
		if err := a.explainUnboundCommand(state, *comment, comments); err != nil {
			return err
		}
		if err := a.recordCommand(gate, state, *comment); err != nil {
			return err
		}
	}
	if err := a.applyCommand(gate, state, cfg, pr, comments); err != nil {
		return a.failState(gate, state, err)
	}
	static := a.config().GetPresubmitsStatic(org + "/" + repo)
	var invalidPlan error
	if !state.Frozen {
		if hasAgenticLabel(pr, agenticSkipLabel) {
			if state.Plan == nil || state.Plan.Source != "opt-out" {
				jobs, err := normalAgenticSelection(static, pr, a.gh, org, repo)
				if err != nil {
					return a.failState(gate, state, err)
				}
				state.Plan = &agenticSelection{Source: "opt-out", Jobs: jobs, Rationale: "Normal selection requested by pipeline-skip-agent-review."}
			}
		} else if state.Plan == nil || state.Plan.Source == "chai" {
			invalidPlan = a.readPlan(state, static, comments)
		}
	}
	var pjs v1.ProwJobList
	if err := a.reader.List(ctx, &pjs, ctrlruntimeclient.InNamespace(a.config().ProwJobNamespace), ctrlruntimeclient.MatchingLabels{
		kube.OrgLabel: org, kube.RepoLabel: repo, kube.PullLabel: strconv.Itoa(number), kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}); err != nil {
		return a.failState(gate, state, err)
	}
	latest := latestAgenticJobs(pjs.Items, org, repo, pr)
	statuses, err := a.gh.GetCombinedStatus(org, repo, pr.Head.SHA)
	if err != nil {
		return a.failState(gate, state, err)
	}
	if state.FirstStage == nil {
		state.FirstStage = map[string]agenticFirstStageWitness{}
	}
	ready, err := agenticFirstStageReady(static, latest, statuses, state.FirstStage, pr, a.gh, org, repo)
	if err != nil {
		return a.failState(gate, state, err)
	}
	authorized := agenticAuthorized(cfg, state, pr)
	if pr.Draft || (pr.Mergable != nil && !*pr.Mergable) || !ready || !authorized {
		state.WaitingSince = nil
		state.PendingDispatch = false
		return a.saveState(gate, state, "", "Waiting for first-stage success and the configured trigger.")
	}
	if state.Plan == nil || invalidPlan != nil {
		if state.WaitingSince == nil {
			now := a.currentTime()
			state.WaitingSince = &now
		}
		if a.currentTime().Sub(*state.WaitingSince) >= a.options.timeout {
			jobs, err := normalAgenticSelection(static, pr, a.gh, org, repo)
			if err != nil {
				return a.failState(gate, state, err)
			}
			state.Plan = &agenticSelection{Source: "timeout", Jobs: jobs, Rationale: "Chai timed out; normal selection is locked for this HEAD/base."}
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
				return agenticPlanPendingError{invalidPlan}
			}
			return a.saveState(gate, state, "", "Waiting for Chai's selected jobs (bounded by the configured timeout).")
		}
	}
	if state.Dispatch == nil {
		if err := checkCollision(); err != nil {
			return a.failState(gate, state, err)
		}
		if err := a.prepareDispatch(state, static, pr, latest); err != nil {
			return a.failState(gate, state, err)
		}
		if err := a.saveState(gate, state, "", "Dispatching selected jobs; waiting for their own GitHub reports."); err != nil {
			return err
		}
	}
	return a.dispatch(ctx, gate, state, static, pr, statuses, checkCollision)
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

func (a *agenticController) readPlan(state *agenticState, static []config.Presubmit, comments []github.IssueComment) error {
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

func (a *agenticController) prepareDispatch(state *agenticState, static []config.Presubmit, pr *github.PullRequest, latest map[string]*v1.ProwJob) error {
	if pr.Base.SHA == "" {
		return fmt.Errorf("pull request is missing its base commit")
	}
	names := make([]string, 0, len(state.Plan.Jobs))
	for _, job := range state.Plan.Jobs {
		names = append(names, job.Name)
	}
	resolved, err := resolveAgenticJobs(names, static, state.BaseBranch)
	if err != nil || !reflect.DeepEqual(resolved, state.Plan.Jobs) {
		return errors.Join(errors.New("selected job definitions changed; refusing to dispatch"), err)
	}
	requestID, force := "initial", false
	if state.ForceRequestID > 0 {
		requestID, force = strconv.Itoa(state.ForceRequestID), true
	}
	dispatch := &agenticDispatch{ID: agenticID(state.Org, state.Repo, strconv.Itoa(state.Number), state.HeadSHA, state.BaseBranch, state.RevisionID, requestID), BaseSHA: pr.Base.SHA, Executions: []agenticExecution{}}
	for _, job := range state.Plan.Jobs {
		execution := agenticExecution{Job: job.Name, Context: job.Context, Name: "pipeline-" + agenticID(dispatch.ID, job.Name)}
		if pj := latest[job.Name]; !force && pj != nil && pj.Spec.Report && pj.Spec.Context == job.Context {
			execution.Name, execution.Adopted = pj.Name, true
			execution.URL = pj.Status.URL
		}
		dispatch.Executions = append(dispatch.Executions, execution)
	}
	state.Dispatch = dispatch
	state.Frozen = true
	state.PendingDispatch = true
	state.WaitingSince = nil
	return nil
}

func (a *agenticController) dispatch(ctx context.Context, gate *github.CheckRun, state *agenticState, static []config.Presubmit, pr *github.PullRequest, statuses *github.CombinedStatus, ensureFreshCollision func() error) error {
	// Only superseded reports need history. Share one lookup across executions;
	// first-stage readiness continues to use current statuses exclusively.
	var history *github.CombinedStatus
	reportMatches := func(jobContext, url string) (bool, error) {
		if agenticStatusMatches(jobContext, url, statuses) {
			return true, nil
		}
		if url == "" || statuses == nil {
			return false, nil
		}
		for _, status := range statuses.Statuses {
			if status.Context != jobContext || status.TargetURL == url {
				continue
			}
			if history == nil {
				previous, err := a.gh.ListStatuses(state.Org, state.Repo, state.HeadSHA)
				if err != nil {
					return false, fmt.Errorf("reading status history: %w", err)
				}
				history = &github.CombinedStatus{Statuses: previous}
			}
			return agenticStatusMatches(jobContext, url, history), nil
		}
		return false, nil
	}
	allReported := true
	for i := range state.Dispatch.Executions {
		execution := &state.Dispatch.Executions[i]
		if execution.Reported {
			continue // Retain durable evidence after ProwJob garbage collection.
		}
		var pj v1.ProwJob
		key := types.NamespacedName{Namespace: a.config().ProwJobNamespace, Name: execution.Name}
		err := a.reader.Get(ctx, key, &pj)
		if apierrors.IsNotFound(err) && execution.URL != "" {
			reported, reportErr := reportMatches(execution.Context, execution.URL)
			if reportErr != nil {
				return a.failState(gate, state, reportErr)
			}
			if reported {
				execution.Reported = true
				continue
			}
		}
		if apierrors.IsNotFound(err) && !execution.Adopted {
			if err := ensureFreshCollision(); err != nil {
				return a.failState(gate, state, err)
			}
			// Refresh immediately before creating jobs. A stale ProwJob event or
			// plan must never dispatch work on an old revision.
			current, err := a.gh.GetPullRequest(state.Org, state.Repo, state.Number)
			if err != nil {
				return a.failState(gate, state, err)
			}
			if current.Head.SHA != state.HeadSHA || current.Base.Ref != state.BaseBranch || current.State != github.PullRequestStateOpen || current.Draft {
				return nil
			}
			currentConfig, enabled := a.repoConfig(state.Org, state.Repo, current.Base.Ref)
			if !enabled || !agenticAuthorized(currentConfig, state, current) || (current.Mergable != nil && !*current.Mergable) {
				return nil
			}
			var definition *config.Presubmit
			for j := range static {
				candidate := &static[j]
				if candidate.Name == execution.Job && candidate.Context == execution.Context && candidate.CouldRun(state.BaseBranch) && agenticAllowedJob(*candidate) && !candidate.SkipReport {
					definition = candidate
					break
				}
			}
			if definition == nil {
				return a.failState(gate, state, fmt.Errorf("selected job %q is no longer available", execution.Job))
			}
			pj = pjutil.NewPresubmit(*pr, state.Dispatch.BaseSHA, *definition, state.Dispatch.ID, map[string]string{"pipeline.openshift.io/dispatch": state.Dispatch.ID}, pjutil.RequireScheduling(a.config().Scheduler.Enabled))
			pj.Name, pj.Namespace = execution.Name, key.Namespace
			if err := a.jobs.Create(ctx, &pj); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return a.failState(gate, state, fmt.Errorf("creating %s: %w", execution.Job, err))
				}
				if err := a.reader.Get(ctx, key, &pj); err != nil {
					return a.failState(gate, state, err)
				}
			}
		} else if err != nil {
			return a.failState(gate, state, fmt.Errorf("recovering %s: %w", execution.Job, err))
		}
		if !matchesAgenticPull(&pj, state.Org, state.Repo, pr) || pj.Spec.Job != execution.Job || pj.Spec.Context != execution.Context || !pj.Spec.Report ||
			(!execution.Adopted && (pj.Spec.Refs.BaseSHA != state.Dispatch.BaseSHA || pj.Labels["pipeline.openshift.io/dispatch"] != state.Dispatch.ID)) {
			return a.failState(gate, state, fmt.Errorf("ProwJob %s does not match the recorded execution", pj.Name))
		}
		if pj.Status.URL != "" {
			execution.URL = pj.Status.URL
		}
		reported := agenticExecutionReported(&pj, statuses)
		if !reported && pj.Status.PrevReportStates["github-reporter"] != "" {
			reported, err = reportMatches(execution.Context, pj.Status.URL)
			if err != nil {
				return a.failState(gate, state, err)
			}
		}
		execution.Reported = reported
		if !reported {
			allReported = false
		}
	}
	if allReported {
		state.PendingDispatch = false
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
		return a.saveState(gate, state, "success", "All selected executions were dispatched and reported their own contexts. Job results continue to gate merging.")
	}
	state.PendingDispatch = false
	return a.saveState(gate, state, "", "Selected jobs are dispatched; waiting for their own GitHub reports.")
}

func agenticExecutionReported(pj *v1.ProwJob, statuses *github.CombinedStatus) bool {
	// A reporter acknowledgment alone is insufficient: crier may swallow a
	// permanent API error. Conversely an older same-context success is not a
	// report from this execution. Require both and match the execution's URL.
	if pj.Status.URL == "" || pj.Status.PrevReportStates["github-reporter"] == "" || statuses == nil {
		return false
	}
	return agenticStatusMatches(pj.Spec.Context, pj.Status.URL, statuses)
}

func agenticStatusMatches(context, url string, statuses *github.CombinedStatus) bool {
	if statuses == nil || url == "" {
		return false
	}
	for _, status := range statuses.Statuses {
		if status.Context == context && status.TargetURL == url &&
			(status.State == github.StatusPending || status.State == github.StatusSuccess || status.State == github.StatusFailure || status.State == github.StatusError) {
			return true
		}
	}
	return false
}
