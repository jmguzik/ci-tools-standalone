package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/labels"
)

// progressPhase describes what is known about dispatch, not the status of tests.
type progressPhase string

const (
	// progressWaiting is the zero value until there is a dispatch observation.
	progressWaiting progressPhase = ""
	// progressChai means the check was published and completion is still pending.
	progressChai progressPhase = "Agentic dispatching"
	// progressPosted records a traditional test request that was posted.
	progressPosted progressPhase = "Tests dispatched"
	// progressChaiMarked retains who acknowledged a gate if the route changes later.
	progressChaiMarked progressPhase = "Chai marked dispatch complete"
	// progressEmpty means the traditional planner requested no new jobs.
	progressEmpty progressPhase = "Nothing new to dispatch"
	// progressMarked records a mark without inferring who sent it.
	progressMarked progressPhase = "Gate acknowledged"
	// progressComplete is a recovered result without a fresh dispatch observation.
	progressComplete progressPhase = "Dispatch complete"
	// progressFailed covers errors where some requests may already have gone through.
	progressFailed progressPhase = "Couldn't confirm dispatch"
)

// progressTrigger is a milestone, never the current stage.
type progressTrigger string

const (
	// progressStageOne is the normal automatic trigger.
	progressStageOne progressTrigger = "Stage 1 ready"
	// progressLGTM represents approval, not first-stage readiness.
	progressLGTM progressTrigger = "LGTM"
	// progressAuto records opting out of waiting for approval.
	progressAuto progressTrigger = "Auto enabled"
	// progressManual represents an explicit dispatch or handoff request.
	progressManual progressTrigger = "Manual request"
)

// progressView contains only observations the scheduler already made.
type progressView struct {
	trigger   progressTrigger
	satisfied bool
	phase     progressPhase
	chai      bool
	recovery  string
	replied   bool
	deadline  time.Time
}

// waitingProgress describes the current trigger without running the readiness check again.
func waitingProgress(cfg RepoConfig, pr *github.PullRequest, chai bool) progressView {
	v := progressView{trigger: progressStageOne, chai: chai}
	switch cfg.Trigger {
	case "manual":
		v.trigger = progressManual
	case "lgtm":
		v.trigger, v.satisfied = progressLGTM, hasAgenticLabel(pr, labels.LGTM)
		if hasAgenticLabel(pr, PipelineAutoLabel) {
			v.trigger, v.satisfied = progressAuto, true
		}
	}
	return v
}

// renderProgress formats the first bot comment without executable slash-command lines.
func renderProgress(refs *v1.Refs, v progressView) string {
	now, next, hint := v.copy()
	if v.recovery != "" {
		now = v.recovery + " " + now
	}
	if v.recovery != "" && !v.chai {
		hint += " A trusted user can re-enable Chai with `/pipeline agentic-mode`. If Traditional owns this commit and has already dispatched, Chai starts on the next push."
	}
	checks := fmt.Sprintf("[**Checks**](https://github.com/%s/%s/pull/%d/checks)", url.PathEscape(refs.Org), url.PathEscape(refs.Repo), refs.Pulls[0].Number)
	next = strings.ReplaceAll(next, "**Checks**", checks)
	owner, middle := "Traditional", ""
	if v.chai {
		owner, middle = "Chai", ` --> handoff["Agentic dispatching"]`
	}
	current, end := "wait", string(progressPosted)
	switch v.phase {
	case progressWaiting:
	case progressChai:
		current = "handoff"
	case progressChaiMarked:
		current = "outcome"
	default:
		current, end = "outcome", string(v.phase)
	}
	milestone := ""
	if v.satisfied {
		milestone = "  class trigger done\n"
	}
	sha := refs.Pulls[0].SHA
	sha = html.EscapeString(strings.NewReplacer("`", "&#96;", "\n", "", "\r", "").Replace(sha[:min(12, len(sha))]))
	return fmt.Sprintf("%s\n**Pipeline · %s** · `%s`\n\n**Now:** %s\n\n**Next:** %s\n\n"+
		"```mermaid\nflowchart LR\n  wait[\"Waiting\"] --> trigger[%q]%s --> outcome[%q]\n"+
		"  classDef here fill:#1757a6,stroke:#1757a6,color:#fff,stroke-width:3px\n"+
		"  classDef done fill:#dafbe1,stroke:#1a7f37,color:#116329,stroke-width:2px\n"+
		"  class %s here\n%s```\n\n**If you're stuck:** %s\n\nUse `/test ?` to list jobs and `/pipeline help` for commands.",
		progressPrefix, owner, sha, now, next, v.trigger, middle, end, current, milestone, hint)
}

