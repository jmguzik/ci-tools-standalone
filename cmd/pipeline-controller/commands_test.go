package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/prow/pkg/github"
)

func TestPipelineHelpDoesNotReconcile(t *testing.T) {
	for _, mode := range []string{"traditional", "agentic", "edited", "not-pr"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			cw := &clientWrapper{ghc: f.gh}
			if mode == "agentic" {
				cw.agentic = f.a
			}
			event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
				Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: github.IssueComment{Body: "/pipeline help"}}
			switch mode {
			case "edited":
				event.Action = github.IssueCommentActionEdited
			case "not-pr":
				event.Issue.PullRequest = nil
			}
			cw.handleIssueComment(f.a.logger, event)
			if mode == "edited" || mode == "not-pr" {
				require.Empty(t, f.gh.comments)
			} else {
				require.Len(t, f.gh.comments, 1)
				for _, command := range []string{"help", "required", "remaining", "auto", "agent-review", "skip-agent-review", "tests-dispatched"} {
					require.Contains(t, f.gh.comments[0].Body, "`/pipeline "+command+"`")
				}
			}
			require.Zero(t, f.gh.getPullRequestCalls)
			require.Empty(t, f.gh.checks)
			require.Nil(t, f.a.store)
		})
	}
}

func TestTraditionalDispatchOverride(t *testing.T) {
	for _, scenario := range []string{"member", "collaborator", "untrusted", "disabled", "edited", "suffix", "no-app", "write-error"} {
		t.Run(scenario, func(t *testing.T) {
			refs := makeTriggerPJ(strings.Repeat("a", 40)).Spec.Refs
			foreign := traditionalCheck(refs, "completed", "success", "another app")
			foreign.ID, foreign.App.ID = 900, 202
			gh := &fakeAgenticGitHub{member: true, checks: []github.CheckRun{foreign},
				pr: github.PullRequest{Number: refs.Pulls[0].Number, State: github.PullRequestStateOpen,
					Head: github.PullRequestBranch{SHA: refs.Pulls[0].SHA},
					Base: github.PullRequestBranch{Ref: refs.BaseRef, Repo: github.Repo{Name: refs.Repo, Owner: github.User{Login: refs.Org}}}}}
			checks := &dispatchChecks{gh: gh, appID: 101}
			require.NoError(t, checks.pending(refs))
			enabled := mustEnabledConfig(t)
			cw := &clientWrapper{ghc: gh, checks: checks, watcher: &watcher{config: enabled}, lgtmWatcher: &watcher{},
				configDataProvider: &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{}}}
			event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: gh.pr.Base.Repo,
				Issue:   github.Issue{Number: gh.pr.Number, PullRequest: &struct{}{}},
				Comment: github.IssueComment{ID: 500, Body: "/pipeline tests-dispatched", User: github.User{Login: "maintainer"}}}
			switch scenario {
			case "collaborator":
				gh.member, gh.collaborator = false, true
			case "untrusted":
				gh.member = false
			case "disabled":
				cw.watcher.config.Orgs[0].Repos[0].Branches = []string{"release"}
			case "edited":
				event.Action = github.IssueCommentActionEdited
			case "suffix":
				event.Comment.Body += " please"
			case "no-app":
				cw.checks = nil
			case "write-error":
				gh.failCheck = true
			}
			cw.handleIssueComment(logrus.NewEntry(logrus.New()), event)
			require.Equal(t, foreign, gh.checks[0])
			require.Len(t, gh.checks, 2)
			if scenario == "member" || scenario == "collaborator" {
				require.Equal(t, "success", gh.checks[1].Conclusion)
				require.Contains(t, gh.checks[1].Output.Summary, "comment 500")
			} else {
				require.NotEqual(t, "success", gh.checks[1].Conclusion)
			}
			require.Zero(t, gh.statusWrites, "override must not change individual job results")
		})
	}
}

func TestAgenticDispatchOverrideSurvivesRestartButNotPush(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	request := f.command(500, "tests-dispatched")
	f.a.reader = errorLister{} // Manual completion must not depend on test discovery.
	f.reconcile(t, request)
	gate, state := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
	require.Equal(t, 500, state.DispatchOverrideID)
	require.Nil(t, state.Plan)
	require.Nil(t, state.Dispatch)
	require.Zero(t, f.jobs.creates)
	require.Zero(t, f.gh.statusWrites)
	require.NoError(t, f.a.closeStore())
	f.reconcile(t, nil)
	cw := &clientWrapper{ghc: f.gh, agentic: f.a}
	cw.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
		Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: github.IssueComment{Body: "/pipeline help"}})
	f.reconcile(t, f.command(600, "skip-agent-review"))
	current, state := f.gate(t)
	require.Equal(t, gate, current)
	require.Equal(t, 500, state.DispatchOverrideID, "help/label commands must not clear manual completion")
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.now = f.now.Add(time.Minute)
	f.a.reader = f.jobs
	f.reconcile(t, nil)
	f.reconcile(t, request)
	current, state = f.gate(t)
	require.Zero(t, state.DispatchOverrideID)
	require.NotEqual(t, "success", current.Conclusion)
	require.Len(t, f.gh.checks, 1, "old override authorized the new HEAD")
}

func TestAgenticExplicitDispatchResumesAfterOverride(t *testing.T) {
	f := newReadyAgenticFixture(t, "manual", "job-a")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "tests-dispatched"))
	f.reconcile(t, f.command(600, "remaining"))
	gate, state := f.gate(t)
	require.Zero(t, state.DispatchOverrideID)
	require.True(t, state.Dispatch.Posted)
	require.Equal(t, "success", gate.Conclusion)
	require.Equal(t, 1, f.jobs.creates)
}

func TestAgenticSuccessOnlyOverridePreservesGreenGate(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "write-error"}[failure], func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			gate, _ := f.gate(t)
			writes := len(f.gh.checkWrites)
			f.gh.failCheck = failure
			err := f.tryReconcile(f.command(500, "tests-dispatched"))
			if failure {
				require.Error(t, err)
				require.Equal(t, gate.Conclusion, f.gh.checks[0].Conclusion)
				_, state := f.gate(t)
				require.Equal(t, 500, state.DispatchOverrideID, "save the override even when publication fails")
				f.gh.failCheck = false
				require.NoError(t, f.a.closeStore())
				f.reconcile(t, nil)
			} else {
				require.NoError(t, err)
			}
			for _, write := range f.gh.checkWrites[writes:] {
				require.Equal(t, "completed", write.Status)
				require.Equal(t, "success", write.Conclusion, "success-only overrides must never retire a green gate")
			}
			require.Equal(t, 1, f.jobs.creates)
			require.Equal(t, 1, f.gh.statusWrites, "override must leave job results alone")
		})
	}
}
