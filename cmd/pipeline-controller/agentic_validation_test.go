package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw, trigger string
		enabled            bool
		wantErr            bool
	}{
		{name: "legacy string", raw: "repo", trigger: "auto"},
		{name: "legacy object", raw: "name: repo\nmode: {trigger: manual}", trigger: "manual"},
		{name: "default trigger", raw: "name: repo\nmode: {agentic: {mode: chai}}", trigger: "auto", enabled: true},
		{name: "manual", raw: "name: repo\nmode: {trigger: manual, agentic: {mode: chai}}", trigger: "manual", enabled: true},
		{name: "lgtm", raw: "name: repo\nmode: {trigger: lgtm, agentic: {mode: chai}}", trigger: "lgtm", enabled: true},
		{name: "unknown selector", raw: "name: repo\nmode: {agentic: {mode: unknown}}", wantErr: true},
		{name: "unknown trigger", raw: "name: repo\nmode: {trigger: queued, agentic: {mode: chai}}", wantErr: true},
		{name: "timeout is global", raw: "name: repo\nmode: {agentic: {mode: chai, timeout: 5m}}", wantErr: true},
		{name: "trust is global", raw: "name: repo\nmode: {agentic: {mode: chai, trusted_authors: [chai]}}", wantErr: true},
		{name: "unknown setting", raw: "name: repo\nmode: {agentic: {mode: chai, extra: true}}", wantErr: true},
		{name: "boolean is not selector block", raw: "name: repo\nmode: {agentic: true}", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var item RepoItem
			err := yaml.Unmarshal([]byte(tc.raw), &item)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted invalid agentic configuration")
				}
				return
			}
			require.NoError(t, err)
			if item.Name != "repo" || item.Mode.Trigger != tc.trigger || item.Mode.Agentic.enabled() != tc.enabled {
				t.Fatalf("unexpected repository configuration: %+v", item)
			}
		})
	}
}

func TestAgenticConfigReloadKeepsLastGoodConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	}
	write("orgs:\n- org: org\n  repos:\n  - name: repo\n    branches: [main]\n    mode:\n      trigger: manual\n      agentic:\n        mode: chai\n")
	w := newWatcher(path, nil)
	require.NoError(t, w.reloadConfig())
	before := w.getConfig()
	write("orgs:\n- org: org\n  repos:\n  - name: repo\n    mode:\n      agentic:\n        mode: chai\n        trusted_authors: [untrusted]\n")
	if err := w.reloadConfig(); err == nil {
		t.Fatal("expected invalid reload to fail")
	}
	if after := w.getConfig(); !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid reload replaced live configuration: %+v", after)
	}
	if got := before["org"]["repo"]; !got.Agentic.enabled() || got.Trigger != "manual" || !reflect.DeepEqual(got.Branches, []string{"main"}) {
		t.Fatalf("watcher lost agentic mode or branch scope: %+v", got)
	}
}

func TestAgenticMetadataEnvelope(t *testing.T) {
	revision := agenticRevision{HeadSHA: strings.Repeat("a", 40), BaseBranch: "main", ObservedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), RevisionID: "revision"}
	body, err := agenticMetadata(agenticRevisionMarker, revision)
	require.NoError(t, err)
	var decoded agenticRevision
	if err := parseAgenticMetadata("Controller tracking comment\n\n"+body+"\n", agenticRevisionMarker, &decoded); err != nil || !reflect.DeepEqual(decoded, revision) {
		t.Fatalf("controller journal did not round-trip: %+v / %v", decoded, err)
	}
	for _, tc := range []struct{ name, body string }{
		{"missing marker", "Run protected."},
		{"two markers", body + "\n" + body},
		{"unterminated", strings.TrimSuffix(body, "-->")},
		{"invalid JSON", agenticRevisionMarker + "\n{broken}\n-->"},
		{"unknown field", agenticRevisionMarker + "\n{\"head_sha\":\"a\",\"surprise\":true}\n-->"},
		{"two JSON objects", agenticRevisionMarker + "\n{} {}\n-->"},
		{"array instead of object", agenticRevisionMarker + "\n[]\n-->"},
		{"oversize", strings.Repeat("x", 64*1024) + body},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got agenticRevision
			if err := parseAgenticMetadata(tc.body, agenticRevisionMarker, &got); err == nil {
				t.Fatal("accepted malformed or ambiguous metadata")
			}
		})
	}
}

func TestAgenticTrustedAuthorUsesLogin(t *testing.T) {
	authors := []string{"chai[bot]"}
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{"chai[bot]", true},
		{"CHAI[bot]", true},
		{"chai", false},
		{"chai-bot", false},
		{"untrusted[bot]", false},
		{"", false},
	} {
		if got := trustedAgenticAuthor(authors, github.User{Login: tc.login, Type: github.UserTypeBot}); got != tc.want {
			t.Errorf("trusted(%q) = %t, want %t", tc.login, got, tc.want)
		}
	}
	if trustedAgenticAuthor(nil, github.User{Login: "chai[bot]"}) {
		t.Fatal("an absent allowlist trusted a default bot")
	}
}

