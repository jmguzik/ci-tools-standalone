package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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
	changes               []github.PullRequestChange
	member                bool
	collaborator          bool
	statusWrites          int
	checkWrites           []github.CheckRun
	failCheck             bool
	failCheckAfterCreate  bool
	failCommentAfterWrite bool
	otherPRs              []github.PullRequest
	checkAttempts         int
	getPullRequestError   error
	getPullRequestCalls   int
	getPullRequestsError  error
	getPullRequestsCalls  int
	beforePullRequest     func(int)
	hook                  *agenticFixture
	beforeComment         func(string)
	afterCheckWrite       func(github.CheckRun)
	listCheckRunsCalls    int
	listCommentsCalls     int
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
	if f.beforeComment != nil {
		f.beforeComment(body)
	}
	f.comments = append(f.comments, github.IssueComment{ID: f.nextCommentID(), Body: body, User: github.User{Login: "controller[bot]"}})
	if f.hook != nil && f.hook.autoHook {
		f.hook.processHook(body)
	}
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

func (f *fakeAgenticGitHub) ListIssueComments(_, _ string, _ int) ([]github.IssueComment, error) {
	f.listCommentsCalls++
	return append([]github.IssueComment{}, f.comments...), nil
}
func (f *fakeAgenticGitHub) GetPullRequestChanges(_, _ string, _ int) ([]github.PullRequestChange, error) {
	return f.changes, nil
}
func (f *fakeAgenticGitHub) CreateStatus(_, _, _ string, _ github.Status) error {
	f.statusWrites++
	return nil
}
func (f *fakeAgenticGitHub) AddLabel(_, _ string, _ int, label string) error {
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
	f.listCheckRunsCalls++
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
	if f.failCheck {
		return 0, fmt.Errorf("check write failed: %w", io.ErrUnexpectedEOF)
	}
	check.ID, check.App.ID = int64(len(f.checks)+1), 101
	f.checks = append(f.checks, check)
	f.checkWrites = append(f.checkWrites, check)
	if f.afterCheckWrite != nil {
		f.afterCheckWrite(check)
	}
	if f.failCheckAfterCreate {
		f.failCheckAfterCreate = false
		return 0, fmt.Errorf("response lost after creating check: %w", io.ErrUnexpectedEOF)
	}
	return check.ID, nil
}
func (f *fakeAgenticGitHub) UpdateCheckRun(_, _ string, id int64, check github.CheckRun) error {
	f.checkAttempts++
	if f.failCheck {
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
		text := old.Output.Text
		old.Output = check.Output
		if check.Output.Text == "" {
			old.Output.Text = text
		} // SDK omits an empty text field in PATCH.
		f.checks[i] = old
		f.checkWrites = append(f.checkWrites, old)
		if f.afterCheckWrite != nil {
			f.afterCheckWrite(old)
		}
		return nil
	}
	return errors.New("unknown check")
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
	creates int
}

func (j *agenticTestJobs) Create(ctx context.Context, object ctrlruntimeclient.Object, options ...ctrlruntimeclient.CreateOption) error {
	j.creates++
	return j.Client.Create(ctx, object, options...)
}

type agenticFixture struct {
	t        *testing.T
	a        *agenticController
	gh       *fakeAgenticGitHub
	jobs     *agenticTestJobs
	cfg      *config.Config
	now      time.Time
	autoHook bool
}

