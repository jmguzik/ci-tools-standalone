package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/pjutil"
)

type fakeAgenticGitHub struct {
	pr                    github.PullRequest
	comments              []github.IssueComment
	checks                []github.CheckRun
	statuses              map[string][]github.Status
	statusHistory         map[string][]github.Status
	listStatusesCalls     int
	listStatusesError     error
	changes               []github.PullRequestChange
	member                bool
	collaborator          bool
	statusWrites          int
	checkWrites           []github.CheckRun
	failCheck             bool
	failCommentAfterWrite bool
	listCommentsError     error
	failEditComment       bool
	otherPRs              []github.PullRequest
	checkAttempts         int
	failCheckAt           int
	failAddLabel          bool
	getPullRequestError   error
	getPullRequestCalls   int
	getPullRequestsError  error
	getPullRequestsCalls  int
	beforePullRequest     func(int)
}

func (f *fakeAgenticGitHub) GetPullRequest(_, _ string, _ int) (*github.PullRequest, error) {
	f.getPullRequestCalls++
	if f.beforePullRequest != nil {
		f.beforePullRequest(f.getPullRequestCalls)
	}
	if f.getPullRequestError != nil {
		return nil, f.getPullRequestError
	}
	copy := f.pr
	copy.Labels = append([]github.Label{}, f.pr.Labels...)
	return &copy, nil
}
func (f *fakeAgenticGitHub) GetPullRequests(_, _ string) ([]github.PullRequest, error) {
	f.getPullRequestsCalls++
	return append([]github.PullRequest{f.pr}, f.otherPRs...), f.getPullRequestsError
}
func (f *fakeAgenticGitHub) CreateComment(_, _ string, _ int, body string) error {
	f.comments = append(f.comments, github.IssueComment{ID: f.nextCommentID(), Body: body, User: github.User{Login: "controller[bot]"}})
	if f.failCommentAfterWrite {
		f.failCommentAfterWrite = false
		return fmt.Errorf("response lost after creating comment: %w", io.ErrUnexpectedEOF)
	}
	return nil
}

func (f *fakeAgenticGitHub) nextCommentID() int {
	id := 100
	for _, comment := range f.comments {
		id = max(id, comment.ID+1)
	}
	return id
}