func agenticValidationJobs(t *testing.T) []config.Presubmit {
	t.Helper()
	jobs := []config.Presubmit{
		{JobBase: config.JobBase{Name: "protected"}, Reporter: config.Reporter{Context: "ci/protected"}},
		{JobBase: config.JobBase{Name: "annotated", Annotations: map[string]string{"pipeline_run_if_changed": "^src/"}}, Optional: true, Reporter: config.Reporter{Context: "ci/annotated"}},
		{JobBase: config.JobBase{Name: "skip", Annotations: map[string]string{"pipeline_skip_if_only_changed": "^docs/"}}, Reporter: config.Reporter{Context: "ci/skip"}},
		{JobBase: config.JobBase{Name: "optional"}, Optional: true, Reporter: config.Reporter{Context: "ci/optional"}},
		{JobBase: config.JobBase{Name: "first"}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/first"}},
		{JobBase: config.JobBase{Name: "native-run"}, RegexpChangeMatcher: config.RegexpChangeMatcher{RunIfChanged: "^src/"}, Reporter: config.Reporter{Context: "ci/native-run"}},
		{JobBase: config.JobBase{Name: "native-skip"}, RegexpChangeMatcher: config.RegexpChangeMatcher{SkipIfOnlyChanged: "^docs/"}, Reporter: config.Reporter{Context: "ci/native-skip"}},
		{JobBase: config.JobBase{Name: "unreported"}, Reporter: config.Reporter{Context: "ci/unreported", SkipReport: true}},
		{JobBase: config.JobBase{Name: "empty-annotation", Annotations: map[string]string{"pipeline_run_if_changed": ""}}, Reporter: config.Reporter{Context: "ci/empty"}},
		{JobBase: config.JobBase{Name: "release"}, Brancher: config.Brancher{Branches: []string{"^release$"}}, Reporter: config.Reporter{Context: "ci/release"}},
	}
	require.NoError(t, config.SetPresubmitRegexes(jobs))
	return jobs
}

func TestAgenticResolveJobsValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		names   []string
		mutate  func([]config.Presubmit) []config.Presubmit
		wantErr bool
	}{
		{name: "protected and annotated", names: []string{"skip", "protected", "annotated"}},
		{name: "explicit empty", names: []string{}},
		{name: "missing array", wantErr: true},
		{name: "unknown", names: []string{"missing"}, wantErr: true},
		{name: "wrong branch", names: []string{"release"}, wantErr: true},
		{name: "first stage", names: []string{"first"}, wantErr: true},
		{name: "native run matcher", names: []string{"native-run"}, wantErr: true},
		{name: "native skip matcher", names: []string{"native-skip"}, wantErr: true},
		{name: "nonreporting", names: []string{"unreported"}, wantErr: true},
		{name: "optional outside pipeline", names: []string{"optional"}, wantErr: true},
		{name: "empty pipeline annotation", names: []string{"empty-annotation"}, wantErr: true},
		{name: "duplicate names", names: []string{"protected", "protected"}, wantErr: true},
		{name: "too many jobs", names: make([]string, 257), wantErr: true},
		{name: "ambiguous definition", names: []string{"protected"}, mutate: func(j []config.Presubmit) []config.Presubmit { return append(j, j[0]) }, wantErr: true},
		{name: "reserved context", names: []string{"protected"}, mutate: func(j []config.Presubmit) []config.Presubmit { j[0].Context = agenticGate; return j }, wantErr: true},
		{name: "missing context", names: []string{"protected"}, mutate: func(j []config.Presubmit) []config.Presubmit { j[0].Context = ""; return j }, wantErr: true},
		{name: "unselected context collision", names: []string{"protected"}, mutate: func(j []config.Presubmit) []config.Presubmit { j[1].Context = j[0].Context; return j }, wantErr: true},
		{name: "other branch context is independent", names: []string{"protected"}, mutate: func(j []config.Presubmit) []config.Presubmit { j[9].Context = j[0].Context; return j }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			static := agenticValidationJobs(t)
			if tc.mutate != nil {
				static = tc.mutate(static)
			}
			got, err := resolveAgenticJobs(tc.names, static, "main")
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted an unsafe job selection")
				}
				return
			}
			require.NoError(t, err)
			if got == nil || len(got) != len(tc.names) {
				t.Fatalf("lost explicit job list: %+v", got)
			}
			for i := range got {
				if i > 0 && got[i-1].Name > got[i].Name {
					t.Fatal("resolved job order is not deterministic")
				}
				if got[i].Name == "annotated" && !got[i].Optional {
					t.Fatal("optional metadata was lost")
				}
			}
		})
	}
}