func newAgenticFixture(t *testing.T, mode string) *agenticFixture {
	t.Helper()
	f := &agenticFixture{t: t, now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), autoHook: true}
	static := []config.Presubmit{
		{JobBase: config.JobBase{Name: "first-stage", Agent: "kubernetes"}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/first"}},
		{JobBase: config.JobBase{Name: "job-a", Agent: "kubernetes", Annotations: map[string]string{"pipeline_run_if_changed": "^src/"}, Labels: map[string]string{"configured-label": "kept"}}, Reporter: config.Reporter{Context: "ci/job-a"}, Brancher: config.Brancher{Branches: []string{"^main$"}}},
		{JobBase: config.JobBase{Name: "job-b", Agent: "kubernetes"}, Reporter: config.Reporter{Context: "ci/job-b"}},
		{JobBase: config.JobBase{Name: "optional-job", Agent: "kubernetes", Annotations: map[string]string{"pipeline_run_if_changed": "^docs/"}}, Optional: true, Reporter: config.Reporter{Context: "ci/optional"}},
	}
	for i := range static {
		static[i].RerunCommand = config.DefaultRerunCommandFor(static[i].Name)
	}
	f.cfg = &config.Config{ProwConfig: config.ProwConfig{ProwJobNamespace: "ci"}}
	require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": static}))
	f.gh = &fakeAgenticGitHub{member: true, changes: []github.PullRequestChange{{Filename: "src/main.go"}},
		pr: github.PullRequest{Number: 42, State: github.PullRequestStateOpen, CreatedAt: f.now.Add(-time.Hour),
			Head: github.PullRequestBranch{SHA: strings.Repeat("a", 40)}, Base: github.PullRequestBranch{Ref: "main", SHA: strings.Repeat("b", 40), Repo: github.Repo{Name: "repo", Owner: github.User{Login: "org"}, HTMLURL: "https://github.com/org/repo"}}}}
	var enrollment enabledConfig
	require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf("orgs:\n- org: org\n  repos:\n  - name: repo\n    branches: [main, release]\n    mode:\n      trigger: %s\n      agentic:\n        mode: chai\n", mode)), &enrollment))
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	f.jobs = &agenticTestJobs{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	f.a = &agenticController{gh: f.gh, reader: f.jobs, apiReader: f.jobs, config: func() *config.Config { return f.cfg },
		watcher: &watcher{config: enrollment}, lgtmWatcher: &watcher{}, appID: 101, now: func() time.Time { return f.now }, logger: logrus.NewEntry(logrus.New()),
		options: agenticOptions{timeout: defaultAgenticTimeout, trustedAuthors: flagutil.NewStrings("chai[bot]"), stateDir: t.TempDir()}}
	// Most tests use an immediate fake Hook; handoff tests disable it to
	// exercise asynchronous delivery explicitly. Only Hook creates ProwJobs.
	f.gh.hook = f
	t.Cleanup(func() { require.NoError(t, f.a.closeStore()) })
	return f
}

func (f *agenticFixture) processHook(body string) {
	for _, definition := range f.cfg.GetPresubmitsStatic("org/repo") {
		if definition.RerunCommand == "" || !slices.Contains(strings.Split(body, "\n"), definition.RerunCommand) {
			continue
		}
		pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, definition, "hook", nil)
		pj.Name, pj.Namespace = fmt.Sprintf("hook-%d-%s", f.gh.comments[len(f.gh.comments)-1].ID, definition.Name), "ci"
		pj.CreationTimestamp = metav1.NewTime(f.now)
		require.NoError(f.t, f.jobs.Create(context.Background(), &pj))
	}
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
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	var deadline time.Time
	return f.a.reconcilePull(context.Background(), "org", "repo", f.gh.pr.Number, command, &deadline)
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
}

func (f *agenticFixture) report(t *testing.T, state v1.ProwJobState) {
	t.Helper()
	for _, pj := range f.allJobs(t) {
		pj.Status.State = state
		pj.Status.URL = ""
		pj.Status.PrevReportStates = nil
		require.NoError(t, f.jobs.Update(context.Background(), &pj))
	}
}

