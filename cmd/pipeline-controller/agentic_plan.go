package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	agenticPlanMarker = "Chai test plan"
	agenticGate       = "ci/tests-dispatched"
	agenticSkipLabel  = "pipeline-skip-agent-review"
)

// agenticPlan is parsed directly from the visible Markdown, without a second
// machine-readable job list. Issue comments carry no commit identity: the
// heading binds the decision to the HEAD/base Chai actually analyzed.
type agenticPlan struct {
	HeadSHA    string
	BaseBranch string
	Jobs       []string
	Rationale  string
	RequestID  string
}

var (
	agenticPlanHeaderRE = regexp.MustCompile("^Chai test plan for `([0-9a-f]{40})` (?:→|->) `([^`[:space:]]+)`$")
	agenticPlanJobRE    = regexp.MustCompile("^- `([^`[:space:]]+)`$")
	agenticRequestRE    = regexp.MustCompile("^Request: `([0-9a-f]{32})`$")
)

func isAgenticPlanComment(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), agenticPlanMarker)
}

func parseAgenticPlan(body string) (agenticPlan, error) {
	var plan agenticPlan
	if len(body) > 64*1024 {
		return plan, fmt.Errorf("comment exceeds 64 KiB")
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")), "\n")
	header := agenticPlanHeaderRE.FindStringSubmatch(lines[0])
	if header == nil {
		return plan, fmt.Errorf("expected Chai test plan for `<full HEAD SHA>` → `<target branch>`")
	}
	plan.HeadSHA, plan.BaseBranch = header[1], header[2]
	empty := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case agenticPlanJobRE.MatchString(line):
			if empty || plan.Rationale != "" {
				return plan, fmt.Errorf("job bullets must be one list before the optional reason")
			}
			plan.Jobs = append(plan.Jobs, agenticPlanJobRE.FindStringSubmatch(line)[1])
		case line == "None.":
			if plan.Jobs != nil || plan.Rationale != "" {
				return plan, fmt.Errorf("empty selection cannot be combined with a job list")
			}
			empty, plan.Jobs = true, []string{}
		case agenticRequestRE.MatchString(line):
			if plan.RequestID != "" {
				return plan, fmt.Errorf("duplicate request ID")
			}
			plan.RequestID = agenticRequestRE.FindStringSubmatch(line)[1]
		case strings.HasPrefix(line, "Reason: "):
			if plan.Rationale != "" {
				return plan, fmt.Errorf("duplicate reason")
			}
			plan.Rationale = strings.TrimSpace(strings.TrimPrefix(line, "Reason: "))
			if plan.Rationale == "" {
				return plan, fmt.Errorf("reason must not be blank")
			}
		default:
			return plan, fmt.Errorf("unrecognized plan line: use job bullets, None., Request or Reason")
		}
	}
	if plan.Jobs == nil {
		return plan, fmt.Errorf("missing job list; use None. for an explicit empty selection")
	}
	return plan, nil
}

type agenticReviewRequest struct {
	HeadSHA    string `json:"head_sha"`
	BaseBranch string `json:"base_branch"`
	RequestID  string `json:"request_id"`
}

type agenticJob struct {
	Name     string `json:"name"`
	Context  string `json:"context"`
	Optional bool   `json:"optional,omitempty"`
}

func formatAgenticReview(request agenticReviewRequest) string {
	return fmt.Sprintf("Chai test selection requested for `%s` → `%s`.\n\nRequest: `%s`", request.HeadSHA, request.BaseBranch, request.RequestID)
}

func trustedAgenticAuthor(authors []string, user github.User) bool {
	for _, author := range authors {
		if strings.EqualFold(author, user.Login) {
			return true
		}
	}
	return false
}

func agenticSecondStage(p config.Presubmit) bool {
	return !p.AlwaysRun && p.RunIfChanged == "" && p.SkipIfOnlyChanged == ""
}

func agenticAllowedJob(p config.Presubmit) bool {
	if !agenticSecondStage(p) {
		return false
	}
	run, hasRun := p.Annotations["pipeline_run_if_changed"]
	skip, hasSkip := p.Annotations["pipeline_skip_if_only_changed"]
	return run != "" || skip != "" || (!p.Optional && !hasRun && !hasSkip)
}

