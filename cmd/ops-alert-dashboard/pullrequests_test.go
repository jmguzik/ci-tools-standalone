package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// pullRequestTestClock makes retry boundaries independent of wall time.
type pullRequestTestClock struct{ at time.Time }

// Now returns the test-owned instant.
func (c *pullRequestTestClock) Now() time.Time { return c.at }

// pullRequestHTTPResult supplies one bounded response without a real transport.
type pullRequestHTTPResult struct {
	body    string
	status  int
	headers http.Header
}

// pullRequestHTTPFake records requests and refuses unplanned traffic.
type pullRequestHTTPFake struct {
	results  []pullRequestHTTPResult
	requests []*http.Request
}

// Do implements the existing small HTTP boundary without network access.
func (f *pullRequestHTTPFake) Do(request *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, request)
	index := len(f.requests) - 1
	if index >= len(f.results) {
		return nil, fmt.Errorf("unexpected request")
	}
	result := f.results[index]
	status := result.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: result.headers, Body: io.NopCloser(strings.NewReader(result.body))}, nil
}

// pullRequestTestItem makes realistic search fixtures including User-typed bot accounts.
func pullRequestTestItem(number int, author, state, rawURL string) map[string]any {
	if rawURL == "" {
		rawURL = fmt.Sprintf("https://github.com/openshift/release/pull/%d", number)
	}
	return map[string]any{"number": number, "title": "Update CI image", "state": state, "html_url": rawURL, "user": map[string]any{"login": author, "type": "User"}, "pull_request": map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/openshift/release/pulls/%d", number)}, "created_at": "2020-01-01T00:00:00Z", "updated_at": "2026-10-28T00:00:00Z", "draft": true, "labels": []map[string]string{{"name": "area/ci-operator"}}}
}

// pullRequestTestPage encodes fixtures through the same JSON boundary as GitHub.
func pullRequestTestPage(t *testing.T, total int, incomplete bool, items []map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"total_count": total, "incomplete_results": incomplete, "items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestPullRequestsFiltering protects exact allowlists, PR identity and the absence of an age cutoff.
func TestPullRequestsFiltering(t *testing.T) {
	cases := []struct {
		name, author, state, rawURL string
		noPR                        bool
		wantNumbers                 []int
	}{
		{name: "configured User typed bot and old PR", author: "redhat-chai-bot", state: "open", wantNumbers: []int{42}},
		{name: "bot type or suffix does not grant membership", author: "other-bot", state: "open", wantNumbers: []int{}},
		{name: "closed response filtered", author: "redhat-chai-bot", state: "closed", wantNumbers: []int{}},
		{name: "other repository rejected", author: "redhat-chai-bot", state: "open", rawURL: "https://github.com/evil/release/pull/42", wantNumbers: []int{}},
		{name: "URL suffix rejected", author: "redhat-chai-bot", state: "open", rawURL: "https://github.com/openshift/release/pull/42?token=x", wantNumbers: []int{}},
		{name: "issue filtered", author: "redhat-chai-bot", state: "open", noPR: true, wantNumbers: []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := pullRequestTestItem(42, tc.author, tc.state, tc.rawURL)
			if tc.noPR {
				delete(item, "pull_request")
			}
			p, err := newPullRequests([]string{"redhat-chai-bot"}, "", false)
			if err != nil {
				t.Fatal(err)
			}
			fake := &pullRequestHTTPFake{results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 1, false, []map[string]any{item})}}}
			p.client = fake
			p.clock = &pullRequestTestClock{at: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
			if err := p.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot := p.Snapshot()
			numbers := []int{}
			for _, pr := range snapshot.PRs {
				numbers = append(numbers, pr.Number)
			}
			if diff := cmp.Diff(tc.wantNumbers, numbers); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
			if len(snapshot.PRs) > 0 {
				want := pullRequest{Number: 42, Title: "Update CI image", URL: "https://github.com/openshift/release/pull/42", Author: tc.author, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Draft: true, Labels: []string{"area/ci-operator"}}
				if diff := cmp.Diff(want, snapshot.PRs[0]); diff != "" {
					t.Errorf("unexpected result (-want +got):\n%s", diff)
				}
				snapshot.PRs[0].Labels[0] = "mutated"
				if diff := cmp.Diff([]string{"area/ci-operator"}, p.Snapshot().PRs[0].Labels); diff != "" {
					t.Errorf("unexpected cache mutation (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestPullRequestsPagination rejects every partial scan and only publishes complete evidence.
func TestPullRequestsPagination(t *testing.T) {
	first := []map[string]any{}
	for i := 1; i <= 100; i++ {
		first = append(first, pullRequestTestItem(i, "openshift-bot", "open", ""))
	}
	limitPages := []pullRequestHTTPResult{}
	for page := 0; page < 10; page++ {
		items := []map[string]any{}
		for number := page*100 + 1; number <= (page+1)*100; number++ {
			items = append(items, pullRequestTestItem(number, "openshift-bot", "open", ""))
		}
		limitPages = append(limitPages, pullRequestHTTPResult{body: pullRequestTestPage(t, 1000, false, items)})
	}
	cases := []struct {
		name      string
		results   []pullRequestHTTPResult
		wantCount int
		wantError string
	}{
		{name: "exact search limit is complete", results: limitPages, wantCount: 1000},
		{name: "complete pages", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 101, false, first)}, {body: pullRequestTestPage(t, 101, false, []map[string]any{pullRequestTestItem(101, "openshift-bot", "open", "")})}}, wantCount: 101},
		{name: "incomplete response", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 100, true, first)}}, wantError: "incomplete"},
		{name: "over search limit", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 1001, false, first)}}, wantError: "1000-result"},
		{name: "short page", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 101, false, []map[string]any{})}}, wantError: "changed during pagination"},
		{name: "duplicate page identity", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 101, false, first)}, {body: pullRequestTestPage(t, 101, false, []map[string]any{pullRequestTestItem(1, "openshift-bot", "open", "")})}}, wantError: "repeated identities"},
		{name: "later page failure", results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 101, false, first)}, {status: 502}}, wantError: "HTTP 502"},
		{name: "missing completeness field", results: []pullRequestHTTPResult{{body: `{"total_count":0,"items":[]}`}}, wantError: "invalid response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newPullRequests([]string{"openshift-bot", "openshift-ci-robot[bot]"}, "", false)
			if err != nil {
				t.Fatal(err)
			}
			fake := &pullRequestHTTPFake{results: tc.results}
			p.client = fake
			err = p.Sync(context.Background())
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("want %q, got %v", tc.wantError, err)
			}
			if diff := cmp.Diff(tc.wantCount, len(p.Snapshot().PRs)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
			for index, request := range fake.requests {
				if diff := cmp.Diff("https://api.github.com/search/issues", request.URL.Scheme+"://"+request.URL.Host+request.URL.Path); diff != "" {
					t.Errorf("unexpected destination (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff("repo:openshift/release is:pr is:open (author:openshift-bot OR author:openshift-ci-robot[bot])", request.URL.Query().Get("q")); diff != "" {
					t.Errorf("unexpected query (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(fmt.Sprint(index+1), request.URL.Query().Get("page")); diff != "" {
					t.Errorf("unexpected page (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff("100", request.URL.Query().Get("per_page")); diff != "" {
					t.Errorf("unexpected page size (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff("true", request.URL.Query().Get("advanced_search")); diff != "" {
					t.Errorf("unexpected search semantics (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff("", request.Header.Get("Authorization")); diff != "" {
					t.Errorf("unexpected anonymous auth (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestPullRequestsBackoff ensures failures retain last-good data and cannot cause request bursts.
func TestPullRequestsBackoff(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		status    int
		headers   http.Header
		wantRetry time.Time
	}{
		{name: "secondary rate limit", status: 429, headers: http.Header{"Retry-After": []string{"3600"}}, wantRetry: now.Add(time.Hour)},
		{name: "primary rate limit", status: 403, headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{fmt.Sprint(now.Add(2 * time.Hour).Unix())}}, wantRetry: now.Add(2*time.Hour + time.Second)},
		{name: "retry HTTP date", status: 429, headers: http.Header{"Retry-After": []string{now.Add(time.Hour).Format(http.TimeFormat)}}, wantRetry: now.Add(time.Hour)},
		{name: "ordinary outage", status: 502, wantRetry: now.Add(pullRequestPoll)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newPullRequests([]string{"openshift-bot"}, "", false)
			if err != nil {
				t.Fatal(err)
			}
			clock := &pullRequestTestClock{at: now}
			fake := &pullRequestHTTPFake{results: []pullRequestHTTPResult{{body: pullRequestTestPage(t, 1, false, []map[string]any{pullRequestTestItem(1, "openshift-bot", "open", "")})}, {status: tc.status, headers: tc.headers}, {body: pullRequestTestPage(t, 0, false, []map[string]any{})}}}
			p.client = fake
			p.clock = clock
			p.config.token = "unit-test-secret"
			if err := p.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			good := p.Snapshot()
			if err := p.Sync(context.Background()); err == nil {
				t.Fatal("expected failure")
			}
			if err := p.Sync(context.Background()); err == nil {
				t.Fatal("expected backoff")
			}
			stale := p.Snapshot()
			if diff := cmp.Diff(good.PRs, stale.PRs); diff != "" {
				t.Errorf("unexpected retained evidence (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(good.LastSync, stale.LastSync); diff != "" {
				t.Errorf("unexpected last sync (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff("stale", stale.Phase); diff != "" {
				t.Errorf("unexpected phase (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRetry, p.cache.nextAttempt); diff != "" {
				t.Errorf("unexpected retry (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(2, len(fake.requests)); diff != "" {
				t.Errorf("unexpected traffic (-want +got):\n%s", diff)
			}
			encoded, err := json.Marshal(stale)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), p.config.token) {
				t.Fatal("credential exposed")
			}
			if diff := cmp.Diff("Bearer unit-test-secret", fake.requests[0].Header.Get("Authorization")); diff != "" {
				t.Errorf("unexpected auth (-want +got):\n%s", diff)
			}
			clock.at = tc.wantRetry
			if err := p.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff("ready", p.Snapshot().Phase); diff != "" {
				t.Errorf("unexpected recovered phase (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPullRequestsConfiguration exercises safe author parsing and network-free modes.
func TestPullRequestsConfiguration(t *testing.T) {
	cases := []struct {
		name        string
		authors     []string
		demo        bool
		wantError   bool
		wantPhase   string
		wantEnabled bool
	}{
		{name: "disabled", wantPhase: "disabled"},
		{name: "configured initial phase", authors: []string{"redhat-chai-bot"}, wantPhase: "catching_up", wantEnabled: true},
		{name: "demo", authors: []string{"redhat-chai-bot"}, demo: true, wantPhase: "ready", wantEnabled: true},
		{name: "app bot", authors: []string{"dependabot[bot]"}, wantPhase: "catching_up", wantEnabled: true},
		{name: "query injection", authors: []string{"bot is:closed"}, wantError: true},
		{name: "empty author", authors: []string{""}, wantError: true},
		{name: "leading hyphen", authors: []string{"-bot"}, wantError: true},
		{name: "trailing hyphen", authors: []string{"bot-"}, wantError: true},
		{name: "invalid double hyphen", authors: []string{"bot--name"}, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newPullRequests(tc.authors, "", tc.demo)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected invalid author")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantPhase, p.Snapshot().Phase); diff != "" {
				t.Errorf("unexpected phase (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantEnabled, p.Snapshot().Enabled); diff != "" {
				t.Errorf("unexpected enabled (-want +got):\n%s", diff)
			}
			if tc.demo || !tc.wantEnabled {
				fake := &pullRequestHTTPFake{}
				p.client = fake
				if err := p.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
				p.Run(context.Background())
				if diff := cmp.Diff(0, len(fake.requests)); diff != "" {
					t.Errorf("unexpected traffic (-want +got):\n%s", diff)
				}
			}
			if tc.demo {
				if p.Snapshot().PRs[0].CreatedAt.After(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
					t.Fatal("demo lacks old PR")
				}
			}
			client, ok := p.client.(*http.Client)
			if ok {
				if client.Timeout <= 0 {
					t.Fatal("missing HTTP timeout")
				}
				request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := client.CheckRedirect(request, nil); err == nil {
					t.Fatal("redirect allowed")
				}
			}
		})
	}
}

// TestPullRequestsTokenFiles confines optional authentication to the server-side request.
func TestPullRequestsTokenFiles(t *testing.T) {
	cases := []struct {
		name, contents, wantToken string
		wantError                 bool
	}{
		{name: "trim terminal newline", contents: "server-token\n", wantToken: "server-token"},
		{name: "empty file", wantError: true},
		{name: "embedded whitespace", contents: "server token", wantError: true},
		{name: "bounded file", contents: strings.Repeat("x", 65537), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := newPullRequests([]string{"openshift-bot"}, path, false)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected invalid token file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantToken, p.config.token); diff != "" {
				t.Errorf("unexpected token (-want +got):\n%s", diff)
			}
		})
	}
}
