package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/prow/pkg/github"
)

// Chai is external; keep a representative producer here alongside literal
// contract tests so integrations exercise the same format as the documentation.
func formatAgenticPlan(plan agenticPlan) string {
	var body strings.Builder
	fmt.Fprintf(&body, "Chai test plan for `%s` → `%s`\n\n", plan.HeadSHA, plan.BaseBranch)
	if plan.Jobs != nil && len(plan.Jobs) == 0 {
		body.WriteString("None.\n")
	}
	for _, job := range plan.Jobs {
		fmt.Fprintf(&body, "- `%s`\n", job)
	}
	if plan.RequestID != "" {
		fmt.Fprintf(&body, "\nRequest: `%s`\n", plan.RequestID)
	}
	if plan.Rationale != "" {
		fmt.Fprintf(&body, "\nReason: %s\n", plan.Rationale)
	}
	return body.String()
}

func TestAgenticPlanMarkdown(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const request = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const header = "Chai test plan for `" + head + "` → `main`\n\n"
	for _, tc := range []struct {
		name, body, reason, request string
		jobs                        []string
		wantErr                     bool
	}{
		{name: "list only", body: header + "- `job-a`\n- `job-b`", jobs: []string{"job-a", "job-b"}},
		{name: "reason", body: header + "- `job-a`\n\nReason: Startup changed.", jobs: []string{"job-a"}, reason: "Startup changed."},
		{name: "explicit empty", body: header + "None.", jobs: []string{}},
		{name: "review reply", body: header + "- `job-a`\n\nRequest: `" + request + "`", jobs: []string{"job-a"}, request: request},
		{name: "line endings", body: strings.ReplaceAll(header+"- `job-a`\n", "\n", "\r\n"), jobs: []string{"job-a"}},
		{name: "ASCII arrow", body: strings.ReplaceAll(header, "→", "->") + "- `job-a`", jobs: []string{"job-a"}},
		{name: "missing header", body: "- `job-a`", wantErr: true},
		{name: "short SHA", body: strings.ReplaceAll(header, head, "aaaaaaa") + "- `job-a`", wantErr: true},
		{name: "missing base", body: strings.ReplaceAll(header, "`main`", "``") + "- `job-a`", wantErr: true},
		{name: "missing list", body: header, wantErr: true},
		{name: "reason is not an empty list", body: header + "Reason: Documentation only.", wantErr: true},
		{name: "empty and jobs", body: header + "None.\n- `job-a`", wantErr: true},
		{name: "jobs and empty", body: header + "- `job-a`\nNone.", wantErr: true},
		{name: "duplicate empty", body: header + "None.\nNone.", wantErr: true},
		{name: "empty name", body: header + "- ``", wantErr: true},
		{name: "malformed bullet", body: header + "- `job-a`\n- job-b", wantErr: true},
		{name: "extra prose", body: header + "- `job-a`\nRun job-b too.", wantErr: true},
		{name: "two plans", body: header + "- `job-a`\n\n" + header + "- `job-b`", wantErr: true},
		{name: "list after reason", body: header + "None.\nReason: None needed.\n- `job-a`", wantErr: true},
		{name: "duplicate reason", body: header + "None.\nReason: One.\nReason: Two.", wantErr: true},
		{name: "blank reason", body: header + "None.\nReason:  ", wantErr: true},
		{name: "invalid request", body: header + "None.\nRequest: `other`", wantErr: true},
		{name: "duplicate request", body: header + "None.\nRequest: `" + request + "`\nRequest: `" + request + "`", wantErr: true},
		{name: "hidden alternate list", body: header + "- `job-a`\n\n<!-- pipeline-controller:plan:v1\n{\"jobs\":[\"job-b\"]}\n-->", wantErr: true},
		{name: "oversize", body: header + strings.Repeat("x", 64*1024), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgenticPlan(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted an incomplete or ambiguous plan")
				}
				return
			}
			want := agenticPlan{HeadSHA: head, BaseBranch: "main", Jobs: tc.jobs, Rationale: tc.reason, RequestID: tc.request}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("plan = %+v, error = %v; want %+v", got, err, want)
			}
		})
	}
}

func TestAgenticReviewIsVisibleAndCorrelated(t *testing.T) {
	request := agenticReviewRequest{HeadSHA: strings.Repeat("a", 40), BaseBranch: "release/next", RequestID: strings.Repeat("b", 32)}
	body := formatAgenticReview(request)
	want := fmt.Sprintf("Chai test selection requested for `%s` → `release/next`.\n\nRequest: `%s`", request.HeadSHA, request.RequestID)
	if body != want || isAgenticPlanComment(body) {
		t.Fatalf("unexpected review request: %s", body)
	}
}

func TestAgenticCommentEventReadsVisibleList(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage(t)
	comment := f.plan(t, "job-a")
	comment.Body, _, _ = strings.Cut(comment.Body, "\n\nReason:")
	f.gh.comments[len(f.gh.comments)-1] = comment
	event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
		Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: comment}
	f.a.startQueue(t.Context())
	t.Cleanup(f.a.queue.ShutDown)
	for range 2 {
		if !f.a.handleIssueComment(f.a.logger, event) {
			t.Fatal("visible plan fell through to legacy comment handling")
		}
	}
	f.a.processNext(t.Context())
	if f.jobs.creates != 1 || f.allJobs(t)[0].Spec.Job != "job-a" {
		t.Fatal("plain list without JSON/reason was not dispatched idempotently")
	}
}

func TestAgenticDelayedCommentCannotUseCurrentPRMetadata(t *testing.T) {
	for _, change := range []string{"push", "retarget"} {
		t.Run(change, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			body := formatAgenticPlan(agenticPlan{HeadSHA: f.gh.pr.Head.SHA, BaseBranch: f.gh.pr.Base.Ref, Jobs: []string{"job-b"}})
			f.reconcile(t, nil)
			f.now = f.now.Add(time.Minute)
			if change == "push" {
				f.gh.pr.Head.SHA = strings.Repeat("c", 40)
			} else {
				f.gh.pr.Base.Ref = "release"
			}
			f.reconcile(t, nil)
			f.passFirstStage(t)
			// The comment is genuinely new, not just an old event delivery: Chai
			// finished its old analysis after the PR changed. Its timestamp/ID
			// and a live PR fetch cannot recover which revision it analyzed.
			f.now = f.now.Add(time.Second)
			comment := github.IssueComment{ID: 1000, Body: body, User: github.User{Login: "chai[bot]"}, CreatedAt: f.now}
			f.gh.comments = append(f.gh.comments, comment)
			event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
				Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: comment}
			f.a.handleIssueComment(f.a.logger, event)
			f.reconcile(t, nil) // A later reconciliation must reach the same decision.
			_, state := f.gate(t)
			if state.Plan != nil || f.jobs.creates != 0 {
				t.Fatal("delayed plan was incorrectly bound to the current PR")
			}
			f.plan(t, "job-b")
			f.reconcile(t, nil)
			if f.jobs.creates != 1 {
				t.Fatal("fresh matching plan was not accepted")
			}
		})
	}
}
