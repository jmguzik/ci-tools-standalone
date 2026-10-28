package opsalerts

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestPrometheusAlerts keeps full rule names without changing classification identities.
func TestPrometheusAlerts(t *testing.T) {
	cases := []struct {
		name, text     string
		fallback, want []string
		attachments    []slack.Attachment
	}{
		{name: "marker fallback", text: "First [ops_color=red ops_category=ci_platform]", fallback: []string{"First"}, want: []string{"First"}},
		{name: "full title", text: "[FIRING:2] A full alert name (core-ci critical)", fallback: []string{"A"}, want: []string{"A full alert name"}},
		{name: "rule link", text: "<https://example.test/rules?rule-name=A%20full%20alert%20name|rule>", fallback: []string{"A"}, want: []string{"A full alert name"}},
		{name: "query key after ampersand", attachments: []slack.Attachment{{TitleLink: "https://example.test/rules?view=rules&alerting-rule-name=Full%20rule"}}, want: []string{"Full rule"}},
		{name: "attachment rule link", attachments: []slack.Attachment{{TitleLink: "https://example.test/rules?rule-name=Full%20rule"}}, want: []string{"Full rule"}},
		{name: "grouped", text: "[FIRING:2] Second alert (core-ci)\nFirst [ops_color=red ops_category=ci_platform]", fallback: []string{"First", "Second"}, want: []string{"Second alert", "First"}},
		{name: "deduplicated", text: "[FIRING:1] First\n<https://example.test/rules?rule-name=First|rule>", fallback: []string{"First"}, want: []string{"First"}},
		{name: "no inferred name", text: "Some unrecognized prose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := slack.Message{Msg: slack.Msg{Text: tc.text, Attachments: tc.attachments}}
			if diff := cmp.Diff(tc.want, prometheusAlerts(message, tc.fallback)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMetricsNormalization authorizes metrics only for trusted Alertmanager roots.
func TestMetricsNormalization(t *testing.T) {
	cases := []struct {
		name, bot, user, thread, text string
		want                          []string
	}{
		{name: "trusted alert", bot: "B_ALERTS", text: "Alert: etcdDown", want: []string{"etcdDown"}},
		{name: "unknown bot", bot: "B_UNKNOWN", text: "Alert: etcdDown"},
		{name: "prow", bot: "B_PROW", text: "Alert: etcdDown"},
		{name: "ship", bot: "B_SHIP", text: "Alert: etcdDown"},
		{name: "chai", user: "U_CHAI", text: "[Scheduled task: daily] Alert: etcdDown"},
		{name: "reply", bot: "B_ALERTS", thread: "1.000000", text: "Alert: etcdDown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, _, clock := fixture(t)
			occurrence, _, err := service.normalize(slack.Message{Msg: slack.Msg{BotID: tc.bot, User: tc.user, ThreadTimestamp: tc.thread, Timestamp: fmt.Sprintf("%d.000000", clock.Now().Unix()), Text: tc.text}})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, occurrence.Message.PrometheusAlerts); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMetricsSnapshotCopy prevents browser snapshots from mutating cached authorization data.
func TestMetricsSnapshotCopy(t *testing.T) {
	cases := []struct{ name string }{{name: "detached alert names"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := fixture(t)
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot := service.Snapshot()
			snapshot.Occurrences[0].Message.PrometheusAlerts[0] = "forged"
			if diff := cmp.Diff([]string{"etcdDown"}, service.Snapshot().Occurrences[0].Message.PrometheusAlerts); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
