package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// slackHTTPResult supplies one SDK response without credentials or a network transport.
type slackHTTPResult struct {
	body   string
	status int
	err    error
}

// slackHTTPFake records SDK requests and fails on an unexpected extra attempt.
type slackHTTPFake struct {
	results []slackHTTPResult
	forms   []url.Values
	paths   []string
	cancel  context.CancelFunc
}

// Do implements the pinned SDK's HTTPClient boundary entirely in memory.
func (f *slackHTTPFake) Do(request *http.Request) (*http.Response, error) {
	if err := request.ParseForm(); err != nil {
		return nil, err
	}
	f.forms = append(f.forms, request.Form)
	f.paths = append(f.paths, request.URL.Path)
	index := len(f.forms) - 1
	if index >= len(f.results) {
		return nil, fmt.Errorf("unexpected Slack request %d", index+1)
	}
	result := f.results[index]
	if result.err != nil {
		return nil, result.err
	}
	status := result.status
	if status == 0 {
		status = http.StatusOK
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if status == http.StatusTooManyRequests {
		headers.Set("Retry-After", "3600")
		if f.cancel != nil {
			f.cancel()
		}
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(result.body)), Request: request}, nil
}

// fakeSlackClient cannot make real requests because every SDK call uses the fake.
func fakeSlackClient(fake *slackHTTPFake) *slack.Client {
	return slack.New("unit-test-placeholder", slack.OptionHTTPClient(fake))
}

