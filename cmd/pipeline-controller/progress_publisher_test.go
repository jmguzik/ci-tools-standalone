package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
	head     string
	base     string
	closed   bool
	draft    bool
	editErr  error
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
	pr := &query.Repository.PullRequest
	pr.HeadRefOID, pr.BaseRefName, pr.State, pr.IsDraft = f.head, f.base, "OPEN", f.draft
	if f.closed {
		pr.State = "CLOSED"
	}
	if vars["comments"] == githubql.Boolean(true) {
		pr.Comments.Nodes = f.comments
	}
	return nil
}

// EditCommentWithContext records the exact comment being edited.
func (f *progressCommentFake) EditCommentWithContext(_ context.Context, _, _ string, id int, body string) error {
	f.edits = append(f.edits, id)
	f.bodies = append(f.bodies, body)
	return f.editErr
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
			{FullDatabaseID: "10", Body: pullRequestInfoComment, Author: progressCommentAuthor{Type: "User"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &progressCommentFake{comments: tc.comments, head: "aaaa1111", base: "main"}
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
			if gh.queries != 1 || gh.limit != "comments(first: 10) @include(if: $comments)" || gh.org != "openshift" {
				t.Errorf("got %d lookups for %s with %q, want one bounded openshift lookup", gh.queries, gh.org, gh.limit)
			}
			wantVars := map[string]interface{}{
				"owner": githubql.String("openshift"), "name": githubql.String("installer"), "number": githubql.Int(11087),
				"comments": githubql.Boolean(true),
			}
			if diff := cmp.Diff(wantVars, gh.vars); diff != "" {
				t.Errorf("unexpected query variables (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProgressDispatchSafety verifies which observations can write the original comment.
func TestProgressDispatchSafety(t *testing.T) {
	work := agenticWork{org: "openshift", repo: "installer", number: 11087}
	for _, tc := range []struct {
		name        string
		liveHead    string
		known       string
		draft       bool
		pause       bool
		disabled    bool
		editErr     error
		miss        bool
		wantEdits   int
		wantQueries int
	}{
		{name: "cold stale revision cannot edit", liveHead: "new", wantQueries: 1},
		{name: "old dispatch cannot replace a known newer revision", liveHead: "new", known: "new"},
		{name: "cold draft cannot edit", liveHead: "old", draft: true, wantQueries: 1},
		{name: "confirmed draft pause blocks dispatch until resume", liveHead: "old", pause: true, wantEdits: 1, wantQueries: 1},
		{name: "late Chai observation cannot resume disabled progress", liveHead: "old", disabled: true},
		{name: "unchanged body retries failed edit after backoff", liveHead: "old", editErr: errors.New("temporary test edit failure"), wantEdits: 2, wantQueries: 1},
		{name: "unchanged body retries missing comment after TTL", liveHead: "old", miss: true, wantEdits: 1, wantQueries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := progressCommentNode{FullDatabaseID: "10", Body: pullRequestInfoComment, Author: progressCommentAuthor{Login: progressBot, Type: "Bot"}}
			gh := &progressCommentFake{comments: []progressCommentNode{comment}, head: tc.liveHead, base: "main", draft: tc.draft, editErr: tc.editErr}
			if tc.miss {
				gh.comments = nil
			}
			p := newProgressPublisher(gh, logrus.NewEntry(logrus.New()))
			if tc.known != "" {
				p.prs[work] = &progressEntry{verified: progressRevision{head: tc.known, base: "main"}}
			}
			refs := &v1.Refs{Org: work.org, Repo: work.repo, BaseRef: "main", Pulls: []v1.Pull{{Number: work.number, SHA: "old"}}}
			rev := progressRevision{head: "old", base: "main"}
			if tc.disabled {
				cw := &clientWrapper{}
				p.enabled = cw.progressEnabled
				p.prs[work] = &progressEntry{verified: rev, paused: true}
				p.submit(refs, "late dispatch", true, false)
			}
			p.submit(refs, "dispatch", false, false)
			if tc.pause {
				generation, _ := p.lifecycleGeneration(work, rev, true)
				if !p.applyLifecycle(work, generation, rev, "", progressComment{}) {
					t.Fatal("could not pause current revision")
				}
				p.submit(refs, "late dispatch", false, false)
				if p.publishNext(t.Context()) {
					t.Fatal("draft pause should suppress queued and late dispatches")
				}
				generation, _ = p.lifecycleGeneration(work, rev, false)
				if !p.applyLifecycle(work, generation, rev, "waiting", progressComment{}) {
					t.Fatal("could not resume current revision")
				}
			}
			p.publishNext(t.Context())
			if tc.editErr != nil || tc.miss {
				p.submit(refs, "dispatch", false, false)
				if p.publishNext(t.Context()) {
					t.Fatal("duplicate queued before the retry interval")
				}
				e := p.prs[work]
				e.retryAt, e.comment.looked = time.Time{}, time.Time{}
				gh.editErr, gh.comments = nil, []progressCommentNode{comment}
				p.submit(refs, "dispatch", false, false)
				p.publishNext(t.Context())
			}
			if len(gh.edits) != tc.wantEdits || gh.queries != tc.wantQueries {
				t.Errorf("got %d edits and %d queries, want %d and %d", len(gh.edits), gh.queries, tc.wantEdits, tc.wantQueries)
			}
			if tc.disabled && !p.prs[work].paused {
				t.Error("late Chai observation cleared the verified pause")
			}
		})
	}
}

// TestProgressLifecycleGeneration prevents an old verification from overwriting a newer revision.
func TestProgressLifecycleGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{name: "older waiting result", body: "waiting"},
		{name: "older cancellation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProgressPublisher(&progressCommentFake{}, logrus.NewEntry(logrus.New()))
			work := agenticWork{org: "openshift", repo: "installer", number: 11087}
			old, newer := progressRevision{head: "old", base: "main"}, progressRevision{head: "newer", base: "main"}
			p.prs[work] = &progressEntry{verified: old}
			generation, _ := p.lifecycleGeneration(work, old, tc.body == "")
			if !p.applyLifecycle(work, generation, newer, "newer revision", progressComment{}) {
				t.Fatal("could not advance to the newer revision")
			}
			if p.applyLifecycle(work, generation, old, tc.body, progressComment{}) {
				t.Fatal("superseded verification was accepted")
			}
			if e := p.prs[work]; e.verified != newer || e.paused || e.request.body != "newer revision" {
				t.Fatal("newer progress was replaced")
			}
		})
	}
}