func (f *fakeAgenticGitHub) removeRevisionMarker() {
	var kept []github.IssueComment
	for _, comment := range f.comments {
		if !strings.Contains(comment.Body, agenticRevisionMarker) {
			kept = append(kept, comment)
		}
	}
	f.comments = kept
}
func (f *fakeAgenticGitHub) EditComment(_, _ string, id int, body string) error {
	if f.failEditComment {
		return errors.New("comment edit failed")
	}
	for i := range f.comments {
		if f.comments[i].ID == id {
			f.comments[i].Body = body
			return nil
		}
	}
	return errors.New("unknown comment")
}
func (f *fakeAgenticGitHub) ListIssueComments(_, _ string, _ int) ([]github.IssueComment, error) {
	return append([]github.IssueComment{}, f.comments...), f.listCommentsError
}
func (f *fakeAgenticGitHub) GetPullRequestChanges(_, _ string, _ int) ([]github.PullRequestChange, error) {
	return f.changes, nil
}
func (f *fakeAgenticGitHub) CreateStatus(_, _, _ string, _ github.Status) error {
	f.statusWrites++
	return nil
}
func (f *fakeAgenticGitHub) AddLabel(_, _ string, _ int, label string) error {
	if f.failAddLabel {
		return errors.New("label write failed")
	}
	f.pr.Labels = append(f.pr.Labels, github.Label{Name: label})
	return nil
}
func (f *fakeAgenticGitHub) RemoveLabel(_, _ string, _ int, label string) error {
	var labels []github.Label
	for _, old := range f.pr.Labels {
		if old.Name != label {
			labels = append(labels, old)
		}
	}
	f.pr.Labels = labels
	return nil
}
func (f *fakeAgenticGitHub) GetIssueLabels(_, _ string, _ int) ([]github.Label, error) {
	return f.pr.Labels, nil
}
func (f *fakeAgenticGitHub) ListCheckRuns(_, _, sha string) (*github.CheckRunList, error) {
	result := &github.CheckRunList{}
	for _, check := range f.checks {
		if check.HeadSHA == sha {
			result.CheckRuns = append(result.CheckRuns, check)
		}
	}
	return result, nil
}
func (f *fakeAgenticGitHub) CreateCheckRun(_, _ string, check github.CheckRun) (int64, error) {
	f.checkAttempts++
	if f.failCheck || f.checkAttempts == f.failCheckAt {
		return 0, fmt.Errorf("check write failed: %w", io.ErrUnexpectedEOF)
	}
	check.ID, check.App.ID = int64(len(f.checks)+1), 101
	f.checks = append(f.checks, check)
	f.checkWrites = append(f.checkWrites, check)
	return check.ID, nil
}
func (f *fakeAgenticGitHub) UpdateCheckRun(_, _ string, id int64, check github.CheckRun) error {
	f.checkAttempts++
	if f.failCheck || f.checkAttempts == f.failCheckAt {
		return fmt.Errorf("check write failed: %w", io.ErrUnexpectedEOF)
	}
	for i := range f.checks {
		if f.checks[i].ID != id {
			continue
		}
		old := f.checks[i]
		if check.Name != "" {
			old.Name = check.Name
		}
		if check.ExternalID != "" {
			old.ExternalID = check.ExternalID
		}
		if check.Status != "" {
			old.Status = check.Status
		}
		if check.Conclusion != "" {
			old.Conclusion = check.Conclusion
		}
		// Model a PATCH which retains omitted fields: a reopening implementation
		// must not depend on an omitted conclusion clearing an old success.
		old.Output = check.Output
		f.checks[i] = old
		f.checkWrites = append(f.checkWrites, old)
		return nil
	}
	return errors.New("unknown check")
}
func (f *fakeAgenticGitHub) GetCombinedStatus(_, _, sha string) (*github.CombinedStatus, error) {
	return &github.CombinedStatus{SHA: sha, Statuses: append([]github.Status{}, f.statuses[sha]...)}, nil
}
func (f *fakeAgenticGitHub) ListStatuses(_, _, sha string) ([]github.Status, error) {
	f.listStatusesCalls++
	return append([]github.Status{}, f.statusHistory[sha]...), f.listStatusesError
}
func (f *fakeAgenticGitHub) IsMember(_, _ string) (bool, error) { return f.member, nil }
func (f *fakeAgenticGitHub) IsCollaborator(_, _, _ string) (bool, error) {
	return f.collaborator, nil
}
func (f *fakeAgenticGitHub) BotUserChecker() (func(string) bool, error) {
	return func(login string) bool { return login == "controller[bot]" }, nil
}

type agenticTestJobs struct {
	ctrlruntimeclient.Client
	creates      int
	failAt       int
	beforeCreate func(*v1.ProwJob)
}

func (j *agenticTestJobs) Create(ctx context.Context, object ctrlruntimeclient.Object, options ...ctrlruntimeclient.CreateOption) error {
	j.creates++
	if j.failAt > 0 && j.creates == j.failAt {
		return apierrors.NewServiceUnavailable("injected create failure")
	}
	if j.beforeCreate != nil {
		j.beforeCreate(object.(*v1.ProwJob))
	}
	return j.Client.Create(ctx, object, options...)
}

type agenticFixture struct {
	a    *agenticController
	gh   *fakeAgenticGitHub
	jobs *agenticTestJobs
	cfg  *config.Config
	now  time.Time
}

