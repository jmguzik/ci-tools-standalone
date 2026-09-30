package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	agenticPlanMarker     = "Chai test plan"
	agenticStateMarker    = "<!-- pipeline-controller:state:v1"
	agenticRevisionMarker = "<!-- pipeline-controller:revision:v1"
	agenticGate           = "ci/tests-dispatched"
	agenticSkipLabel      = "pipeline-skip-agent-review"
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

// parseAgenticMetadata reads legacy state/revision metadata during lazy import.
// Chai plans use parseAgenticPlan, not hidden JSON.
func parseAgenticMetadata(body, marker string, into interface{}) error {
	if len(body) > 64*1024 {
		return fmt.Errorf("comment exceeds 64 KiB")
	}
	if strings.Count(body, marker) != 1 {
		return fmt.Errorf("expected exactly one %s metadata block", strings.TrimPrefix(marker, "<!-- "))
	}
	_, rest, _ := strings.Cut(body, marker)
	raw, _, closed := strings.Cut(rest, "-->")
	if !closed {
		return fmt.Errorf("unterminated metadata block")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("invalid metadata: %w", err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("metadata must contain exactly one JSON object")
	}
	return nil
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

func latestAgenticJobs(pjs []v1.ProwJob, org, repo string, pr *github.PullRequest) map[string]*v1.ProwJob {
	latest := map[string]*v1.ProwJob{}
	for i := range pjs {
		pj := &pjs[i]
		if !matchesAgenticPull(pj, org, repo, pr) {
			continue
		}
		old := latest[pj.Spec.Job]
		if old == nil || pj.CreationTimestamp.After(old.CreationTimestamp.Time) ||
			(pj.CreationTimestamp.Equal(&old.CreationTimestamp) && pj.Name > old.Name) {
			latest[pj.Spec.Job] = pj
		}
	}
	return latest
}

// agenticFirstStageReady is deliberately independent of second-stage existence.
// Unlike the legacy duplicate guard, it also works after a partial dispatch.
func agenticFirstStageReady(static []config.Presubmit, latest map[string]*v1.ProwJob, statuses *github.CombinedStatus, witnesses map[string]agenticFirstStageWitness, pr *github.PullRequest, ghc minimalGhClient, org, repo string) (bool, error) {
	changes := config.NewGitHubDeferredChangedFilesProvider(ghc, org, repo, pr.Number)
	ready := true
	for _, p := range static {
		if !p.ContextRequired() || agenticSecondStage(p) || !p.CouldRun(pr.Base.Ref) {
			continue
		}
		required, err := p.ShouldRun(pr.Base.Ref, changes, false, false)
		if err != nil {
			return false, err
		}
		pj := latest[p.Name]
		var status github.Status
		if statuses != nil {
			for _, observed := range statuses.Statuses {
				if observed.Context == p.Context {
					status = observed
					break
				}
			}
		}
		// A manually run conditional first-stage job still has to succeed.
		if required || pj != nil || status.State != "" {
			if pj != nil {
				if pj.Status.State != v1.SuccessState || status.State != github.StatusSuccess || pj.Status.URL == "" || status.TargetURL != pj.Status.URL {
					delete(witnesses, p.Name)
					ready = false
				} else {
					witnesses[p.Name] = agenticFirstStageWitness{Context: p.Context, ProwJob: pj.Name, URL: pj.Status.URL}
				}
			} else {
				// Commit statuses alone are not base-branch-scoped. Only reuse a
				// report previously witnessed for this exact HEAD/base journal.
				witness, ok := witnesses[p.Name]
				if !ok || witness.Context != p.Context || witness.URL == "" || status.State != github.StatusSuccess || status.TargetURL != witness.URL {
					ready = false
				}
			}
		}
	}
	return ready, nil
}
