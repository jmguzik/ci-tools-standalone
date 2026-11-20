package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

// TestProgressCopy keeps readiness, bot acknowledgement, and job results distinct.
func TestProgressCopy(t *testing.T) {
	refs := &v1.Refs{Org: "openshift", Repo: "installer", BaseRef: "main", Pulls: []v1.Pull{{Number: 11087, SHA: "aaaa1111"}}}
	for _, tc := range []struct {
		name string
		view progressView
		want []string
		omit string
	}{
		{name: "traditional no fake intermediate step", view: progressView{trigger: progressStageOne, satisfied: true, phase: progressPosted}, want: []string{"class trigger done", "class outcome here", "posted stage-2 test requests"}, omit: "Agentic dispatching"},
		{name: "auto override still needs stage one", view: waitingProgress(RepoConfig{Trigger: "lgtm"}, &github.PullRequest{Labels: []github.Label{{Name: PipelineAutoLabel}}}, true), want: []string{"Auto enabled", "class trigger done", "class wait here", "Waiting for stage 1"}},
		{name: "live Chai deadline", view: progressView{trigger: progressManual, satisfied: true, phase: progressChai, chai: true, deadline: time.Unix(1, 0)}, want: []string{"class handoff here", "before the timeout", "**If you're stuck:**", "/pipeline skip-agentic-mode"}},
		{name: "Chai replied without finishing", view: progressView{trigger: progressLGTM, phase: progressChai, chai: true, replied: true}, want: []string{"no automatic fallback after a reply", "/pipeline skip-agentic-mode"}},
		{name: "Chai mark not a test result", view: progressView{trigger: progressManual, satisfied: true, phase: progressChaiMarked, chai: true}, want: []string{"Chai marked dispatch complete", "see which jobs started", "Tests dispatched"}, omit: "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderProgress(refs, tc.view)
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in progress comment", want)
				}
			}
			if tc.omit != "" && strings.Contains(body, tc.omit) {
				t.Errorf("progress comment unexpectedly contains %q", tc.omit)
			}
			if _, executable := parsePipelineCommand(body); executable || strings.Contains(body, "\n/test ") {
				t.Error("progress comment contains an executable command")
			}
		})
	}
}

// TestProgressRevision keeps delayed push events from replacing the current comment.
func TestProgressRevision(t *testing.T) {
	for _, tc := range []struct {
		name, live string
		enabled    bool
		want       bool
	}{
		{name: "current opt-in revision", live: "new", enabled: true, want: true},
		{name: "delayed push", live: "newer", enabled: true},
		{name: "no opt-in", live: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			pr := f.gh.pr
			pr.Head.SHA = "new"
			f.gh.pr.Head.SHA = tc.live
			w := &watcher{}
			config := "orgs:\n- org: org\n  repos:\n  - name: repo\n    mode: {progress_comment: true}\n"
			if !tc.enabled {
				config = "orgs:\n- org: org\n  repos: [repo]\n"
			}
			if err := yaml.Unmarshal([]byte(config), &w.config); err != nil {
				t.Fatal(err)
			}
			gh := &progressCommentFake{comments: []github.IssueComment{{ID: 10, Body: pullRequestInfoComment, User: github.User{Login: progressBot}}}}
			logger := logrus.NewEntry(logrus.New())
			p := newProgressPublisher(gh, logger)
			cw := &clientWrapper{ghc: f.gh, lgtmWatcher: w, progress: p}
			cw.handleProgressRevision(logger, github.PullRequestEvent{Action: github.PullRequestActionSynchronize, Repo: pr.Base.Repo, PullRequest: pr})
			if got := p.publishNext(t.Context()); got != tc.want {
				t.Errorf("published progress = %t, want %t", got, tc.want)
			}
			if tc.want && (len(gh.bodies) != 1 || !strings.Contains(gh.bodies[0], "Waiting for `/lgtm`")) {
				t.Error("LGTM config did not project an LGTM waiting comment")
			}
		})
	}
}