// copy keeps the usual next action separate from the always-visible recovery path.
func (v progressView) copy() (now, next, hint string) {
	checks := "Open **Checks** to see which jobs started and how they finished."
	if v.phase == progressFailed {
		return "The controller couldn't confirm dispatch.", "Check for existing runs before retrying.", "Use `/test <job>` to request one missing job."
	}
	if v.phase == progressEmpty {
		return "No new jobs needed to be requested.", "Open **Checks** for any existing runs.", "Use `/test <job>` if you need to run a specific job."
	}
	if v.phase == progressMarked || v.phase == progressComplete {
		return "The dispatch gate is complete. This does not confirm that jobs started.", checks, "Use `/test <job>` to request one missing job."
	}
	if v.phase == progressChaiMarked {
		hint = "Use `/test <job>` for one missing run."
		if v.chai {
			hint += " To switch to Traditional, a trusted user can run `/pipeline skip-agentic-mode`."
		}
		return "Chai marked dispatch complete.", checks, hint
	}
	if v.chai {
		if v.phase == progressChai {
			hint = "A trusted user can switch to Traditional with `/pipeline skip-agentic-mode`."
			if !v.deadline.IsZero() {
				return "Waiting for Chai to reply.", "If Chai doesn't reply before the timeout, Traditional takes over.", hint
			}
			if v.replied {
				return "Chai replied, but the gate is still pending. There is no automatic fallback after a reply.", "Check Chai's latest comment and look for runs in **Checks**.", hint
			}
			return "The Chai gate is pending. No response deadline was recovered.", "Check Chai's latest comment and **Checks**.", hint
		}
		hint = "A trusted user can notify Chai now with `/pipeline required`."
		if v.trigger == progressManual {
			return "Waiting for a request to notify Chai.", "A trusted user can use `/pipeline remaining` or `/pipeline required`.", "To use Traditional instead, a trusted user can run `/pipeline skip-agentic-mode`."
		}
		if v.trigger == progressLGTM && !v.satisfied {
			return "Waiting for `/lgtm`.", "Ask a reviewer for `/lgtm`.", "A trusted user can use `/pipeline auto` to wait only for stage 1, or `/pipeline required` to notify Chai now."
		}
		return "Waiting for stage 1 before notifying Chai.", "Open **Checks** to see what's pending or failed.", hint
	}
	switch {
	case v.phase == progressPosted:
		return "The controller posted stage-2 test requests.", checks, "Use `/test <job>` for one failed or missing run."
	case v.trigger == progressManual:
		return "Waiting for a request to run stage 2.", "Use `/pipeline remaining` to request missing jobs.", "Use `/pipeline required` for the full applicable set, or `/test <job>` for one job."
	case v.trigger == progressLGTM && !v.satisfied:
		return "Waiting for `/lgtm`.", "Ask a reviewer for `/lgtm`.", "Use `/pipeline auto` to wait for stage 1 instead, or `/pipeline remaining` for missing jobs now."
	default:
		return "Waiting for stage 1.", "Open **Checks** to see what's pending or failed.", "Use `/pipeline remaining` to request missing stage-2 jobs now."
	}
}

// submitProgress keeps the feature off unless the matching configuration enables it.
func (p *progressPublisher) submitProgress(cfg RepoConfig, refs *v1.Refs, v progressView, follow, baseline bool) {
	if p == nil || !cfg.ProgressComment || refs == nil || len(refs.Pulls) != 1 || refs.Pulls[0].SHA == "" {
		return
	}
	p.submit(refs, renderProgress(refs, v), follow, baseline)
}