func TestAgenticReadPlanTrustAndIdentity(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	makeComment := func(id int, author string, plan agenticPlan) github.IssueComment {
		return github.IssueComment{ID: id, User: github.User{Login: author}, Body: formatAgenticPlan(plan)}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*agenticPlan)
		author  string
		wantErr bool
		wantID  int
	}{
		{name: "latest trusted", author: "chai", wantID: 20},
		{name: "untrusted newer comment", author: "other", wantID: 10},
		{name: "old head", author: "chai", mutate: func(p *agenticPlan) { p.HeadSHA = strings.Repeat("b", 40) }, wantID: 10},
		{name: "old base", author: "chai", mutate: func(p *agenticPlan) { p.BaseBranch = "release" }, wantID: 10},
		{name: "missing head", author: "chai", mutate: func(p *agenticPlan) { p.HeadSHA = "" }, wantErr: true},
		{name: "missing base", author: "chai", mutate: func(p *agenticPlan) { p.BaseBranch = "" }, wantErr: true},
		{name: "reason is optional", author: "chai", mutate: func(p *agenticPlan) { p.Rationale = "" }, wantID: 20},
		{name: "null jobs is not empty", author: "chai", mutate: func(p *agenticPlan) { p.Jobs = nil }, wantErr: true},
		{name: "explicit empty jobs", author: "chai", mutate: func(p *agenticPlan) { p.Jobs = []string{} }, wantID: 20},
		{name: "invalid newest does not use older valid plan", author: "chai", mutate: func(p *agenticPlan) { p.Jobs = []string{"unknown"} }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := agenticPlan{HeadSHA: head, BaseBranch: "main", Jobs: []string{"protected"}, Rationale: "Relevant changes."}
			comments := []github.IssueComment{makeComment(10, "chai", plan)}
			if tc.mutate != nil {
				tc.mutate(&plan)
			}
			comments = append(comments, makeComment(20, tc.author, plan))
			state := &agenticState{HeadSHA: head, BaseBranch: "main"}
			a := &agenticController{options: agenticOptions{trustedAuthors: flagutil.NewStrings("chai")}}
			err := a.readPlan(state, agenticValidationJobs(t), comments)
			if tc.wantErr {
				if err == nil || state.Plan != nil {
					t.Fatalf("invalid current plan was accepted: %+v / %v", state.Plan, err)
				}
				return
			}
			if err != nil || state.Plan == nil || state.Plan.CommentID != tc.wantID || state.Plan.Source != "chai" {
				t.Fatalf("unexpected accepted plan: %+v / %v", state.Plan, err)
			}
		})
	}
}

func TestAgenticFallbackSelectionRules(t *testing.T) {
	for _, tc := range []struct {
		name, branch string
		files, want  []string
	}{
		{"source", "main", []string{"src/main.go"}, []string{"annotated", "protected", "skip"}},
		{"docs only", "main", []string{"docs/readme.md"}, []string{"protected"}},
		{"mixed", "main", []string{"docs/readme.md", "other/file"}, []string{"protected", "skip"}},
		{"no files", "main", nil, []string{"protected"}},
		{"branch filter", "release", []string{"docs/readme.md"}, []string{"protected", "release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changes []github.PullRequestChange
			for _, name := range tc.files {
				changes = append(changes, github.PullRequestChange{Filename: name})
			}
			pr := &github.PullRequest{Number: 42, Base: github.PullRequestBranch{Ref: tc.branch}}
			jobs, err := normalAgenticSelection(agenticValidationJobs(t), pr, &fakeGhClient{changes: changes}, "org", "repo")
			require.NoError(t, err)
			names := make([]string, 0, len(jobs))
			for _, job := range jobs {
				names = append(names, job.Name)
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("fallback selection = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestAgenticFallbackRunAnnotationPrecedence(t *testing.T) {
	static := agenticValidationJobs(t)
	static[1].Annotations["pipeline_skip_if_only_changed"] = "^docs/"
	pr := &github.PullRequest{Number: 42, Base: github.PullRequestBranch{Ref: "main"}}
	jobs, err := normalAgenticSelection(static, pr, &fakeGhClient{changes: []github.PullRequestChange{{Filename: "other/file"}}}, "org", "repo")
	require.NoError(t, err)
	for _, job := range jobs {
		if job.Name == "annotated" {
			t.Fatal("skip annotation overrode the run annotation")
		}
	}
	static[1].Annotations["pipeline_run_if_changed"] = "["
	if _, err := normalAgenticSelection(static, pr, &fakeGhClient{}, "org", "repo"); err == nil {
		t.Fatal("invalid fallback matcher was silently accepted")
	}
	// The same job remains selectable by Chai: the controller validates the
	// static job, but must not evaluate its pipeline-selection expression.
	if _, err := resolveAgenticJobs([]string{"annotated"}, static, "main"); err != nil {
		t.Fatalf("Chai selection unexpectedly evaluated a file matcher: %v", err)
	}
}