func TestAgenticReadinessThreshold(t *testing.T) {
	for _, test := range []struct {
		name     string
		states   []v1.ProwJobState
		active   bool
		selected bool
	}{
		{"missing", nil, false, false},
		{"one-of-three", []v1.ProwJobState{v1.SuccessState, v1.PendingState, v1.PendingState}, false, false},
		{"rounded-up-half", []v1.ProwJobState{v1.SuccessState, v1.SuccessState, v1.PendingState}, true, false},
		{"failure-veto", []v1.ProwJobState{v1.SuccessState, v1.SuccessState, v1.FailureState}, false, false},
		{"abort-veto", []v1.ProwJobState{v1.SuccessState, v1.SuccessState, v1.AbortedState}, false, false},
		{"selected-awaits-all-green", []v1.ProwJobState{v1.SuccessState, v1.SuccessState, v1.PendingState}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			if test.selected {
				f.plan(t, "job-a")
			}
			static := f.cfg.GetPresubmitsStatic("org/repo")
			for _, name := range []string{"first-b", "first-c"} {
				p := static[0]
				p.Name, p.Context = name, "ci/"+name
				static = append(static, p)
			}
			require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": static}))
			first := []config.Presubmit{static[0], static[4], static[5]}
			for i, state := range test.states {
				pj := pjutil.NewPresubmit(f.gh.pr, f.gh.pr.Base.SHA, first[i], "first", nil)
				pj.Name, pj.Namespace = first[i].Name, "ci"
				pj.CreationTimestamp = metav1.NewTime(f.now)
				pj.Status.State = state
				require.NoError(t, f.jobs.Client.Create(context.Background(), &pj))
				if state != v1.SuccessState {
					stale := pj.DeepCopy()
					stale.Name, stale.ResourceVersion, stale.Status.State = "z-old-"+pj.Name, "", v1.SuccessState
					require.NoError(t, f.jobs.Client.Create(context.Background(), stale))
				}
			}
			f.reconcile(t, nil)
			_, state := f.gate(t)
			require.Equal(t, test.active, state.ActivatedAt != nil)
			require.Equal(t, test.active && !test.selected, state.WaitingSince != nil)
			require.Nil(t, state.Dispatch, "threshold must not dispatch before selection/all prerequisites")
			if test.active {
				require.Len(t, f.gh.checks, 1)
				check := f.gh.checks[0]
				require.Equal(t, "in_progress", check.Status)
				if !test.selected {
					require.Equal(t, "Waiting for Chai", check.Output.Title)
				}
				require.Contains(t, check.Output.Summary, "Pipeline for `"+f.gh.pr.Head.SHA+"` → `main`.")
				require.Equal(t, agenticExternalID("org", "repo", 42), check.ExternalID)
			} else {
				require.Empty(t, f.gh.checks, "do not consume the creation event before readiness")
			}
			if test.selected {
				var pj v1.ProwJob
				require.NoError(t, f.jobs.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: "ci", Name: "first-c"}, &pj))
				pj.Status.State = v1.SuccessState
				require.NoError(t, f.jobs.Update(context.Background(), &pj))
				f.reconcile(t, nil)
				require.Equal(t, 1, f.jobs.creates)
			}
		})
	}
}

func TestAgenticGateUsesSelectedProwJobResults(t *testing.T) {
	for _, state := range []v1.ProwJobState{v1.PendingState, v1.FailureState, v1.ErrorState, v1.AbortedState, v1.SuccessState} {
		t.Run(string(state), func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			require.Equal(t, 1, f.jobs.creates, "only Hook should create the selected job")
			require.Zero(t, f.gh.statusWrites, "controller must not publish job placeholders")
			f.report(t, state)
			// A tied old success must not hide this run's pending/failing state.
			stale := f.allJobs(t)[0].DeepCopy()
			stale.Name, stale.ResourceVersion, stale.Status.State = "z-old-success", "", v1.SuccessState
			require.NoError(t, f.jobs.Client.Create(context.Background(), stale))
			f.reconcile(t, nil)
			gate, saved := f.gate(t)
			require.Equal(t, state == v1.SuccessState, saved.Dispatch.Executions[0].Passed)
			switch state {
			case v1.PendingState:
				require.Equal(t, "in_progress", gate.Status)
			case v1.SuccessState:
				require.Equal(t, "success", gate.Conclusion)
			default:
				require.Equal(t, "failure", gate.Conclusion)
			}
			if state != v1.SuccessState {
				stale.Name, stale.ResourceVersion = "a-new-success", ""
				stale.CreationTimestamp = metav1.NewTime(f.now.Add(time.Second))
				require.NoError(t, f.jobs.Client.Create(context.Background(), stale))
				f.reconcile(t, nil)
				gate, _ = f.gate(t)
				require.Equal(t, "success", gate.Conclusion, "an unambiguously newer success must release the gate")
			}
		})
	}
}

func TestAgenticSelectedResultsRequireCurrentIdentity(t *testing.T) {
	for _, wrong := range []string{"sha", "base", "context", "job"} {
		t.Run(wrong, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			pj := f.allJobs(t)[0]
			pj.Status.State = v1.SuccessState
			switch wrong {
			case "sha":
				pj.Spec.Refs.Pulls[0].SHA = strings.Repeat("c", 40)
			case "base":
				pj.Spec.Refs.BaseRef = "release"
			case "context":
				pj.Spec.Context = "other-context"
			case "job":
				pj.Spec.Job = "other-job"
			}
			require.NoError(t, f.jobs.Update(context.Background(), &pj))
			f.reconcile(t, nil)
			gate, _ := f.gate(t)
			require.NotEqual(t, "success", gate.Conclusion)
		})
	}
}

