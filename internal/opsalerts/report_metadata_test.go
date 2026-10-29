package opsalerts

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// scheduledReportFixture supplies only synthetic scheduler metadata and public test identities.
func scheduledReportFixture(timestamp string) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: timestamp, User: "U_CHAI", BotID: "B_CHAI", BotProfile: &slack.BotProfile{AppID: "A_CHAI"}, Text: "[Scheduled task: pending_workloads_scan]\nReport part", Metadata: slack.SlackMetadata{EventType: "scheduled_task", EventPayload: map[string]interface{}{"scheduled_task": "pending_workloads_scan", "persona": "ops", "req_id": "run-123"}}}}
}

// TestScheduledReportRunIdentity groups only explicitly identical scheduler run identities.
func TestScheduledReportRunIdentity(t *testing.T) {
	cases := []struct {
		name, change string
		same         bool
	}{
		{name: "different timestamp same run", same: true},
		{name: "different report text same run", change: "text", same: true},
		{name: "different task", change: "scheduled_task"},
		{name: "different persona", change: "persona"},
		{name: "different request", change: "req_id"},
		{name: "different producer user", change: "user"},
		{name: "different producer bot", change: "bot"},
		{name: "different producer app", change: "app"},
		{name: "different channel", change: "channel"},
		{name: "different configured source", change: "source"},
		{name: "tuple delimiters remain unambiguous", change: "delimiters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			first := scheduledReportFixture(a.roots[0].Timestamp)
			if tc.change == "delimiters" {
				first.Metadata.EventPayload["persona"], first.Metadata.EventPayload["req_id"] = "a|b", "c"
			}
			before, admitted, err := s.normalize(first)
			if err != nil || !admitted || len(before.Message.ScheduledRun) != 64 {
				t.Fatalf("baseline admission=%v key=%q error=%v", admitted, before.Message.ScheduledRun, err)
			}
			second := scheduledReportFixture(a.roots[1].Timestamp)
			switch tc.change {
			case "text":
				second.Text = "Report continuation without task header"
			case "scheduled_task", "persona", "req_id":
				second.Metadata.EventPayload[tc.change] = "different"
			case "user":
				s.cfg.Sources[3].UserIDs = append(s.cfg.Sources[3].UserIDs, "U_CHAI_OTHER")
				second.User = "U_CHAI_OTHER"
			case "bot":
				second.BotID = "B_CHAI_OTHER"
			case "app":
				second.BotProfile.AppID = "A_CHAI_OTHER"
			case "channel":
				s.cfg.ChannelID = "C_OTHER"
			case "source":
				s.cfg.Sources[3].Name = "other-chai"
			case "delimiters":
				second.Metadata.EventPayload["persona"], second.Metadata.EventPayload["req_id"] = "a", "b|c"
			}
			after, admitted, err := s.normalize(second)
			if err != nil || !admitted || after.Message.ScheduledRun == "" {
				t.Fatalf("admission=%v key=%q error=%v", admitted, after.Message.ScheduledRun, err)
			}
			if same := before.Message.ScheduledRun == after.Message.ScheduledRun; same != tc.same {
				t.Fatalf("same run=%v, want %v", same, tc.same)
			}
		})
	}
}

// TestScheduledReportMetadataValidation refuses incomplete or untrusted grouping hints.
func TestScheduledReportMetadataValidation(t *testing.T) {
	cases := []struct {
		name, change, field string
		value               interface{}
		wantKey             bool
	}{
		{name: "valid bounded identifiers", wantKey: true},
		{name: "maximum field bytes", field: "req_id", value: strings.Repeat("x", 256), wantKey: true},
		{name: "Unicode identifiers", field: "persona", value: "operátor", wantKey: true},
		{name: "task missing", field: "scheduled_task"},
		{name: "persona missing", field: "persona"},
		{name: "request missing", field: "req_id"},
		{name: "task nonstring", field: "scheduled_task", value: 42},
		{name: "persona nonstring", field: "persona", value: []string{"ops"}},
		{name: "request nonstring", field: "req_id", value: true},
		{name: "empty task", field: "scheduled_task", value: ""},
		{name: "empty persona", field: "persona", value: ""},
		{name: "empty request", field: "req_id", value: ""},
		{name: "task control", field: "scheduled_task", value: "task\nother"},
		{name: "persona control", field: "persona", value: "ops\x00other"},
		{name: "request Unicode control", field: "req_id", value: "run\u0085other"},
		{name: "task oversized", field: "scheduled_task", value: strings.Repeat("x", 257)},
		{name: "persona oversized", field: "persona", value: strings.Repeat("x", 257)},
		{name: "request oversized bytes", field: "req_id", value: strings.Repeat("界", 86)},
		{name: "padded field", field: "persona", value: " ops "},
		{name: "invalid UTF8", field: "req_id", value: "run\xff"},
		{name: "visible header alone", change: "no-metadata"},
		{name: "untrusted user", change: "untrusted"},
		{name: "untrusted report markers", change: "markers"},
		{name: "other trusted producer", change: "other-source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			message := scheduledReportFixture(a.roots[0].Timestamp)
			if tc.field != "" {
				message.Metadata.EventPayload[tc.field] = tc.value
			}
			switch tc.change {
			case "no-metadata":
				message.Metadata = slack.SlackMetadata{}
			case "untrusted", "markers":
				message.User = "U_OTHER"
				if tc.change == "markers" {
					message.Text += "\n[ops_color=green ops_category=chai_reports]"
				}
			case "other-source":
				message.User, message.BotID = "", "B_ALERTS"
				message.Text += "\n[ops_color=green ops_category=chai_reports]"
			}
			got, _, err := s.normalize(message)
			if err != nil {
				t.Fatal(err)
			}
			if hasKey := got.Message.ScheduledRun != ""; hasKey != tc.wantKey {
				t.Fatalf("has run key=%v, want %v", hasKey, tc.wantKey)
			}
			if task, ok := message.Metadata.EventPayload["scheduled_task"].(string); ok && got.View == "chai_reports" && got.Message.ScheduledTask != strings.TrimSpace(task) {
				t.Fatal("existing scheduled task normalization changed")
			}
		})
	}
}
