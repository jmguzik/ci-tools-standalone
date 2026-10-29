package opsalerts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// legacyActionText makes historical records independently of the current writer.
func legacyActionText(t *testing.T, record actionRecord) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return ActionMarker + string(data)
}

// TestActionMetadataDecoder keeps metadata envelopes as strict as legacy records.
func TestActionMetadataDecoder(t *testing.T) {
	cases := []struct {
		name, change, wantError string
		wantRecord              bool
	}{
		{name: "metadata", wantRecord: true},
		{name: "legacy", change: "legacy", wantRecord: true},
		{name: "equal dual copies", change: "dual", wantRecord: true},
		{name: "conflicting dual copies", change: "conflict", wantError: "conflicting legacy and metadata"},
		{name: "unknown envelope field", change: "envelope", wantError: "invalid action metadata envelope"},
		{name: "nonstring record", change: "type", wantError: "invalid action metadata envelope"},
		{name: "empty envelope", change: "empty", wantError: "invalid action metadata envelope"},
		{name: "malformed record", change: "malformed", wantError: "decoding action metadata"},
		{name: "unknown record field", change: "field", wantError: "unknown field"},
		{name: "unknown version", change: "version", wantError: "invalid action version"},
		{name: "wrong target", change: "target", wantError: "invalid action version"},
		{name: "trailing record", change: "trailing", wantError: "trailing action data"},
		{name: "wrong event", change: "event", wantError: "missing supported metadata"},
		{name: "missing metadata", change: "missing", wantError: "missing supported metadata"},
		{name: "ordinary writer reply", change: "ordinary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			o, _, err := s.normalize(a.roots[0])
			if err != nil {
				t.Fatal(err)
			}
			record := actionRecord{Version: 1, Actor: "operator", At: s.clock.Now(), Request: ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, RequestID: "decoder", Kind: "acknowledge"}}
			record.State.AcknowledgedBy, record.State.AcknowledgedAt = record.Actor, &record.At
			message, err := s.actionMessage(record, o)
			if err != nil {
				t.Fatal(err)
			}
			raw := message.Metadata.EventPayload["record"].(string)
			switch tc.change {
			case "legacy":
				message.Text, message.Metadata = legacyActionText(t, record), slack.SlackMetadata{}
			case "dual":
				message.Text += "\n" + legacyActionText(t, record)
			case "conflict":
				record.Actor = "different"
				message.Text += "\n" + legacyActionText(t, record)
			case "envelope":
				message.Metadata.EventPayload["extra"] = true
			case "type":
				message.Metadata.EventPayload["record"] = map[string]any{"version": 1}
			case "empty":
				message.Metadata.EventPayload = nil
			case "malformed":
				message.Metadata.EventPayload["record"] = "{broken}"
			case "field":
				message.Metadata.EventPayload["record"] = strings.TrimSuffix(raw, "}") + `,"extra":true}`
			case "version":
				message.Metadata.EventPayload["record"] = strings.Replace(raw, `"version":1`, `"version":2`, 1)
			case "target":
				o.Identity.ID = "C_TEST:other"
			case "trailing":
				message.Metadata.EventPayload["record"] = raw + " {}"
			case "event":
				message.Metadata.EventType = "other_event"
			case "missing":
				message.Metadata = slack.SlackMetadata{}
			case "ordinary":
				message.Text, message.Metadata = "ordinary reply", slack.SlackMetadata{EventType: "other_event"}
			}
			got, exists, err := decodeMessageRecord(message, o.Identity.ID)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil || exists != tc.wantRecord {
				t.Fatalf("exists=%v error=%v", exists, err)
			}
			if exists {
				if diff := cmp.Diff(record, got); diff != "" {
					t.Fatalf("record (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestMetadataRecoveryFailures preserves confirmed workflow state on reader failures.
func TestMetadataRecoveryFailures(t *testing.T) {
	cases := []struct {
		name, change string
		untrusted    bool
	}{
		{name: "lost metadata", change: "missing"},
		{name: "wrong event", change: "event"},
		{name: "malformed metadata", change: "malformed"},
		{name: "untrusted malformed metadata ignored", change: "malformed", untrusted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", actor: "operator"})
			before := s.currentCache()
			reply := a.replies[a.roots[0].Timestamp][0]
			reply.Timestamp = strings.TrimSuffix(reply.Timestamp, "000001") + "000002"
			switch tc.change {
			case "missing":
				reply.Metadata = slack.SlackMetadata{}
			case "event":
				reply.Metadata.EventType = "other_event"
			case "malformed":
				reply.Metadata.EventPayload = map[string]any{"record": "{broken}"}
			}
			if tc.untrusted {
				reply.User, reply.BotID = "U_OTHER", "B_OTHER"
			}
			a.replies[a.roots[0].Timestamp] = append(a.replies[a.roots[0].Timestamp], reply)
			a.roots[0].ReplyCount++
			err := s.Sync(context.Background())
			if (err != nil) == tc.untrusted {
				t.Fatalf("unexpected sync error=%v", err)
			}
			after := s.currentCache()
			if diff := cmp.Diff(before.Records, after.Records); diff != "" {
				t.Fatalf("records changed (-want +got):\n%s", diff)
			}
			if !tc.untrusted {
				if diff := cmp.Diff(before.Occurrences, after.Occurrences); diff != "" {
					t.Fatalf("workflow changed (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestMixedActionRecovery replays legacy and metadata actions across restart and reset.
func TestMixedActionRecovery(t *testing.T) {
	cases := []struct{ name, kind, scope string }{
		{name: "regex WIP", kind: "acknowledge", scope: "regex"},
		{name: "regex snooze", kind: "snooze", scope: "regex"},
		{name: "occurrence snooze", kind: "snooze", scope: "occurrence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", actor: "first"})
			legacy := a.replies[a.roots[0].Timestamp][0]
			legacy.Text = legacyActionText(t, s.currentCache().Records["action_1"])
			legacy.Metadata = slack.SlackMetadata{}
			a.replies[a.roots[0].Timestamp][0] = legacy
			clock.now = clock.now.Add(time.Minute)
			applyScopedAction(t, s, a, scopedAction{kind: tc.kind, scope: tc.scope, actor: "second", target: 1})
			if strings.Contains(a.lastText, ActionMarker) || strings.Contains(a.lastText, `"version"`) || !strings.HasPrefix(a.lastText, ActionReceiptPrefix) || len(a.lastText) > 180 {
				t.Fatalf("unexpected visible receipt: %q", a.lastText)
			}
			s = restartScopedService(t, s, a, clock)
			clock.now = nextSixUTC(clock.now)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := findOccurrence(t, s, "C_TEST:"+a.roots[1].Timestamp); got.WIPRule != nil || got.Snooze != nil || got.View != "feed" {
				t.Fatalf("rule survived reset: %+v", got)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestActionReceipts exposes scope and expiry without leaking machine record fields.
func TestActionReceipts(t *testing.T) {
	cases := []struct {
		name, kind, scope, want string
	}{
		{name: "occurrence WIP", kind: "acknowledge", want: "Ops dashboard: 👀 WIP by owner &amp; helper."},
		{name: "family WIP", kind: "acknowledge", scope: "regex", want: "Ops dashboard: 👀 WIP matching alerts by owner &amp; helper until 2026-10-29 06:00 UTC."},
		{name: "release family", kind: "unacknowledge", scope: "regex", want: "Ops dashboard: Released matching alerts by owner &amp; helper."},
		{name: "close", kind: "close", want: "Ops dashboard: ✅ Closed by owner &amp; helper."},
		{name: "reopen", kind: "reopen", want: "Ops dashboard: Reopened by owner &amp; helper."},
		{name: "occurrence snooze", kind: "snooze", scope: "occurrence", want: "Ops dashboard: 💤 Snoozed by owner &amp; helper until 2026-10-29 06:00 UTC."},
		{name: "family snooze", kind: "snooze", scope: "regex", want: "Ops dashboard: 💤 Snoozed matching alerts by owner &amp; helper until 2026-10-29 06:00 UTC."},
		{name: "legacy family snooze", kind: "snooze", want: "Ops dashboard: 💤 Snoozed matching alerts by owner &amp; helper until 2026-10-29 06:00 UTC."},
		{name: "unsnooze", kind: "unsnooze", want: "Ops dashboard: Unsnoozed by owner &amp; helper."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := actionRecord{Actor: "owner & helper", Request: ActionRequest{Kind: tc.kind, Scope: tc.scope, RequestID: "machine-only"}}
			until := time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
			if tc.kind == "snooze" {
				r.Snooze = &Snooze{Until: until}
			}
			if tc.kind == "acknowledge" && tc.scope == "regex" {
				r.WIPRule = &WIPRule{Until: until}
			}
			if diff := cmp.Diff(tc.want, actionReceipt(r)); diff != "" {
				t.Fatalf("receipt (-want +got):\n%s", diff)
			}
		})
	}
}

// TestActionMetadataBudget bounds the escaped envelope rather than the raw question.
func TestActionMetadataBudget(t *testing.T) {
	cases := []struct {
		name, question string
		oversized      bool
	}{
		{name: "plain below", question: strings.Repeat("a", 100)},
		{name: "plain above", question: strings.Repeat("a", 1800), oversized: true},
		{name: "escaped below", question: strings.Repeat("\"\\", 10)},
		{name: "escaped above", question: strings.Repeat("\"\\", 300), oversized: true},
		{name: "control below", question: strings.Repeat("\x01", 10)},
		{name: "control above", question: strings.Repeat("\x01", 200), oversized: true},
		{name: "unicode below", question: strings.Repeat("界", 50)},
		{name: "unicode above", question: strings.Repeat("界", 600), oversized: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			o := findOccurrence(t, s, "C_TEST:"+a.roots[0].Timestamp)
			req := ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, Kind: "investigate", RequestID: "budget", Question: tc.question}
			r, err := s.prepareAction("operator", req, o, s.currentCache())
			if err != nil {
				t.Fatal(err)
			}
			message, err := s.actionMessage(r, o)
			if tc.oversized {
				if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "application budget") || message.Text != "" || message.Metadata.EventType != "" {
					t.Fatalf("oversized message=%+v error=%v", message, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				serialized, err := json.Marshal(message.Metadata)
				if err != nil || len(serialized) > MaxActionMetadataBytes {
					t.Fatalf("envelope bytes=%d error=%v", len(serialized), err)
				}
			}
			_, err = s.Act(context.Background(), "operator", req)
			wantPosts := 1
			if tc.oversized {
				wantPosts = 0
				if !errors.Is(err, ErrValidation) || len(s.currentCache().Pending) != 0 || len(s.currentCache().Records) != 0 {
					t.Fatalf("oversized action changed durable or pending state: %v", err)
				}
			}
			if a.posts != wantPosts || !tc.oversized && err != nil {
				t.Fatalf("posts=%d want=%d error=%v", a.posts, wantPosts, err)
			}
		})
	}
}

// TestLargeLegacyMetadataRecovery keeps historical state readable beyond the write budget.
func TestLargeLegacyMetadataRecovery(t *testing.T) {
	cases := []struct {
		name     string
		metadata bool
	}{
		{name: "legacy record"},
		{name: "metadata record", metadata: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			o := findOccurrence(t, s, "C_TEST:"+a.roots[0].Timestamp)
			req := ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, Kind: "investigate", RequestID: "large-old-question", Question: strings.Repeat("x", 3000)}
			r, err := s.prepareAction("operator", req, o, s.currentCache())
			if err != nil {
				t.Fatal(err)
			}
			legacy := legacyActionText(t, r)
			message := slack.Msg{Timestamp: historyRoot(clock.now).Timestamp, ThreadTimestamp: o.Identity.Timestamp, User: "U_WRITER", BotID: "B_WRITER", Text: legacy}
			if tc.metadata {
				message.Text = ActionReceiptPrefix + "Historical investigation."
				message.Metadata = slack.SlackMetadata{EventType: ActionMetadataEvent, EventPayload: map[string]any{"record": strings.TrimPrefix(legacy, ActionMarker)}}
			}
			a.replies[o.Identity.Timestamp] = []slack.Message{{Msg: message}}
			a.roots[0].ReplyCount++
			if err := s.Sync(context.Background()); err != nil {
				t.Fatalf("large historical record unreadable: %v", err)
			}
			s = restartScopedService(t, s, a, clock)
			o = findOccurrence(t, s, o.Identity.ID)
			if o.Investigation == nil || o.Investigation.Question != req.Question {
				t.Fatal("large historical investigation was truncated")
			}
			_, err = s.Act(context.Background(), "operator", ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, Kind: "close", RequestID: "retain-large"})
			if !errors.Is(err, ErrValidation) || a.posts != 0 || len(s.currentCache().Pending) != 0 {
				t.Fatalf("oversized retained investigation: posts=%d error=%v", a.posts, err)
			}
			if got := findOccurrence(t, s, o.Identity.ID); got.State.ClosedAt != nil {
				t.Fatal("failed oversized write closed the occurrence")
			}
		})
	}
}