func TestAgenticTimeoutStartsAtReadinessAndLocksFallback(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "lgtm"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgenticFixture(t, mode)
			f.reconcile(t, nil)
			f.now = f.now.Add(time.Hour)
			f.reconcile(t, nil)
			_, state := f.gate(t)
			require.Nil(t, state.WaitingSince)
			require.Nil(t, state.Plan)
			require.Empty(t, f.gh.checks)
			f.passFirstStage(t)
			f.reconcile(t, nil)
			_, state = f.gate(t)
			started := *state.WaitingSince
			require.Equal(t, f.now, started)
			f.now = f.now.Add(defaultAgenticTimeout - time.Second)
			f.reconcile(t, nil)
			_, state = f.gate(t)
			require.Nil(t, state.Plan)
			require.Equal(t, started, *state.WaitingSince)
			f.now = f.now.Add(time.Second)
			f.reconcile(t, nil)
			_, state = f.gate(t)
			require.Equal(t, "timeout", state.Plan.Source)
			require.Len(t, state.Plan.Jobs, 2)
			require.False(t, hasAgenticLabel(&f.gh.pr, agenticSkipLabel))
			if mode != "auto" {
				require.Zero(t, f.jobs.creates)
			}
			require.NoError(t, f.a.closeStore())
			f.plan(t) // Late empty Chai replies must not replace fallback.
			switch mode {
			case "manual":
				f.reconcile(t, f.command(500, "remaining"))
			case "lgtm":
				f.gh.pr.Labels = []github.Label{{Name: "lgtm"}}
				f.reconcile(t, nil)
			default:
				f.reconcile(t, nil)
			}
			_, state = f.gate(t)
			require.Equal(t, "timeout", state.Plan.Source)
			require.Equal(t, 2, f.jobs.creates)
		})
	}
}

func TestAgenticEmptyPlanStillRequiresCIAndAuthorization(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "lgtm"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgenticFixture(t, mode)
			f.plan(t)
			f.reconcile(t, nil)
			require.Empty(t, f.gh.checks)
			f.passFirstStage(t)
			f.reconcile(t, nil)
			gate, _ := f.gate(t)
			if mode != "auto" {
				require.NotEqual(t, "success", gate.Conclusion)
			}
			switch mode {
			case "manual":
				f.reconcile(t, f.command(500, "remaining"))
			case "lgtm":
				f.gh.pr.Labels = []github.Label{{Name: "lgtm"}}
				f.reconcile(t, nil)
			}
			gate, _ = f.gate(t)
			require.Equal(t, "success", gate.Conclusion)
			require.Zero(t, f.jobs.creates)
		})
	}
}

func TestAgenticEarlyCommandsAreRejectedNotReplayed(t *testing.T) {
	for _, command := range []string{"required", "remaining"} {
		t.Run(command, func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			f.reconcile(t, nil)
			missed := f.command(500, command)
			request := f.command(600, command)
			f.reconcile(t, request)
			_, state := f.gate(t)
			require.True(t, state.Command.Rejected)
			require.Zero(t, state.ManualRequestID)
			require.Contains(t, f.gh.comments[len(f.gh.comments)-1].Body, "This request is not queued.")
			replies := len(f.gh.comments)
			f.plan(t, "job-a")
			f.passFirstStage(t)
			require.NoError(t, f.a.closeStore())
			f.reconcile(t, missed)
			f.reconcile(t, request)
			require.Zero(t, f.jobs.creates)
			require.Len(t, f.gh.comments, replies+1, "redelivery duplicated the rejection")
			f.reconcile(t, f.command(700, command))
			require.Equal(t, 1, f.jobs.creates)
		})
	}
}

func TestAgenticManualRequestDoesNotAuthorizeNewHead(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	request := f.command(500, "required")
	f.reconcile(t, request)
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.reconcile(t, request)
	require.Equal(t, 1, f.jobs.creates)
	oldGate, _ := f.gate(t)
	f.gh.pr.Head.SHA = strings.Repeat("c", 40)
	f.now = f.now.Add(time.Minute)
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 1, "new HEAD created a gate before readiness")
	f.passFirstStage(t)
	f.plan(t, "job-a")
	f.reconcile(t, request)
	gate, state := f.gate(t)
	require.NotEqual(t, oldGate.ID, gate.ID)
	require.Zero(t, state.ManualRequestID)
	require.Nil(t, state.Dispatch)
	require.Equal(t, 1, f.jobs.creates)
}