func resolveAgenticJobs(names []string, static []config.Presubmit, branch string) ([]agenticJob, error) {
	if names == nil {
		return nil, fmt.Errorf("jobs must be an explicit list (use None. for an empty selection)")
	}
	if len(names) > 256 {
		return nil, fmt.Errorf("selection exceeds 256 jobs")
	}
	jobs := make([]agenticJob, 0, len(names))
	seenNames, seenContexts := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		if seenNames[name] {
			return nil, fmt.Errorf("duplicate job %q", name)
		}
		seenNames[name] = true
		var matches []config.Presubmit
		for _, p := range static {
			if p.Name == name && p.CouldRun(branch) {
				matches = append(matches, p)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("job %q must resolve to one static presubmit for branch %q", name, branch)
		}
		p := matches[0]
		if !agenticAllowedJob(p) || p.SkipReport || p.Context == "" || p.Context == agenticGate {
			return nil, fmt.Errorf("job %q must be a reporting second-stage presubmit", name)
		}
		if seenContexts[p.Context] {
			return nil, fmt.Errorf("duplicate selected context %q", p.Context)
		}
		for _, other := range static {
			if other.Name != p.Name && other.CouldRun(branch) && other.Context == p.Context && !other.SkipReport {
				return nil, fmt.Errorf("context %q is shared by multiple branch-eligible jobs", p.Context)
			}
		}
		seenContexts[p.Context] = true
		jobs = append(jobs, agenticJob{Name: p.Name, Context: p.Context, Optional: p.Optional})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

// normalAgenticSelection uses the ordinary protected/pipeline-annotation rules
// only for explicit opt-out or timeout. A Chai plan never calls this selector.
func normalAgenticSelection(static []config.Presubmit, pr *github.PullRequest, ghc minimalGhClient, org, repo string) ([]agenticJob, error) {
	changes := config.NewGitHubDeferredChangedFilesProvider(ghc, org, repo, pr.Number)
	names := []string{}
	for _, p := range static {
		if !agenticAllowedJob(p) || !p.CouldRun(pr.Base.Ref) || p.SkipReport {
			continue
		}
		shouldRun, err := pipelineAnnotationMatches(p, changes)
		if err != nil {
			return nil, err
		}
		if shouldRun {
			names = append(names, p.Name)
		}
	}
	return resolveAgenticJobs(names, static, pr.Base.Ref)
}

func matchesAgenticPull(pj *v1.ProwJob, org, repo string, pr *github.PullRequest) bool {
	refs := pj.Spec.Refs
	return pj.Spec.Type == v1.PresubmitJob && refs != nil && refs.Org == org && refs.Repo == repo &&
		refs.BaseRef == pr.Base.Ref && len(refs.Pulls) == 1 && refs.Pulls[0].Number == pr.Number && refs.Pulls[0].SHA == pr.Head.SHA
}

// Creation timestamps have second precision; names cannot establish run order.
// Within a tie, failure wins over pending, and either wins over success.
func preferAgenticJob(candidate, current *v1.ProwJob) bool {
	if current == nil || candidate.CreationTimestamp.After(current.CreationTimestamp.Time) {
		return true
	}
	if !candidate.CreationTimestamp.Equal(&current.CreationTimestamp) {
		return false
	}
	priority := func(state v1.ProwJobState) int {
		switch state {
		case v1.SuccessState:
			return 0
		case v1.FailureState, v1.ErrorState, v1.AbortedState:
			return 2
		default:
			return 1
		}
	}
	return priority(candidate.Status.State) > priority(current.Status.State)
}

func latestAgenticJobs(pjs []v1.ProwJob, org, repo string, pr *github.PullRequest) map[string]*v1.ProwJob {
	latest := map[string]*v1.ProwJob{}
	for i := range pjs {
		pj := &pjs[i]
		if !matchesAgenticPull(pj, org, repo, pr) {
			continue
		}
		old := latest[pj.Spec.Job]
		if preferAgenticJob(pj, old) {
			latest[pj.Spec.Job] = pj
		}
	}
	return latest
}

// All applicable required first-stage jobs must pass; second-stage runs do not count.
func agenticFirstStageComplete(static []config.Presubmit, latest map[string]*v1.ProwJob, witnesses map[string]firstStageSuccessWitness, branch string) (ready, failed bool) {
	ready = true
	for _, p := range static {
		if !p.ContextRequired() || agenticSecondStage(p) || !p.CouldRun(branch) {
			continue
		}
		pj := latest[p.Name]
		// Conditional first-stage jobs absent at this revision do not block completion.
		_, witnessed := witnesses[p.Name]
		if !p.AlwaysRun && pj == nil && !witnessed {
			continue
		}
		if pj != nil && (pj.Status.State == v1.FailureState || pj.Status.State == v1.ErrorState || pj.Status.State == v1.AbortedState) {
			failed = true
		}
		if pj != nil && pj.Spec.Context != p.Context {
			delete(witnesses, p.Name)
			ready = false
		} else if !firstStageJobPassed(p, pj, true, witnesses) {
			ready = false
		}
	}
	return ready, failed
}