func newAgenticFixture(t *testing.T, mode string) *agenticFixture {
	t.Helper()
	f := &agenticFixture{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	static := []config.Presubmit{
		{JobBase: config.JobBase{Name: "first-stage", Agent: "kubernetes"}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/first"}},
		{JobBase: config.JobBase{Name: "job-a", Agent: "kubernetes", Annotations: map[string]string{"pipeline_run_if_changed": "^src/"}, Labels: map[string]string{"configured-label": "kept"}}, Reporter: config.Reporter{Context: "ci/job-a"}, Brancher: config.Brancher{Branches: []string{"^main$"}}},
		{JobBase: config.JobBase{Name: "job-b", Agent: "kubernetes"}, Reporter: config.Reporter{Context: "ci/job-b"}},
		{JobBase: config.JobBase{Name: "optional-job", Agent: "kubernetes", Annotations: map[string]string{"pipeline_run_if_changed": "^docs/"}}, Optional: true, Reporter: config.Reporter{Context: "ci/optional"}},
	}
	f.cfg = &config.Config{ProwConfig: config.ProwConfig{ProwJobNamespace: "ci"}}
	require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": static}))
	f.gh = &fakeAgenticGitHub{member: true, statuses: map[string][]github.Status{}, changes: []github.PullRequestChange{{Filename: "src/main.go"}},
		pr: github.PullRequest{Number: 42, State: github.PullRequestStateOpen, CreatedAt: f.now.Add(-time.Hour),
			Head: github.PullRequestBranch{SHA: strings.Repeat("a", 40)}, Base: github.PullRequestBranch{Ref: "main", SHA: strings.Repeat("b", 40), Repo: github.Repo{Name: "repo", Owner: github.User{Login: "org"}, HTMLURL: "https://github.com/org/repo"}}}}
	var enrollment enabledConfig
	require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf("orgs:\n- org: org\n  repos:\n  - name: repo\n    branches: [main, release]\n    mode:\n      trigger: %s\n      agentic:\n        mode: chai\n", mode)), &enrollment))
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	f.jobs = &agenticTestJobs{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	f.a = &agenticController{gh: f.gh, jobs: f.jobs, reader: f.jobs, config: func() *config.Config { return f.cfg },
		watcher: &watcher{config: enrollment}, lgtmWatcher: &watcher{}, appID: 101, now: func() time.Time { return f.now }, logger: logrus.NewEntry(logrus.New()),
		options: agenticOptions{timeout: defaultAgenticTimeout, trustedAuthors: flagutil.NewStrings("chai[bot]")}}
	return f
}

// Prepare a current plan and first-stage success without yet reconciling.
func newReadyAgenticFixture(t *testing.T, mode string, jobs ...string) *agenticFixture {
	t.Helper()
	f := newAgenticFixture(t, mode)
	f.plan(t, jobs...)
	f.passFirstStage(t)
	return f
}

func (f *agenticFixture) tryReconcile(command *github.IssueComment) error {
	return f.a.reconcile(context.Background(), "org", "repo", f.gh.pr.Number, command)
}

func (f *agenticFixture) reconcile(t *testing.T, command *github.IssueComment) {
	t.Helper()
	require.NoError(t, f.tryReconcile(command))
}

func (f *agenticFixture) command(id int, command string) *github.IssueComment {
	comment := &github.IssueComment{ID: id, Body: "/pipeline " + command, CreatedAt: f.now, User: github.User{Login: "maintainer"}}
	f.gh.comments = append(f.gh.comments, *comment)
	return comment
}

func (f *agenticFixture) plan(t *testing.T, names ...string) github.IssueComment {
	t.Helper()
	if names == nil {
		names = []string{}
	}
	body := formatAgenticPlan(agenticPlan{HeadSHA: f.gh.pr.Head.SHA, BaseBranch: f.gh.pr.Base.Ref, Jobs: names, Rationale: "Relevant tests."})
	comment := github.IssueComment{ID: f.gh.nextCommentID(), Body: body, User: github.User{Login: "chai[bot]", Type: github.UserTypeBot}, CreatedAt: f.now}
	f.gh.comments = append(f.gh.comments, comment)
	return comment
}

func (f *agenticFixture) gate(t *testing.T) (github.CheckRun, *agenticState) {
	t.Helper()
	check, state, err := f.a.loadState("org", "repo", &f.gh.pr)
	require.NoError(t, err)
	return *check, state
}

