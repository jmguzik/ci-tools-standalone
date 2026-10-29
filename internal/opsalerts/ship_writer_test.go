package opsalerts

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestSharedSHIPWriterConfiguration permits only SHIP to reuse writer identities.
func TestSharedSHIPWriterConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		sources   []Source
		wantError string
	}{
		{name: "SHIP writer user", sources: []Source{{Name: "DPTP Bot", Kind: "ship", UserIDs: []string{"U_WRITER"}}}},
		{name: "SHIP writer bot", sources: []Source{{Name: "DPTP Bot", Kind: "ship", BotIDs: []string{"B_WRITER"}}}},
		{name: "SHIP writer user and bot", sources: []Source{{Name: "DPTP Bot", Kind: "ship", UserIDs: []string{"U_WRITER"}, BotIDs: []string{"B_WRITER"}}}},
		{name: "SHIP writer app", sources: []Source{{Name: "DPTP Bot", Kind: "ship", AppIDs: []string{"A_WRITER"}}}},
		{name: "Chai writer user", sources: []Source{{Name: "chai", Kind: "chai", UserIDs: []string{"U_WRITER"}}}, wantError: "validation failed: identity shared by writer and chai"},
		{name: "Chai writer bot", sources: []Source{{Name: "chai", Kind: "chai", BotIDs: []string{"B_WRITER"}}}, wantError: "validation failed: identity shared by writer and chai"},
		{name: "Alertmanager writer user", sources: []Source{{Name: "alerts", Kind: "alertmanager", UserIDs: []string{"U_WRITER"}}}, wantError: "validation failed: identity shared by writer and alerts"},
		{name: "Alertmanager writer bot", sources: []Source{{Name: "alerts", Kind: "alertmanager", BotIDs: []string{"B_WRITER"}}}, wantError: "validation failed: identity shared by writer and alerts"},
		{name: "Prow writer user", sources: []Source{{Name: "prow", Kind: "prow", UserIDs: []string{"U_WRITER"}}}, wantError: "validation failed: identity shared by writer and prow"},
		{name: "Chai writer app", sources: []Source{{Name: "chai", Kind: "chai", AppIDs: []string{"A_WRITER"}}}, wantError: "validation failed: identity shared by writer and chai"},
		{name: "two SHIP sources share writer", sources: []Source{{Name: "ship", Kind: "ship", UserIDs: []string{"U_WRITER"}}, {Name: "another", Kind: "ship", UserIDs: []string{"U_WRITER"}}}, wantError: "validation failed: identity shared by ship and another"},
		{name: "writer source name cannot bypass producer uniqueness", sources: []Source{{Name: "writer", Kind: "ship", BotIDs: []string{"B_WRITER"}}, {Name: "another", Kind: "ship", BotIDs: []string{"B_WRITER"}}}, wantError: "validation failed: identity shared by writer and another"},
		{name: "SHIP and Prow share producer", sources: []Source{{Name: "ship", Kind: "ship", BotIDs: []string{"B_SOURCE"}}, {Name: "prow", Kind: "prow", BotIDs: []string{"B_SOURCE"}}}, wantError: "validation failed: identity shared by ship and prow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := s.cfg
			cfg.Writer.AppID = "A_WRITER"
			cfg.Sources = tc.sources
			_, err := NewService(cfg, s.classifier, s.archive, s.clock)
			gotError := ""
			if err != nil {
				gotError = err.Error()
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if diff := cmp.Diff(tc.wantError, gotError); diff != "" {
				t.Errorf("unexpected error (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSharedSHIPWriterNormalization recognizes DPTP notifications without admitting actions.
func TestSharedSHIPWriterNormalization(t *testing.T) {
	cases := []struct {
		name, user, bot, text, thread string
		want                          bool
		metadata                      slack.SlackMetadata
		classification                Classification
		identifiers                   []string
	}{
		{name: "DPTP writer SHIP outage", user: "U01AP73A3AP", text: "🚨 Outage Detected: Build Farm/Prow\n[ops_color=green ops_category=chai_reports]", want: true, classification: Classification{Color: "yellow", Category: "cluster_fleet", Origin: "policy"}, identifiers: []string{"Build Farm/Prow"}},
		{name: "writer bot SHIP outage", bot: "B_WRITER", text: "🚨 Outage Detected: Build Farm/Prow", want: true, classification: Classification{Color: "yellow", Category: "cluster_fleet", Origin: "policy"}, identifiers: []string{"Build Farm/Prow"}},
		{name: "DPTP writer other notification", user: "U01AP73A3AP", text: "repo-brancher failed [ops_color=red ops_category=repo_automation]", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "writer bot other notification", bot: "B_WRITER", text: "repo-brancher failed", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "own thread action", user: "U01AP73A3AP", text: "Acknowledged\n" + ActionMarker + "{}", thread: "123.000000"},
		{name: "own root action", user: "U01AP73A3AP", text: "Acknowledged\n" + ActionMarker + "{}"},
		{name: "own bot root malformed action", bot: "B_WRITER", text: ActionMarker + "{broken}"},
		{name: "own metadata root", user: "U01AP73A3AP", text: "acknowledged", metadata: slack.SlackMetadata{EventType: ActionMetadataEvent}},
		{name: "own receipt with lost metadata", user: "U01AP73A3AP", text: ActionReceiptPrefix + "acknowledged"},
		{name: "unknown human outage", user: "U_HUMAN", text: "🚨 Outage Detected: Build Farm/Prow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, archive, _ := fixture(t)
			cfg := s.cfg
			cfg.Writer.UserID = "U01AP73A3AP"
			cfg.Sources = []Source{{Name: "DPTP Bot", Kind: "ship", UserIDs: []string{"U01AP73A3AP"}, BotIDs: []string{"B_WRITER"}}}
			s, err := NewService(cfg, s.classifier, archive, s.clock)
			if err != nil {
				t.Fatal(err)
			}
			message := slack.Message{Msg: slack.Msg{Timestamp: archive.roots[0].Timestamp, User: tc.user, BotID: tc.bot, Text: tc.text, Metadata: tc.metadata, ThreadTimestamp: tc.thread}}
			got, admitted, err := s.normalize(message)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, admitted); diff != "" {
				t.Fatalf("unexpected admission (-want +got):\n%s", diff)
			}
			if !admitted {
				return
			}
			if diff := cmp.Diff(tc.classification, got.Classification); diff != "" {
				t.Errorf("unexpected classification (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.identifiers, got.Message.Identifiers); diff != "" {
				t.Errorf("unexpected identifiers (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff("DPTP Bot", got.Message.Source); diff != "" {
				t.Errorf("unexpected source (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSharedSHIPWriterReplay preserves durable actions on roots from the same writer.
func TestSharedSHIPWriterReplay(t *testing.T) {
	cases := []struct{ name, user, bot string }{
		{name: "shared user", user: "U_WRITER"},
		{name: "shared bot", bot: "B_WRITER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, archive, clock := fixture(t)
			cfg := s.cfg
			cfg.Sources = []Source{{Name: "DPTP Bot", Kind: "ship", UserIDs: []string{"U_WRITER"}, BotIDs: []string{"B_WRITER"}}}
			root := archive.roots[0]
			root.User, root.BotID, root.Text = tc.user, tc.bot, "🚨 Outage Detected: Build Farm/Prow"
			archive.roots = []slack.Message{root}
			s, err := NewService(cfg, s.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + root.Timestamp
			want, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: root.Timestamp, RequestID: "shared-writer-ack"})
			if err != nil {
				t.Fatal(err)
			}
			if want.State.AcknowledgedAt == nil {
				t.Fatal("action did not acknowledge SHIP root")
			}
			archive.roots = append(archive.roots, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000002", clock.Now().Unix()), User: "U_WRITER", BotID: "B_WRITER", Text: archive.lastMessage.Text, Metadata: archive.lastMessage.Metadata}})
			restarted, err := NewService(cfg, s.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(1, len(restarted.Snapshot().Occurrences)); diff != "" {
				t.Errorf("unexpected occurrence count (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(want, findOccurrence(t, restarted, id)); diff != "" {
				t.Errorf("unexpected replay (-want +got):\n%s", diff)
			}
		})
	}
}
