package main

import (
	"context"
	"encoding/json"
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

// TestSlackPost confirms durable metadata with one write and one bounded read.
func TestSlackPost(t *testing.T) {
	const record = `{"version":1,"request":{"occurrence_id":"C:100.000001","kind":"acknowledge","expected_revision":"revision-1","request_id":"stable-request-id","pattern":"","question":""},"actor":"unit-test-user","at":"2026-10-28T12:00:00Z","state":{"acknowledged_by":"unit-test-user","acknowledged_at":"2026-10-28T12:00:00Z","closed_by":"","closed_at":null},"snooze":null,"unsnooze":"","investigation":null}`
	message := slack.Msg{Text: "Ops dashboard: acknowledged", Metadata: slack.SlackMetadata{EventType: opsalerts.ActionMetadataEvent, EventPayload: map[string]interface{}{"record": record}}}
	metadata, err := json.Marshal(message.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := slackHTTPResult{body: `{"ok":true,"channel":"C","ts":"200.000001"}`}
	readback := slackHTTPResult{body: fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","thread_ts":"100.000001","metadata":%s}]}`, metadata)}
	cases := []struct {
		name          string
		results       []slackHTTPResult
		invalidType   bool
		timestamp     string
		wantError     error
		wantErrorText string
	}{
		{name: "confirmed metadata round trip", results: []slackHTTPResult{confirmed, readback}, timestamp: "200.000001"},
		{name: "readback without optional thread timestamp", results: []slackHTTPResult{confirmed, {body: fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","metadata":%s}]}`, metadata)}}, timestamp: "200.000001"},
		{name: "invalid metadata type rejected before write", invalidType: true, wantError: opsalerts.ErrDeliveryFailed, wantErrorText: "invalid action metadata"},
		{name: "explicit rejection", results: []slackHTTPResult{{body: `{"ok":false,"error":"not_in_channel"}`}}, wantError: opsalerts.ErrDeliveryFailed, wantErrorText: "not_in_channel"},
		{name: "rate limited write is not retried", results: []slackHTTPResult{{status: 429}}, wantError: opsalerts.ErrDeliveryFailed, wantErrorText: "rate limit"},
		{name: "transport uncertain", results: []slackHTTPResult{{err: errors.New("connection lost")}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "connection lost"},
		{name: "server uncertain", results: []slackHTTPResult{{status: 503}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "slack server error"},
		{name: "missing timestamp skips readback", results: []slackHTTPResult{{body: `{"ok":true,"channel":"C"}`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "incomplete write confirmation"},
		{name: "wrong channel skips readback", results: []slackHTTPResult{{body: `{"ok":true,"channel":"other","ts":"200.000001"}`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "incomplete write confirmation"},
		{name: "schema warning and omitted metadata", results: []slackHTTPResult{{body: `{"ok":true,"channel":"C","ts":"200.000001","warning":"metadata_schema_validation_failed","response_metadata":{"warnings":["metadata_schema_validation_failed"]}}`}, {body: `{"ok":true,"messages":[{"ts":"200.000001","thread_ts":"100.000001"}]}`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "metadata is missing or differs"},
		{name: "different payload", results: []slackHTTPResult{confirmed, {body: fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","metadata":%s}]}`, strings.Replace(string(metadata), "unit-test-user", "other-user", 1))}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "metadata is missing or differs"},
		{name: "nonstring payload", results: []slackHTTPResult{confirmed, {body: fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","metadata":{"event_type":%q,"event_payload":{"record":%s}}}]}`, opsalerts.ActionMetadataEvent, record)}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "metadata is missing or differs"},
		{name: "different metadata event type", results: []slackHTTPResult{confirmed, {body: strings.Replace(readback.body, opsalerts.ActionMetadataEvent, "other_event", 1)}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "metadata is missing or differs"},
		{name: "wrong readback thread", results: []slackHTTPResult{confirmed, {body: strings.Replace(readback.body, `"thread_ts":"100.000001"`, `"thread_ts":"99.000001"`, 1)}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "outside its thread"},
		{name: "missing posted message", results: []slackHTTPResult{confirmed, {body: `{"ok":true,"messages":[]}`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "not returned with its metadata"},
		{name: "another message cannot confirm metadata", results: []slackHTTPResult{confirmed, {body: strings.Replace(readback.body, `"ts":"200.000001"`, `"ts":"199.000001"`, 1)}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "not returned with its metadata"},
		{name: "rate limited readback is not retried", results: []slackHTTPResult{confirmed, {status: 429}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "rate limit"},
		{name: "readback API rejection", results: []slackHTTPResult{confirmed, {body: `{"ok":false,"error":"missing_scope"}`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "missing_scope"},
		{name: "readback transport error", results: []slackHTTPResult{confirmed, {err: errors.New("read connection lost")}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "read connection lost"},
		{name: "readback server error", results: []slackHTTPResult{confirmed, {status: 503}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "slack server error"},
		{name: "readback malformed JSON", results: []slackHTTPResult{confirmed, {body: `{"ok":true`}}, wantError: opsalerts.ErrDeliveryUncertain, wantErrorText: "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: tc.results}
			client := fakeSlackClient(fake)
			archive := &slackArchive{reader: client, writer: client}
			posted := message
			if tc.invalidType {
				posted.Metadata.EventType = "other_event"
			}
			got, err := archive.Post(context.Background(), "C", "100.000001", "stable-request-id", posted)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("want error %v, got %v", tc.wantError, err)
			}
			if tc.wantErrorText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrorText)) {
				t.Errorf("want error containing %q, got %v", tc.wantErrorText, err)
			}
			if diff := cmp.Diff(tc.timestamp, got); diff != "" {
				t.Errorf("unexpected confirmation (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(len(tc.results), len(fake.forms)); diff != "" {
				t.Fatalf("unexpected request attempts (-want +got):\n%s", diff)
			}
			if tc.invalidType {
				return
			}
			want := url.Values{"channel": {"C"}, "thread_ts": {"100.000001"}, "client_msg_id": {"stable-request-id"}, "text": {message.Text}, "metadata": {string(metadata)}}
			for key, values := range want {
				if diff := cmp.Diff(values, fake.forms[0][key]); diff != "" {
					t.Errorf("unexpected post field %s (-want +got):\n%s", key, diff)
				}
			}
			if text := fake.forms[0].Get("text"); strings.Contains(text, `"version"`) || strings.Contains(text, opsalerts.ActionMarker) {
				t.Errorf("raw action record leaked into readable text: %q", text)
			}
			wantPaths := []string{"/api/chat.postMessage"}
			if len(tc.results) == 2 {
				wantPaths = append(wantPaths, "/api/conversations.replies")
				wantRead := url.Values{"channel": {"C"}, "ts": {"100.000001"}, "oldest": {"200.000001"}, "latest": {"200.000001"}, "inclusive": {"1"}, "limit": {"1"}, "include_all_metadata": {"1"}}
				for key, values := range wantRead {
					if diff := cmp.Diff(values, fake.forms[1][key]); diff != "" {
						t.Errorf("unexpected readback field %s (-want +got):\n%s", key, diff)
					}
				}
			}
			if diff := cmp.Diff(wantPaths, fake.paths); diff != "" {
				t.Errorf("unexpected endpoints (-want +got):\n%s", diff)
			}
		})
	}
}

// slackSizedMetadata constructs an encoded envelope at an exact byte boundary.
func slackSizedMetadata(t *testing.T, fill string, target int) slack.Msg {
	t.Helper()
	message := slack.Msg{Text: "Ops dashboard: acknowledged", Metadata: slack.SlackMetadata{EventType: opsalerts.ActionMetadataEvent}}
	record := `{"version":1,"question":""}`
	message.Metadata.EventPayload = map[string]interface{}{"record": record}
	empty, err := json.Marshal(message.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	encodedFill, err := json.Marshal(fill)
	if err != nil {
		t.Fatal(err)
	}
	encodedRecordFill, err := json.Marshal(string(encodedFill[1 : len(encodedFill)-1]))
	if err != nil {
		t.Fatal(err)
	}
	width := len(encodedRecordFill) - 2
	remaining := target - len(empty)
	question := strings.Repeat(fill, remaining/width) + strings.Repeat("x", remaining%width)
	questionJSON, err := json.Marshal(question)
	if err != nil {
		t.Fatal(err)
	}
	message.Metadata.EventPayload["record"] = `{"version":1,"question":` + string(questionJSON) + `}`
	return message
}

// TestSlackPostMetadataBudget bounds the entire serialized envelope before any write.
func TestSlackPostMetadataBudget(t *testing.T) {
	cases := []struct {
		name        string
		fill        string
		size        int
		unsupported bool
	}{
		{name: "ASCII exact budget", fill: "x", size: opsalerts.MaxActionMetadataBytes},
		{name: "ASCII above budget", fill: "x", size: opsalerts.MaxActionMetadataBytes + 1},
		{name: "multibyte exact budget", fill: "ż", size: opsalerts.MaxActionMetadataBytes},
		{name: "multibyte above budget", fill: "ż", size: opsalerts.MaxActionMetadataBytes + 1},
		{name: "escaped quote exact budget", fill: `"`, size: opsalerts.MaxActionMetadataBytes},
		{name: "escaped quote above budget", fill: `"`, size: opsalerts.MaxActionMetadataBytes + 1},
		{name: "HTML escaped exact budget", fill: "&", size: opsalerts.MaxActionMetadataBytes},
		{name: "HTML escaped above budget", fill: "&", size: opsalerts.MaxActionMetadataBytes + 1},
		{name: "unsupported metadata value", unsupported: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := slack.Msg{Text: "Ops dashboard: acknowledged", Metadata: slack.SlackMetadata{EventType: opsalerts.ActionMetadataEvent, EventPayload: map[string]interface{}{"record": make(chan int)}}}
			var metadata []byte
			if !tc.unsupported {
				message = slackSizedMetadata(t, tc.fill, tc.size)
				var err error
				metadata, err = json.Marshal(message.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.size, len(metadata)); diff != "" {
					t.Fatalf("unexpected fixture encoded byte size (-want +got):\n%s", diff)
				}
			}
			var results []slackHTTPResult
			wantError := opsalerts.ErrDeliveryFailed
			wantTimestamp := ""
			if !tc.unsupported && tc.size == opsalerts.MaxActionMetadataBytes {
				results = []slackHTTPResult{{body: `{"ok":true,"channel":"C","ts":"200.000001"}`}, {body: fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","thread_ts":"100.000001","metadata":%s}]}`, metadata)}}
				wantError = nil
				wantTimestamp = "200.000001"
			}
			fake := &slackHTTPFake{results: results}
			client := fakeSlackClient(fake)
			archive := &slackArchive{reader: client, writer: client}
			got, err := archive.Post(context.Background(), "C", "100.000001", "stable-request-id", message)
			if !errors.Is(err, wantError) {
				t.Errorf("want error %v, got %v", wantError, err)
			}
			if wantError != nil {
				wantErrorText := fmt.Sprintf("action metadata exceeds the %d-byte application budget", opsalerts.MaxActionMetadataBytes)
				if tc.unsupported {
					wantErrorText = "encoding action metadata: json: unsupported type: chan int"
				}
				if err == nil || !strings.Contains(err.Error(), wantErrorText) {
					t.Errorf("want error containing %q, got %v", wantErrorText, err)
				}
			}
			if diff := cmp.Diff(wantTimestamp, got); diff != "" {
				t.Errorf("unexpected confirmation (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(len(results), len(fake.forms)); diff != "" {
				t.Fatalf("unexpected requests (-want +got):\n%s", diff)
			}
			if len(results) != 0 {
				if diff := cmp.Diff(string(metadata), fake.forms[0].Get("metadata")); diff != "" {
					t.Errorf("unexpected posted metadata (-want +got):\n%s", diff)
				}
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
