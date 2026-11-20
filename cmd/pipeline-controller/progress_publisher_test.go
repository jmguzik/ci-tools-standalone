package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

// progressCommentFake records comment API calls without any GitHub access.
type progressCommentFake struct {
	comments []progressCommentNode
	queries  int
	org      string
	vars     map[string]interface{}
	limit    string
	edits    []int
	bodies   []string
}

// QueryWithGitHubAppsSupport returns fixtures and records the request bounds.
func (f *progressCommentFake) QueryWithGitHubAppsSupport(_ context.Context, dest interface{}, vars map[string]interface{}, org string) error {
	f.queries++
	f.org, f.vars = org, vars
	query, ok := dest.(*progressCommentQuery)
	if !ok {
		return errors.New("unexpected progress lookup query type")
	}
	// Comments is the only connection in the query and there is no cursor.
	field, ok := reflect.TypeOf(query.Repository.PullRequest).FieldByName("Comments")
	if !ok {
		return errors.New("missing comments connection")
	}
	f.limit = field.Tag.Get("graphql")
	query.Repository.PullRequest.Comments.Nodes = f.comments
	return nil
}

// EditCommentWithContext records the exact comment being edited.
func (f *progressCommentFake) EditCommentWithContext(_ context.Context, _, _ string, id int, body string) error {
	f.edits = append(f.edits, id)
	f.bodies = append(f.bodies, body)
	return nil
}

// TestProgressPublisher only adopts the original bot comment and suppresses duplicate API calls.
func TestProgressPublisher(t *testing.T) {
	bot := progressCommentAuthor{Login: progressBot, Type: "Bot"}
	for _, tc := range []struct {
		name     string
		comments []progressCommentNode
		want     []int
	}{
		{name: "earliest bot notification only with a full-size ID", comments: []progressCommentNode{
			{FullDatabaseID: "6832519745", Body: progressPrefix + "later notification", Author: bot},
			{FullDatabaseID: "6832519744", Body: pullRequestInfoComment, Author: bot},
			{FullDatabaseID: "8", Body: progressPrefix + "not the bot", Author: progressCommentAuthor{Login: progressBot, Type: "User"}},
			{FullDatabaseID: "7", Body: "**Pipeline controller notification**\n\nScheduling:\n/test e2e", Author: bot},
		}, want: []int{6832519744, 6832519744}},
		{name: "missing notification never edited", comments: []progressCommentNode{
			{FullDatabaseID: "10", Body: pullRequestInfoComment, Author: progressCommentAuthor{Login: "contributor", Type: "User"}},
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
			if gh.queries != 1 || gh.limit != "comments(first: 10)" || gh.org != "openshift" {
				t.Errorf("got %d lookups for %s with %q, want one bounded openshift lookup", gh.queries, gh.org, gh.limit)
			}
			wantVars := map[string]interface{}{
				"owner": githubql.String("openshift"), "name": githubql.String("installer"), "number": githubql.Int(11087),
			}
			if diff := cmp.Diff(wantVars, gh.vars); diff != "" {
				t.Errorf("unexpected query variables (-want +got):\n%s", diff)
			}
		})
	}
}