func (f *agenticFixture) allJobs(t *testing.T) []v1.ProwJob {
	t.Helper()
	var jobs v1.ProwJobList
	require.NoError(t, f.jobs.List(context.Background(), &jobs))
	var selected []v1.ProwJob
	for _, pj := range jobs.Items {
		if pj.Spec.Job != "first-stage" {
			selected = append(selected, pj)
		}
	}
	return selected
}

func (f *agenticFixture) deleteJobs(t *testing.T, jobs ...v1.ProwJob) {
	t.Helper()
	for _, pj := range jobs {
		require.NoError(t, f.jobs.Delete(context.Background(), &pj))
	}
}

func (f *agenticFixture) passFirstStage(t *testing.T) {
	t.Helper()
	pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, f.cfg.GetPresubmitsStatic("org/repo")[0], "first", nil)
	pj.Name, pj.Namespace = "first-"+f.gh.pr.Head.SHA[:8]+"-"+f.gh.pr.Base.Ref, "ci"
	pj.CreationTimestamp = metav1.NewTime(f.now.Add(-time.Minute))
	pj.Status.State, pj.Status.URL = v1.SuccessState, "https://prow/first-stage/1"
	if err := f.jobs.Client.Create(context.Background(), &pj); !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	f.gh.statuses[f.gh.pr.Head.SHA] = []github.Status{{Context: "ci/first", State: github.StatusSuccess, TargetURL: "https://prow/first-stage/1"}}
}

func (f *agenticFixture) report(t *testing.T, state v1.ProwJobState) {
	t.Helper()
	for _, pj := range f.allJobs(t) {
		pj.Status.State = state
		pj.Status.URL = "https://prow/view/" + pj.Name
		pj.Status.PrevReportStates = map[string]v1.ProwJobState{"github-reporter": state}
		require.NoError(t, f.jobs.Update(context.Background(), &pj))
		status := github.StatusPending
		if state == v1.FailureState {
			status = github.StatusFailure
		}
		current := f.gh.statuses[f.gh.pr.Head.SHA]
		var next []github.Status
		for _, prior := range current {
			if prior.Context != pj.Spec.Context {
				next = append(next, prior)
			}
		}
		f.gh.statuses[f.gh.pr.Head.SHA] = append(next, github.Status{Context: pj.Spec.Context, State: status, TargetURL: pj.Status.URL})
	}
}

func TestAgenticDispatchAndReporting(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	check, state := f.gate(t)
	if check.Status != "in_progress" || state.Dispatch == nil || !state.Frozen || len(f.allJobs(t)) != 1 {
		t.Fatalf("expected a closed gate and one execution, got %+v / %+v", check, state)
	}
	require.Contains(t, check.Output.Summary, "Test selection is locked for this commit. Later Chai plans are ignored; push a new commit to change the selection.", "dispatch check must explain the selection lock")
	pj := f.allJobs(t)[0]
	if pj.Spec.Job != "job-a" || !pj.Spec.Report || pj.Labels["configured-label"] != "kept" || pj.Annotations["pipeline_run_if_changed"] != "^src/" {
		t.Fatalf("lost selected job definition/metadata: %+v", pj)
	}
	require.Zero(t, f.gh.statusWrites, "controller created per-job placeholders")
	f.report(t, v1.FailureState)
	f.reconcile(t, nil)
	check, state = f.gate(t)
	if check.Conclusion != "success" || !state.Dispatch.Executions[0].Reported {
		t.Fatal("gate did not open after the requested execution reported")
	}
	require.Contains(t, check.Output.Summary, "Test selection is locked for this commit.", "completed dispatch check lost the selection-lock explanation")
	require.Equal(t, github.StatusFailure, f.gh.statuses[f.gh.pr.Head.SHA][1].State, "real job failure was overwritten")
	// Durable evidence survives controller restart and ProwJob garbage collection.
	f.deleteJobs(t, f.allJobs(t)...)
	f.reconcile(t, nil)
	require.Equal(t, 1, f.jobs.creates, "recreated an execution already durably acknowledged")
}

