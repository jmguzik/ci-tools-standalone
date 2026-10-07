package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/labels"
)

func mixedAgenticFixture(t *testing.T) (*agenticFixture, *clientWrapper) {
	t.Helper()
	f := newAgenticFixture(t, "auto")
	agentic := f.a.watcher
	agentic.config.Orgs[0].Repos[0].Branches = []string{"main"}
	var normal enabledConfig
	require.NoError(t, yaml.Unmarshal([]byte("orgs:\n- org: org\n  repos:\n  - name: repo\n    branches: [release]\n    mode:\n      trigger: auto\n"), &normal))
	f.a.watcher, f.a.lgtmWatcher = &watcher{config: normal}, agentic
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		"org/repo": {protected: []config.Presubmit{{JobBase: config.JobBase{Name: "repo-release-protected"},
			Reporter: config.Reporter{Context: "ci/legacy"}, RerunCommand: "/test legacy"}}},
	}}
	cw := &clientWrapper{ghc: f.gh, agentic: f.a, watcher: f.a.watcher, lgtmWatcher: f.a.lgtmWatcher, configDataProvider: provider, pjLister: f.jobs,
		checks: &dispatchChecks{gh: f.gh, appID: f.a.appID}}
	return f, cw
}

func staleNormalEvent(f *agenticFixture) github.PullRequestEvent {
	pr := f.gh.pr
	pr.Base.Ref = "release"
	return github.PullRequestEvent{Action: github.PullRequestActionOpened, Repo: github.Repo{Name: "repo", Owner: github.User{Login: "org"}}, PullRequest: pr}
}

func TestAgenticMixedModeRejectsStaleLegacyEvents(t *testing.T) {
	for _, handler := range []string{"notification", "placeholders", "lgtm"} {
		t.Run(handler, func(t *testing.T) {
			f, cw := mixedAgenticFixture(t)
			event := staleNormalEvent(f)
			switch handler {
			case "notification":
				cw.handlePullRequestCreation(f.a.logger, event)
			case "placeholders":
				cw.handlePipelineContextCreation(f.a.logger, event)
			case "lgtm":
				// The ordinary branch uses LGTM and the agentic branch uses auto.
				f.a.watcher, f.a.lgtmWatcher = f.a.lgtmWatcher, f.a.watcher
				cw.watcher, cw.lgtmWatcher = f.a.watcher, f.a.lgtmWatcher
				event.Action, event.Label = github.PullRequestActionLabeled, github.Label{Name: labels.LGTM}
				cw.handleLabelAddition(f.a.logger, event)
			}
			if len(f.gh.comments) != 0 || f.gh.statusWrites != 0 || f.jobs.creates != 0 {
				t.Fatal("old normal-branch event performed legacy side effects on an agentic PR")
			}
			require.Empty(t, f.gh.checks)
		})
	}
}

func TestAgenticMixedModeOpeningNotification(t *testing.T) {
	f, cw := mixedAgenticFixture(t)
	event := github.PullRequestEvent{Action: github.PullRequestActionOpened, Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr}
	cw.handlePullRequestCreation(f.a.logger, event)
	require.Len(t, f.gh.comments, 1)
	require.Equal(t, pullRequestInfoComment, f.gh.comments[0].Body)

	for _, scenario := range []string{"dry-run", "lookup-failure", "disabled-branch", "not-opened"} {
		t.Run(scenario, func(t *testing.T) {
			f, cw := mixedAgenticFixture(t)
			event := github.PullRequestEvent{Action: github.PullRequestActionOpened, Repo: f.gh.pr.Base.Repo, PullRequest: f.gh.pr}
			switch scenario {
			case "dry-run":
				f.a.dryRun = true
			case "lookup-failure":
				f.gh.getPullRequestError = errors.New("GitHub unavailable")
			case "disabled-branch":
				f.gh.pr.Base.Ref, event.PullRequest.Base.Ref = "disabled", "disabled"
			case "not-opened":
				event.Action = github.PullRequestActionSynchronize
			}
			cw.handlePullRequestCreation(f.a.logger, event)
			require.Empty(t, f.gh.comments)
		})
	}
}

func TestAgenticMixedModeRejectsStaleProwJob(t *testing.T) {
	for _, lookupFails := range []bool{false, true} {
		f, cw := mixedAgenticFixture(t)
		pj := &v1.ProwJob{ObjectMeta: metav1.ObjectMeta{Name: "old-first-stage", Namespace: "ci"}, Spec: v1.ProwJobSpec{Type: v1.PresubmitJob, Job: "first-stage",
			Refs: &v1.Refs{Org: "org", Repo: "repo", BaseRef: "release", Pulls: []v1.Pull{{Number: 42, SHA: f.gh.pr.Head.SHA}}}}}
		require.NoError(t, f.jobs.Client.Create(context.Background(), pj))
		if lookupFails {
			f.gh.getPullRequestError = errors.New("GitHub unavailable")
		}
		r := &reconciler{pjclientset: f.jobs, lister: f.jobs, configDataProvider: cw.configDataProvider, ghc: f.gh,
			watcher: f.a.watcher, lgtmWatcher: f.a.lgtmWatcher, agentic: f.a, logger: f.a.logger, checks: cw.checks,
			closedPRsCache: closedPRsCache{prs: map[string]pullRequest{}, ghc: f.gh}}
		err := r.reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pj.Namespace, Name: pj.Name}})
		if (err != nil) != lookupFails {
			t.Fatalf("lookupFails=%v: unexpected reconcile error: %v", lookupFails, err)
		}
		if len(f.gh.comments) != 0 || f.gh.statusWrites != 0 {
			t.Fatal("old normal-branch ProwJob dispatched legacy tests")
		}
		require.Empty(t, f.gh.checks)
	}
}

