package opsalerts

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
)

// TestLegacyMentionInvestigationReplay recovers the pre-metadata investigation
// format before validating workflow actions that reference its Slack revision.
func TestLegacyMentionInvestigationReplay(t *testing.T) {
	service, archive, clock := chatFixture(t)
	ctx := context.Background()
	if err := service.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	root := archive.roots[0].Timestamp
	id := service.cfg.ChannelID + ":" + root
	occurrence := findOccurrence(t, service, id)
	updated, err := service.Act(ctx, "operator", ActionRequest{OccurrenceID: id, ExpectedRevision: occurrence.Identity.Revision, Kind: "investigate", RequestID: "legacy-investigate", Question: "Why did this alert fire?"})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, err := service.Act(ctx, "operator", ActionRequest{OccurrenceID: id, ExpectedRevision: updated.Identity.Revision, Kind: "acknowledge", RequestID: "after-legacy-investigation"})
	if err != nil {
		t.Fatal(err)
	}
	// Older posts put the durable record after the visible Chai request.
	message := &archive.replies[root][0]
	raw := message.Metadata.EventPayload["record"].(string)
	message.Text = "<@U_CHAI> Please investigate this alert.\n" + ActionMarker + raw
	message.Metadata = slack.SlackMetadata{}
	restarted, err := NewService(service.cfg, service.classifier, archive, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Sync(ctx); err != nil {
		t.Fatalf("recovering legacy investigation and acknowledgement: %v", err)
	}
	got := findOccurrence(t, restarted, id)
	if restarted.Snapshot().Status.Phase != "ready" || got.State.AcknowledgedAt == nil || got.Identity.Revision != acknowledged.Identity.Revision {
		t.Fatalf("workflow not recovered: %#v", got)
	}
	if got.Investigation == nil || got.Investigation.RequestID != "legacy-investigate" {
		t.Fatalf("investigation not recovered: %#v", got.Investigation)
	}
	conversations := restarted.Conversations()
	if len(conversations.Conversations) != 1 || len(conversations.Conversations[0].Messages) != 1 || conversations.Conversations[0].Messages[0].RequestID != "legacy-investigate" {
		t.Fatalf("legacy investigation missing from chat: %#v", conversations)
	}
}

// TestConversationWithActionMarkerReplay keeps explicit chat metadata
// authoritative when an operator quotes the legacy marker on its own line.
func TestConversationWithActionMarkerReplay(t *testing.T) {
	service, archive, clock := chatFixture(t)
	ctx := context.Background()
	if err := service.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	question := "Explain this log:\n" + ActionMarker + "not a workflow record"
	conversation, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "quoted-marker", Text: question})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(service.cfg, service.classifier, archive, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Conversations()
	if snapshot.Status.Phase != "ready" || len(snapshot.Conversations) != 1 || snapshot.Conversations[0].ID != conversation.ID || len(snapshot.Conversations[0].Messages) != 1 || snapshot.Conversations[0].Messages[0].Text != question {
		t.Fatalf("chat quoting marker not recovered: %#v", snapshot)
	}
}