func TestAgenticManualEarlyRequestAndRevisionBinding(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	request := f.command(500, "required")
	f.reconcile(t, request)
	check, state := f.gate(t)
	if state.ManualRequestID != 500 || state.Dispatch != nil || state.WaitingSince != nil || len(f.allJobs(t)) != 0 {
		t.Fatalf("early command was not durably deferred: %+v", state)
	}
	require.NotContains(t, check.Output.Summary, "Test selection is locked for this commit.", "check described a pending selection as locked")
	f.now = f.now.Add(time.Minute)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	require.Len(t, f.allJobs(t), 1, "recorded manual request was lost on restart")
	// A repeated delivery does not force a second run.
	f.reconcile(t, request)
	require.Len(t, f.allJobs(t), 1, "redelivered command duplicated a dispatch")
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.now = f.now.Add(time.Minute)
	f.passFirstStage(t)
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	_, state = f.gate(t)
	if state.ManualRequestID != 0 || state.Dispatch != nil {
		t.Fatal("old manual request authorized a new HEAD")
	}
	f.reconcile(t, request)
	_, state = f.gate(t)
	require.Zero(t, state.ManualRequestID, "delayed old command authorized a new HEAD")
}

func TestAgenticTimeoutWaitsForPrerequisitesAndIgnoresLatePlans(t *testing.T) {
	f := newAgenticFixture(t, "lgtm")
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.now = f.now.Add(2 * time.Hour)
	f.reconcile(t, nil)
	_, state := f.gate(t)
	if state.WaitingSince != nil || state.Plan != nil || f.jobs.creates != 0 {
		t.Fatal("timeout ran while waiting for LGTM")
	}
	f.gh.pr.Labels = []github.Label{{Name: "lgtm"}}
	f.reconcile(t, nil)
	_, state = f.gate(t)
	if state.WaitingSince == nil || !state.WaitingSince.Equal(f.now) {
		t.Fatal("timeout did not start when all other prerequisites held")
	}
	f.now = f.now.Add(defaultAgenticTimeout)
	f.reconcile(t, nil)
	_, state = f.gate(t)
	if state.Plan.Source != "timeout" || len(state.Plan.Jobs) != 2 || f.jobs.creates != 2 || hasAgenticLabel(&f.gh.pr, agenticSkipLabel) {
		t.Fatalf("timeout did not select normal jobs without permanent opt-out: %+v", state)
	}
	f.plan(t)
	f.reconcile(t, nil)
	_, state = f.gate(t)
	if state.Plan.Source != "timeout" || len(state.Plan.Jobs) != 2 {
		t.Fatal("late Chai plan replaced a timeout decision")
	}
}

func TestAgenticEmptyPlanIsExplicitAndAuthorized(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "lgtm"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgenticFixture(t, mode)
			f.plan(t)
			f.reconcile(t, nil)
			check, _ := f.gate(t)
			require.NotEqual(t, "success", check.Conclusion, "empty plan bypassed first-stage success")
			f.passFirstStage(t)
			f.reconcile(t, nil)
			check, _ = f.gate(t)
			if mode != "auto" && check.Conclusion == "success" {
				t.Fatal("empty plan bypassed trigger authorization")
			}
			if mode == "lgtm" {
				f.gh.pr.Labels = []github.Label{{Name: "lgtm"}}
			}
			var command *github.IssueComment
			if mode == "manual" {
				command = f.command(500, "remaining")
			}
			f.reconcile(t, command)
			check, _ = f.gate(t)
			if check.Conclusion != "success" || f.jobs.creates != 0 {
				t.Fatal("explicit empty plan did not settle after prerequisites")
			}
		})
	}
}