// TestSlackPagination exercises complete reads and rejects partial or stalled scans.
func TestSlackPagination(t *testing.T) {
	cases := []struct {
		name       string
		replies    bool
		results    []slackHTTPResult
		timestamps []string
		wantError  string
		forms      []url.Values
	}{
		{name: "history cursor", results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"200.000001"}],"has_more":true,"response_metadata":{"next_cursor":"page-2"}}`}, {body: `{"ok":true,"messages":[{"ts":"100.000001"}],"has_more":false}`}}, timestamps: []string{"200.000001", "100.000001"}, forms: []url.Values{{"channel": {"C"}, "oldest": {"1.000000"}}, {"cursor": {"page-2"}}}},
		{name: "history time boundary", results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"200.000001"},{"ts":"150.000001"}],"has_more":true}`}, {body: `{"ok":true,"messages":[{"ts":"100.000001"}]}`}}, timestamps: []string{"200.000001", "150.000001", "100.000001"}, forms: []url.Values{{"oldest": {"1.000000"}}, {"latest": {"150.000001"}, "inclusive": {"0"}}}},
		{name: "replies cursor", replies: true, results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"100.000001"}],"has_more":true,"response_metadata":{"next_cursor":"page-2"}}`}, {body: `{"ok":true,"messages":[{"ts":"200.000001"}]}`}}, timestamps: []string{"100.000001", "200.000001"}, forms: []url.Values{{"channel": {"C"}, "ts": {"100.000001"}}, {"cursor": {"page-2"}}}},
		{name: "replies time boundary", replies: true, results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"100.000001"},{"ts":"150.000001"}],"has_more":true}`}, {body: `{"ok":true,"messages":[{"ts":"200.000001"}]}`}}, timestamps: []string{"100.000001", "150.000001", "200.000001"}, forms: []url.Values{{"ts": {"100.000001"}}, {"oldest": {"150.000001"}, "inclusive": {"0"}}}},
		{name: "history later page error discards earlier results", results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"200.000001"}],"response_metadata":{"next_cursor":"page-2"}}`}, {body: `{"ok":false,"error":"missing_scope"}`}}, wantError: "missing_scope"},
		{name: "replies later page error discards earlier results", replies: true, results: []slackHTTPResult{{body: `{"ok":true,"messages":[{"ts":"100.000001"}],"response_metadata":{"next_cursor":"page-2"}}`}, {body: `{"ok":false,"error":"missing_scope"}`}}, wantError: "missing_scope"},
		{name: "history repeated cursor", results: []slackHTTPResult{{body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`}, {body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`}}, wantError: "repeated a cursor"},
		{name: "replies repeated cursor", replies: true, results: []slackHTTPResult{{body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`}, {body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`}}, wantError: "repeated a cursor"},
		{name: "history empty continuation", results: []slackHTTPResult{{body: `{"ok":true,"has_more":true}`}}, wantError: "did not advance"},
		{name: "replies empty continuation", replies: true, results: []slackHTTPResult{{body: `{"ok":true,"has_more":true}`}}, wantError: "did not advance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: tc.results}
			client := fakeSlackClient(fake)
			archive := &slackArchive{reader: client}
			var messages []slack.Message
			var err error
			if tc.replies {
				messages, err = archive.Replies(context.Background(), "C", "100.000001")
			} else {
				messages, err = archive.History(context.Background(), "C", time.Unix(1, 0))
			}
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("want error containing %q, got %v", tc.wantError, err)
			}
			var timestamps []string
			for _, message := range messages {
				timestamps = append(timestamps, message.Timestamp)
			}
			if diff := cmp.Diff(tc.timestamps, timestamps); diff != "" {
				t.Errorf("unexpected complete messages (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(len(tc.results), len(fake.forms)); diff != "" {
				t.Fatalf("unexpected request count (-want +got):\n%s", diff)
			}
			for index, want := range tc.forms {
				for key, values := range want {
					if diff := cmp.Diff(values, fake.forms[index][key]); diff != "" {
						t.Errorf("request %d field %s (-want +got):\n%s", index, key, diff)
					}
				}
			}
		})
	}
}

// TestSlackRateLimitCancellation ensures Retry-After never blocks cancellation.
func TestSlackRateLimitCancellation(t *testing.T) {
	cases := []struct {
		name    string
		replies bool
	}{{name: "history"}, {name: "replies", replies: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &slackHTTPFake{results: []slackHTTPResult{{status: http.StatusTooManyRequests}}, cancel: cancel}
			archive := &slackArchive{reader: fakeSlackClient(fake)}
			var messages []slack.Message
			var err error
			if tc.replies {
				messages, err = archive.Replies(ctx, "C", "100.000001")
			} else {
				messages, err = archive.History(ctx, "C", time.Unix(1, 0))
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("want canceled read, got %v", err)
			}
			if diff := cmp.Diff(1, len(fake.forms)); diff != "" {
				t.Errorf("unexpected retries (-want +got):\n%s", diff)
			}
			if messages != nil {
				t.Error("canceled read returned partial messages")
			}
		})
	}
}

// TestSlackPost keeps thread/request identity and makes only one delivery attempt.
func TestSlackPost(t *testing.T) {
	cases := []struct {
		name      string
		result    slackHTTPResult
		timestamp string
		wantError error
	}{
		{name: "confirmed", result: slackHTTPResult{body: `{"ok":true,"channel":"C","ts":"200.000001"}`}, timestamp: "200.000001"},
		{name: "explicit rejection", result: slackHTTPResult{body: `{"ok":false,"error":"not_in_channel"}`}, wantError: opsalerts.ErrDeliveryFailed},
		{name: "rate limited write is not retried", result: slackHTTPResult{status: 429}, wantError: opsalerts.ErrDeliveryFailed},
		{name: "transport uncertain", result: slackHTTPResult{err: errors.New("connection lost")}, wantError: opsalerts.ErrDeliveryUncertain},
		{name: "server uncertain", result: slackHTTPResult{status: 503}, wantError: opsalerts.ErrDeliveryUncertain},
		{name: "missing confirmation", result: slackHTTPResult{body: `{"ok":true,"channel":"C"}`}, wantError: opsalerts.ErrDeliveryUncertain},
		{name: "wrong channel confirmation", result: slackHTTPResult{body: `{"ok":true,"channel":"other","ts":"200.000001"}`}, wantError: opsalerts.ErrDeliveryUncertain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{tc.result}}
			archive := &slackArchive{writer: fakeSlackClient(fake)}
			got, err := archive.Post(context.Background(), "C", "100.000001", "stable-request-id", "readable action")
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("want error %v, got %v", tc.wantError, err)
			}
			if diff := cmp.Diff(tc.timestamp, got); diff != "" {
				t.Errorf("unexpected confirmation (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(1, len(fake.forms)); diff != "" {
				t.Fatalf("unexpected write attempts (-want +got):\n%s", diff)
			}
			want := url.Values{"channel": {"C"}, "thread_ts": {"100.000001"}, "client_msg_id": {"stable-request-id"}, "text": {"readable action"}}
			for key, values := range want {
				if diff := cmp.Diff(values, fake.forms[0][key]); diff != "" {
					t.Errorf("unexpected %s (-want +got):\n%s", key, diff)
				}
			}
			if diff := cmp.Diff([]string{"/api/chat.postMessage"}, fake.paths); diff != "" {
				t.Errorf("unexpected endpoint (-want +got):\n%s", diff)
			}
		})
	}
}

// TestVerifySlackIdentity rejects personal tokens and mismatched configured identities.
func TestVerifySlackIdentity(t *testing.T) {
	cases := []struct{ name, body, botID, wantError string }{
		{name: "trusted bot", body: `{"ok":true,"team_id":"T","user_id":"U","bot_id":"B","url":"https://workspace.slack.com/"}`},
		{name: "configured bot", botID: "B", body: `{"ok":true,"team_id":"T","user_id":"U","bot_id":"B","url":"https://workspace.slack.com"}`},
		{name: "wrong workspace", body: `{"ok":true,"team_id":"OTHER","user_id":"U","bot_id":"B"}`, wantError: "configured workspace"},
		{name: "wrong user", body: `{"ok":true,"team_id":"T","user_id":"OTHER","bot_id":"B"}`, wantError: "DPTP-bot user"},
		{name: "personal token", body: `{"ok":true,"team_id":"T","user_id":"U"}`, wantError: "DPTP-bot user"},
		{name: "wrong bot", botID: "EXPECTED", body: `{"ok":true,"team_id":"T","user_id":"U","bot_id":"B"}`, wantError: "bot identity"},
		{name: "wrong URL", body: `{"ok":true,"team_id":"T","user_id":"U","bot_id":"B","url":"https://other.slack.com"}`, wantError: "workspace URL"},
		{name: "auth rejected", body: `{"ok":false,"error":"invalid_auth"}`, wantError: "invalid_auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: tc.body}}}
			cfg := opsalerts.Config{WorkspaceID: "T", WorkspaceURL: "https://workspace.slack.com", Writer: opsalerts.Identity{UserID: "U", BotID: tc.botID}}
			err := verifySlackIdentity(context.Background(), fakeSlackClient(fake), &cfg)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff("B", cfg.Writer.BotID); diff != "" {
				t.Errorf("unexpected verified bot (-want +got):\n%s", diff)
			}
		})
	}
}
