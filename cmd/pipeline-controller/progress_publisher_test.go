package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

// progressCommentFake records comment API calls without any GitHub access.
type progressCommentFake struct {
	comments []github.IssueComment
	lists    int
	edits    []int
	bodies   []string
}

// ListIssueCommentsWithContext returns fixtures while counting lookup calls.
func (f *progressCommentFake) ListIssueCommentsWithContext(_ context.Context, _, _ string, _ int) ([]github.IssueComment, error) {
	f.lists++
	return f.comments, nil
}

// EditCommentWithContext records the exact comment being edited.
func (f *progressCommentFake) EditCommentWithContext(_ context.Context, _, _ string, id int, body string) error {
	f.edits = append(f.edits, id)
	f.bodies = append(f.bodies, body)
	return nil
}

// TestProgressPublisher only adopts the original bot comment and suppresses duplicate API calls.
func TestProgressPublisher(t *testing.T) {
	bot := github.User{Login: progressBot}
	for _, tc := range []struct {
		name     string
		comments []github.IssueComment
		want     []int
	}{
		{name: "earliest bot notification only", comments: []github.IssueComment{
			{ID: 15, Body: progressPrefix + "later notification", User: bot},
			{ID: 10, Body: pullRequestInfoComment, User: bot},
			{ID: 8, Body: progressPrefix + "not the bot", User: github.User{Login: "contributor"}},
			{ID: 7, Body: "**Pipeline controller notification**\n\nScheduling:\n/test e2e", User: bot},
		}, want: []int{10, 10}},
		{name: "missing notification never edited", comments: []github.IssueComment{
			{ID: 10, Body: pullRequestInfoComment, User: github.User{Login: "contributor"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &progressCommentFake{comments: tc.comments}
			p := newProgressPublisher(gh, logrus.NewEntry(logrus.New()))
			refs := &v1.Refs{Org: "openshift", Repo: "installer", BaseRef: "main", Pulls: []v1.Pull{{Number: 11087, SHA: "aaaa1111"}}}
			cfg := RepoConfig{Trigger: "auto", ProgressComment: true}
			p.submitProgress(RepoConfig{}, refs, progressView{}, true, true)
			if p.publishNext(t.Context()) {
				t.Fatal("disabled config enqueued an update")
			}
			v := progressView{trigger: progressStageOne}
			for _, phase := range []progressPhase{progressWaiting, progressWaiting, progressPosted, progressPosted} {
				v.phase = phase
				p.submitProgress(cfg, refs, v, false, false)
				p.publishNext(t.Context())
			}
			if diff := cmp.Diff(tc.want, gh.edits); diff != "" {
				t.Errorf("unexpected edit targets (-want +got):\n%s", diff)
			}
			if gh.lists != 1 {
				t.Errorf("got %d comment list calls, want 1", gh.lists)
			}
		})
	}
}