func TestAgenticPartialDispatchRecovery(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a", "job-b")
	f.jobs.failAt = 2
	require.Error(t, f.tryReconcile(nil), "expected partial-dispatch failure")
	check, before := f.gate(t)
	if check.Conclusion != "failure" || len(f.allJobs(t)) != 1 {
		t.Fatal("failure was not recorded after partial dispatch")
	}
	f.jobs.failAt = 0
	f.reconcile(t, nil)
	_, after := f.gate(t)
	if len(f.allJobs(t)) != 2 || f.jobs.creates != 3 || !reflect.DeepEqual(before.Dispatch, after.Dispatch) {
		t.Fatal("recovery duplicated work or changed the persisted execution identities")
	}
}

func TestAgenticRerunClosesSameGateAndRejectsOldReports(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.reconcile(t, nil)
	f.report(t, v1.PendingState)
	f.reconcile(t, nil)
	check, old := f.gate(t)
	gateID := check.ID
	f.now = f.now.Add(time.Minute)
	f.jobs.beforeCreate = func(pj *v1.ProwJob) {
		check, _ := f.gate(t)
		if check.ID != gateID || check.Status != "in_progress" || check.Conclusion == "success" {
			t.Fatalf("rerun created with a stale green gate: %+v", check)
		}
	}
	f.reconcile(t, f.command(500, "required"))
	check, next := f.gate(t)
	if len(f.gh.checks) != 1 || check.ID != gateID || check.Status != "in_progress" || next.Dispatch.ID == old.Dispatch.ID || next.Dispatch.Executions[0].Reported {
		t.Fatal("rerun accepted an older execution or left duplicate green gates")
	}
	f.reconcile(t, nil)
	check, _ = f.gate(t)
	require.NotEqual(t, "success", check.Conclusion, "older same-context report satisfied the new execution")
}

func TestAgenticReviewRequestCorrelationAndCrashRecovery(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	f.gh.failCommentAfterWrite = true
	require.Error(t, f.tryReconcile(f.command(500, "agent-review")), "expected lost comment response")
	f.reconcile(t, nil)
	_, state := f.gate(t)
	if state.Review == nil || !state.ReviewPosted || state.Plan != nil || len(f.gh.comments) != 4 {
		t.Fatalf("fresh review failed to correlate/deduplicate: %+v; comments=%d", state, len(f.gh.comments))
	}
	f.passFirstStage(t)
	// A new same-HEAD automatic reply is still not a response to this request.
	f.plan(t, "job-b")
	f.reconcile(t, f.command(501, "remaining"))
	_, state = f.gate(t)
	if state.Plan != nil || f.jobs.creates != 0 {
		t.Fatal("uncorrelated reply satisfied a fresh review request")
	}
	body := formatAgenticPlan(agenticPlan{HeadSHA: f.gh.pr.Head.SHA, BaseBranch: "main", Jobs: []string{"job-b"}, Rationale: "Refreshed selection.", RequestID: state.Review.RequestID})
	f.gh.comments = append(f.gh.comments, github.IssueComment{ID: 2000, Body: body, User: github.User{Login: "chai[bot]"}, CreatedAt: f.now})
	f.reconcile(t, nil)
	if f.jobs.creates != 1 || f.allJobs(t)[0].Spec.Job != "job-b" {
		t.Fatal("correlated reply was not dispatched")
	}
}

func TestAgenticNoDispatchWithoutDurableGate(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a")
	f.gh.failCheck = true
	require.Error(t, f.tryReconcile(nil), "expected gate persistence failure")
	require.Zero(t, f.jobs.creates, "dispatched jobs before recording their identities")
}

func TestAgenticFirstStageNewerRunWinsOverOldSuccess(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto")
	pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, f.cfg.GetPresubmitsStatic("org/repo")[0], "first", nil)
	pj.Name, pj.Namespace, pj.CreationTimestamp = "new-first-stage", "ci", metav1.NewTime(f.now)
	pj.Status.State = v1.PendingState
	require.NoError(t, f.jobs.Client.Create(context.Background(), &pj))
	f.reconcile(t, nil)
	check, _ := f.gate(t)
	require.NotEqual(t, "success", check.Conclusion, "old GitHub success bypassed a newer pending first-stage execution")
}