func TestAgenticMixedModeLiveGuardAndNormalCoexistence(t *testing.T) {
	for _, scenario := range []string{"current-normal", "lookup-failure", "stale-head", "normal-only"} {
		t.Run(scenario, func(t *testing.T) {
			f, cw := mixedAgenticFixture(t)
			event := staleNormalEvent(f)
			f.gh.pr.Base.Ref = "release"
			wantWrites := 0
			switch scenario {
			case "current-normal":
				wantWrites = 1
			case "lookup-failure":
				f.gh.getPullRequestError = errors.New("GitHub unavailable")
			case "stale-head":
				f.gh.pr.Head.SHA = "different-head"
			case "normal-only":
				f.a.lgtmWatcher, cw.lgtmWatcher = &watcher{}, &watcher{}
				f.gh.getPullRequestError = errors.New("must not query GitHub")
				wantWrites = 1
			}
			cw.handlePipelineContextCreation(f.a.logger, event)
			if f.gh.statusWrites != wantWrites {
				t.Fatalf("got %d legacy contexts, want %d", f.gh.statusWrites, wantWrites)
			}
			require.Len(t, f.gh.checks, wantWrites)
			if scenario == "normal-only" && f.gh.getPullRequestCalls != 0 {
				t.Fatal("normal-only repository acquired a new live-PR lookup")
			}
		})
	}
}

func TestAgenticCommentRetargetBetweenRoutingReads(t *testing.T) {
	f, cw := mixedAgenticFixture(t)
	f.gh.pr.Base.Ref = "release"
	f.gh.beforePullRequest = func(call int) {
		if call == 2 {
			f.gh.pr.Base.Ref = "main"
		}
	}
	event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: github.Repo{Name: "repo", Owner: github.User{Login: "org"}},
		Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: github.IssueComment{Body: "Unrelated discussion."}}
	cw.handleIssueComment(f.a.logger, event)
	require.Zero(t, f.gh.getPullRequestCalls)
	event.Comment.Body = "/pipeline required extra text" // Ordinary mode accepts command prefixes.
	cw.handleIssueComment(f.a.logger, event)
	if f.gh.getPullRequestCalls != 2 || len(f.gh.comments) != 0 || f.gh.statusWrites != 0 {
		t.Fatal("retarget between routing lookups fell through to legacy dispatch")
	}
}

func TestAgenticStaleNormalEventReconcilesCurrentAgenticRevision(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	f.a.handlePullRequest(f.a.logger, staleNormalEvent(f))
	f.a.startQueue(t.Context())
	t.Cleanup(f.a.queue.ShutDown)
	f.a.processNext(t.Context())
	gate, state := f.gate(t)
	if gate.ID != 0 || state.BaseBranch != "main" || state.RevisionPending || f.gh.statusWrites != 0 {
		t.Fatal("stale event did not reconcile the live agentic revision safely")
	}
}

func TestAgenticBatchRetestsDoNotChangePRSelection(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	writes := len(f.gh.checkWrites)
	// Tide may retest a larger set, including jobs Chai did not select. Batch
	// results must not be adopted as single-PR dispatch/report evidence.
	pj := &v1.ProwJob{ObjectMeta: metav1.ObjectMeta{Name: "tide-extra-job", Namespace: "ci"},
		Spec: v1.ProwJobSpec{Type: v1.BatchJob, Job: "job-b", Refs: &v1.Refs{Org: "org", Repo: "repo", BaseRef: "main",
			Pulls: []v1.Pull{{Number: 42, SHA: f.gh.pr.Head.SHA}, {Number: 43, SHA: "another-head"}}}}}
	require.NoError(t, f.jobs.Client.Create(context.Background(), pj))
	r := &reconciler{pjclientset: f.jobs, agentic: f.a}
	require.NoError(t, r.reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pj.Namespace, Name: pj.Name}}))
	current, state := f.gate(t)
	if len(f.gh.checkWrites) != writes || current.Output.Text != gate.Output.Text || len(state.Plan.Jobs) != 1 || state.Plan.Jobs[0].Name != "job-a" {
		t.Fatal("batch retest changed the PR-scoped selection or gate")
	}
}

func TestAgenticCommandAuthorization(t *testing.T) {
	for _, author := range []string{"member", "collaborator", "outsider"} {
		t.Run(author, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "manual", "job-a")
			f.gh.member, f.gh.collaborator = author == "member", author == "collaborator"
			f.reconcile(t, nil)
			f.reconcile(t, f.command(500, "required"))
			_, state := f.gate(t)
			if author == "outsider" {
				if state.ManualRequestID != 0 || f.jobs.creates != 0 {
					t.Fatal("untrusted command authorized dispatch")
				}
			} else if state.ManualRequestID != 500 || f.jobs.creates != 1 {
				t.Fatal("trusted command did not authorize dispatch")
			}
		})
	}
}