func TestAgenticForcedRerunClosesGateAndExcludesOldRuns(t *testing.T) {
	for _, lag := range []string{"created", "deleted"} {
		t.Run(lag, func(t *testing.T) {
			f := newReadyAgenticFixture(t, "auto", "job-a")
			f.reconcile(t, nil)
			f.report(t, v1.SuccessState)
			f.reconcile(t, nil)
			gate, old := f.gate(t)
			oldJobs := f.allJobs(t)
			var cachedJobs v1.ProwJobList
			require.NoError(t, f.jobs.List(context.Background(), &cachedJobs))
			if lag == "created" {
				cachedJobs.Items = slices.DeleteFunc(cachedJobs.Items, func(pj v1.ProwJob) bool { return pj.Spec.Job != "first-stage" })
			} else {
				f.deleteJobs(t, oldJobs...) // The informer still has the deleted green run.
			}
			cache := fake.NewClientBuilder().WithScheme(f.jobs.Scheme()).WithLists(&cachedJobs).Build()
			f.a.reader = cache
			f.gh.beforeComment = func(string) { require.NotEqual(t, "success", f.gh.checks[0].Conclusion) }
			f.reconcile(t, f.command(500, "required"))
			current, state := f.gate(t)
			require.Len(t, f.gh.checks, 1)
			require.Equal(t, gate.ID, current.ID)
			require.NotEqual(t, old.Dispatch.ID, state.Dispatch.ID)
			require.Contains(t, state.Dispatch.Executions[0].Previous, oldJobs[0].Name)
			require.NotEqual(t, "success", current.Conclusion)
			if lag == "created" {
				delayed := oldJobs[0].DeepCopy()
				delayed.ResourceVersion = ""
				require.NoError(t, cache.Create(context.Background(), delayed))
			}
			f.reconcile(t, nil)
			current, _ = f.gate(t)
			require.NotEqual(t, "success", current.Conclusion, "cache lag adopted a pre-rerun success")
			// If the new pending run disappears, an older green run still cannot pass.
			for _, pj := range f.allJobs(t) {
				if pj.Name != oldJobs[0].Name {
					f.deleteJobs(t, pj)
				}
			}
			require.NoError(t, f.a.closeStore())
			f.reconcile(t, nil)
			current, _ = f.gate(t)
			require.NotEqual(t, "success", current.Conclusion)
		})
	}
}

func TestAgenticRemainingRequestsOnlyMissingExecutions(t *testing.T) {
	f := newReadyAgenticFixture(t, "auto", "job-a", "job-b")
	f.autoHook = false
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "required"))
	f.processHook("/test job-a")
	f.report(t, v1.SuccessState)
	f.reconcile(t, f.command(501, "remaining"))
	body := f.gh.comments[len(f.gh.comments)-1].Body
	require.Contains(t, body, "\n/test job-b\n")
	require.NotContains(t, body, "/test job-a")
	f.processHook(body)
	f.report(t, v1.SuccessState)
	f.reconcile(t, nil)
	gate, _ := f.gate(t)
	require.Equal(t, "success", gate.Conclusion)
}

func TestAgenticExplicitReviewIsDelayedAndCorrelated(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.reconcile(t, nil)
	f.reconcile(t, f.command(500, "agent-review"))
	_, state := f.gate(t)
	require.Nil(t, state.WaitingSince)
	require.False(t, state.ReviewPosted)
	f.passFirstStage(t)
	f.gh.failCommentAfterWrite = true
	require.Error(t, f.tryReconcile(nil))
	require.NoError(t, f.a.closeStore())
	f.reconcile(t, nil)
	_, state = f.gate(t)
	require.True(t, state.ReviewPosted)
	require.Len(t, f.gh.comments, 2, "lost response duplicated the review request")
	f.plan(t, "job-a")
	f.reconcile(t, nil)
	_, state = f.gate(t)
	require.Nil(t, state.Plan, "uncorrelated reply satisfied an explicit review")
	comment := f.plan(t, "job-b")
	f.gh.comments[len(f.gh.comments)-1].Body = formatAgenticPlan(agenticPlan{
		HeadSHA: state.HeadSHA, BaseBranch: state.BaseBranch, Jobs: []string{"job-b"}, RequestID: state.Review.RequestID})
	f.reconcile(t, &comment)
	f.reconcile(t, f.command(700, "remaining"))
	require.Equal(t, 1, f.jobs.creates)
	require.Equal(t, "job-b", f.allJobs(t)[0].Spec.Job)
}