// recordedProgress conservatively interprets a persisted check without fetching job runs or comments.
func recordedProgress(gate github.CheckRun) progressPhase {
	if gate.Status != "completed" {
		return progressWaiting
	}
	if gate.Conclusion != "success" {
		return progressFailed
	}
	if strings.HasPrefix(gate.Output.Summary, "Dispatch completion acknowledged with ") {
		return progressMarked
	}
	return progressComplete
}

// agenticProgress stores display observations that cannot be inferred from a check run alone.
type agenticProgress struct {
	trigger progressTrigger
	outcome progressPhase
	replied bool
}

// publishProgress projects agentic state after reconciliation without changing any scheduling decisions.
func (a *agenticController) publishProgress(work agenticWork, pr *github.PullRequest, cfg RepoConfig, s *agenticState) {
	traditional := s.traditionalMode()
	v := waitingProgress(cfg, pr, !traditional)
	if s.timedOut {
		v.recovery = "Chai did not reply before fallback; Traditional took over."
	} else if s.optOut {
		v.recovery = "Traditional scheduling is enabled for this PR."
	} else if s.traditional {
		v.recovery = "Traditional owns this commit. Chai is enabled for the next push."
	}
	v.phase = s.progress.outcome
	if v.phase == progressWaiting && s.dispatched {
		v.phase = recordedProgress(s.gate)
		if v.phase == progressWaiting {
			v.phase = progressComplete
		}
	}
	if v.phase == progressWaiting && !traditional && s.ready {
		v.phase, v.deadline, v.replied = progressChai, s.waitingUntil, s.progress.replied
	}
	if s.progress.trigger != "" && v.phase != progressWaiting {
		v.trigger, v.satisfied = s.progress.trigger, true
	}
	if v == s.lastProgress {
		return
	}
	s.lastProgress = v
	a.progress.submitProgress(cfg, pullRefs(work.org, work.repo, pr), v, true, false)
}

// handleProgressRevision resets the traditional display on pushes or retargets.
// The agentic worker independently projects its live revision.
func (cw *clientWrapper) handleProgressRevision(logger *logrus.Entry, event github.PullRequestEvent) {
	if cw.progress == nil {
		return
	}
	pr := &event.PullRequest
	org, repo := event.Repo.Owner.Login, event.Repo.Name
	switch event.Action {
	case github.PullRequestActionSynchronize:
	case github.PullRequestActionEdited:
		var change struct{ Base json.RawMessage }
		if err := json.Unmarshal(event.Changes, &change); err != nil || len(change.Base) == 0 {
			return
		}
	case github.PullRequestActionClosed, github.PullRequestActionConvertedToDraft:
		cw.progress.submit(pullRefs(org, repo, pr), "", true, false)
		return
	default:
		return
	}
	cfg, _ := pipelineBranchConfig(cw.watcher, cw.lgtmWatcher, org, repo, pr.Base.Ref)
	refs := pullRefs(org, repo, pr)
	if !cfg.ProgressComment || pr.Draft || pr.State != github.PullRequestStateOpen {
		cw.progress.submit(refs, "", true, false)
		return
	}
	if cfg.Agentic.enabled() || cw.progress.knowsRevision(refs) {
		return
	}
	// Only an opt-in change of revision needs verification. A delayed webhook
	// must not replace a dispatched view with Waiting for an obsolete commit.
	current, err := cw.ghc.GetPullRequest(org, repo, pr.Number)
	if err != nil {
		logger.WithError(err).Debug("Could not verify pipeline progress revision")
		return
	}
	if current == nil || current.State != github.PullRequestStateOpen || current.Draft || current.Head.SHA != pr.Head.SHA || current.Base.Ref != pr.Base.Ref {
		return
	}
	cw.progress.submitProgress(cfg, refs, waitingProgress(cfg, current, false), true, true)
}
