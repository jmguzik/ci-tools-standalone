package main

import (
	"sigs.k8s.io/prow/pkg/github"
	"sort"
)

// Caller holds mu. Retain interests while records route this SHA: configuration
// changes must not hide a report for an execution already recorded in the journal.
func (a *agenticController) rememberStatusContexts(state *agenticState) {
	if a.statusContexts == nil {
		a.statusContexts = map[agenticWork]map[string]bool{}
	}
	key := agenticWork{org: state.Org, repo: state.Repo, sha: state.HeadSHA}
	contexts := a.statusContexts[key]
	if contexts == nil {
		contexts = map[string]bool{}
		a.statusContexts[key] = contexts
	}
	for _, job := range a.config().GetPresubmitsStatic(state.Org + "/" + state.Repo) {
		if !job.SkipReport {
			contexts[job.Context] = true
		}
	}
	for _, interest := range state.StatusInterests {
		contexts[interest] = true
	}
	for _, witness := range state.FirstStage {
		contexts[witness.Context] = true
	}
	if state.Plan != nil {
		for _, job := range state.Plan.Jobs {
			contexts[job.Context] = true
		}
	}
	if state.Dispatch != nil {
		for _, execution := range state.Dispatch.Executions {
			contexts[execution.Context] = true
		}
	}
	state.StatusInterests = state.StatusInterests[:0]
	for interest := range contexts {
		state.StatusInterests = append(state.StatusInterests, interest)
	}
	sort.Strings(state.StatusInterests)
}

func (a *agenticController) shouldHandleAgenticStatus(event github.StatusEvent) bool {
	org, repo := event.Repo.Owner.Login, event.Repo.Name
	if !a.hasRepo(org, repo) || event.SHA == "" || event.Context == "" {
		return false
	}
	// Conservatively include reporting jobs on all branches, including manually
	// run conditional first-stage jobs. This also preserves unusual gate-named jobs.
	for _, job := range a.config().GetPresubmitsStatic(org + "/" + repo) {
		if !job.SkipReport && job.Context == event.Context {
			return true
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	contexts, known := a.statusContexts[agenticWork{org: org, repo: repo, sha: event.SHA}]
	if event.Context == agenticGate {
		return contexts[event.Context] // Ignore our CheckRun's name unless an actual job uses it.
	}
	return !known || contexts[event.Context] // Unknown/restarted SHAs must not lose frozen reports.
}
