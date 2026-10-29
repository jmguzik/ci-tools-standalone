package opsalerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// chatArchive extends the shared archive fake with ordinary channel-root writes.
type chatArchive struct{ *memoryArchive }

// Post stores standalone channel messages and advances deterministic server time.
func (a *chatArchive) Post(ctx context.Context, channel, root, id string, message slack.Msg) (string, error) {
	ts, err := a.memoryArchive.Post(ctx, channel, root, id, message)
	if root == "" && (err == nil || a.storeOnError) {
		stored := a.replies[""]
		a.roots = append(a.roots, stored[len(stored)-1])
		delete(a.replies, "")
	}
	a.clock.now = a.clock.now.Add(time.Millisecond)
	return ts, err
}

// chatFixture enables standalone roots while retaining the existing alert fixtures.
func chatFixture(t *testing.T) (*Service, *chatArchive, *fixedClock) {
	t.Helper()
	service, archive, clock := fixture(t)
	wrapped := &chatArchive{memoryArchive: archive}
	service.archive = wrapped
	return service, wrapped, clock
}

// TestConversationLifecycle covers root and linked chats, restart and safe cached reads.
func TestConversationLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		linked bool
	}{{name: "standalone"}, {name: "alert thread", linked: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, _ := chatFixture(t)
			if _, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "initial", Text: "Why?"}); !errors.Is(err, ErrNotReady) {
				t.Fatalf("unready error %v", err)
			}
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			alerts := len(service.Snapshot().Occurrences)
			req := ConversationRequest{RequestID: "first", Text: " What is <happening> & why? "}
			if tc.linked {
				req.OccurrenceID = service.Snapshot().Occurrences[0].Identity.ID
			}
			conversation, err := service.Converse(ctx, "operator", req)
			if err != nil {
				t.Fatal(err)
			}
			if conversation.ID == "" || len(conversation.Messages) != 1 {
				t.Fatalf("conversation %#v", conversation)
			}
			if conversation.SourceOccurrenceID != req.OccurrenceID {
				t.Fatalf("wrong source %q", conversation.SourceOccurrenceID)
			}
			if tc.linked {
				occurrence := findOccurrence(t, service, req.OccurrenceID)
				if diff := cmp.Diff(strings.Join(occurrence.Message.Identifiers, " · "), conversation.Title); diff != "" {
					t.Errorf("unexpected alert conversation title (-want +got):\n%s", diff)
				}
			}
			wantText := "<@U_CHAI> What is &lt;happening&gt; &amp; why?"
			if !tc.linked {
				wantText += "\n\n💬 Dashboard · Chai"
			}
			if diff := cmp.Diff(wantText, archive.lastText); diff != "" {
				t.Errorf("Slack text (-want +got):\n%s", diff)
			}
			calls := archive.replyCalls
			retried, err := service.Converse(ctx, "operator", req)
			if err != nil {
				t.Fatal(err)
			}
			if archive.posts != 1 || archive.replyCalls != calls {
				t.Fatalf("retry performed I/O: posts %d replies %d", archive.posts, archive.replyCalls)
			}
			if diff := cmp.Diff(conversation, retried); diff != "" {
				t.Errorf("retry (-want +got):\n%s", diff)
			}
			_, err = service.Converse(ctx, "other", req)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("actor reuse %v", err)
			}
			follow := ConversationRequest{ConversationID: conversation.ID, RequestID: "second", Text: "And " + ActionMarker + "not a workflow record?"}
			conversation, err = service.Converse(ctx, "operator", follow)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(archive.lastText, "Dashboard") || !strings.HasPrefix(archive.lastText, "<@U_CHAI> ") {
				t.Fatalf("follow-up text %q", archive.lastText)
			}
			root := strings.TrimPrefix(conversation.ID, "C_TEST:")
			for i, author := range []string{"U_CHAI", "U_HUMAN", "B_UNTRUSTED"} {
				ts := fmt.Sprintf("%d.%06d", archive.clock.now.Unix(), 100+i)
				msg := slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: root, User: author, Text: "reply " + author}}
				if author == "B_UNTRUSTED" {
					msg.User = ""
					msg.BotID = author
				}
				archive.replies[root] = append(archive.replies[root], msg)
			}
			archive.clock.now = archive.clock.now.Add(time.Second)
			for i := range archive.roots {
				if archive.roots[i].Timestamp == root {
					archive.roots[i].ReplyCount += 3
					archive.roots[i].LatestReply = archive.replies[root][len(archive.replies[root])-1].Timestamp
				}
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, archive.clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			snapshot := restarted.Conversations()
			if len(snapshot.Conversations) != 1 || len(snapshot.Conversations[0].Messages) != 4 {
				t.Fatalf("history %#v", snapshot)
			}
			if len(restarted.Snapshot().Occurrences) != alerts {
				t.Fatalf("chat changed alert count")
			}
			messages := snapshot.Conversations[0].Messages
			if !messages[0].IsOperator || messages[0].RequestID != "first" || !messages[2].IsChai || messages[3].IsOperator {
				t.Fatalf("message identities %#v", messages)
			}
			snapshot.Conversations[0].Messages[0].Text = "mutated"
			if restarted.Conversations().Conversations[0].Messages[0].Text == "mutated" {
				t.Fatal("snapshot aliases cache")
			}
			before := archive.replyCalls
			restarted.Conversations()
			restarted.Conversations()
			if archive.replyCalls != before {
				t.Fatal("GET fetched Slack")
			}
			if _, err := restarted.Converse(ctx, "operator", req); err != nil {
				t.Fatalf("restart retry %v", err)
			}
			if archive.posts != 2 {
				t.Fatal("restart duplicated request")
			}
		})
	}
}

// TestConversationDeliveryRecovery exercises uncertainty guards and explicit failure retries.
func TestConversationDeliveryRecovery(t *testing.T) {
	cases := []struct {
		name      string
		postError error
		stored    bool
		want      error
	}{{name: "uncertain delivered", postError: errors.New("transport lost"), stored: true, want: ErrDeliveryUncertain}, {name: "uncertain absent", postError: errors.New("transport lost"), want: ErrDeliveryUncertain}, {name: "explicit rejection", postError: ErrDeliveryFailed, want: ErrDeliveryFailed}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, _ := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			archive.postError = tc.postError
			archive.storeOnError = tc.stored
			req := ConversationRequest{RequestID: "unknown", Text: "Where is the failure?"}
			if _, err := service.Converse(ctx, "operator", req); !errors.Is(err, tc.want) {
				t.Fatalf("post error %v", err)
			}
			archive.postError = nil
			if !errors.Is(tc.want, ErrDeliveryFailed) {
				if _, err := service.Converse(ctx, "operator", req); !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("retry error %v", err)
				}
				if _, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "different", Text: "Another question?"}); !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("new root guard %v", err)
				}
				if archive.posts != 1 {
					t.Fatal("blind retry")
				}
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				_, err := service.Converse(ctx, "operator", req)
				if tc.stored && err != nil {
					t.Fatalf("reconciled retry %v", err)
				}
				if !tc.stored && !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("absence cleared uncertainty %v", err)
				}
				if archive.posts != 1 {
					t.Fatal("recovery reposted")
				}
			} else {
				if _, err := service.Converse(ctx, "operator", req); err != nil {
					t.Fatalf("explicit retry %v", err)
				}
				if archive.posts != 2 {
					t.Fatal("explicit rejection not retryable")
				}
			}
		})
	}
}

// TestConversationTrustAndWindow checks metadata forgery, malformed writer records and expiry.
func TestConversationTrustAndWindow(t *testing.T) {
	cases := []struct {
		name              string
		mutation          string
		wantError         bool
		wantConversations int
	}{{name: "valid", wantConversations: 1}, {name: "forged author", mutation: "forged"}, {name: "tampered text", mutation: "text", wantError: true}, {name: "unknown record field", mutation: "unknown", wantError: true}, {name: "missing metadata", mutation: "missing", wantError: true}, {name: "expired", mutation: "expired"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "one", Text: "Question?"}); err != nil {
				t.Fatal(err)
			}
			root := &archive.roots[len(archive.roots)-1]
			switch tc.mutation {
			case "forged":
				root.User = "U_FORGED"
				root.BotID = "B_FORGED"
			case "text":
				root.Text += " changed"
			case "unknown":
				raw := root.Metadata.EventPayload["record"].(string)
				root.Metadata.EventPayload["record"] = "{\"unknown\":true," + strings.TrimPrefix(raw, "{")
			case "missing":
				root.Metadata = slack.SlackMetadata{}
			case "expired":
				clock.now = clock.now.Add(49 * time.Hour)
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			err = restarted.Sync(ctx)
			if (err != nil) != tc.wantError {
				t.Fatalf("sync error %v want %v", err, tc.wantError)
			}
			if len(restarted.Conversations().Conversations) != tc.wantConversations {
				t.Fatalf("unexpected conversations %#v", restarted.Conversations())
			}
			if !tc.wantError && tc.mutation != "forged" && len(restarted.Snapshot().Occurrences) > 3 {
				t.Fatal("standalone counted as alert")
			}
		})
	}
}

// TestConversationValidation covers budget, targeting and configured-user requirements.
func TestConversationValidation(t *testing.T) {
	cases := []struct {
		name, text, conversation, occurrence string
		disabled                             bool
		want                                 error
	}{{name: "4000 bytes", text: strings.Repeat("a", 4000)}, {name: "oversize", text: strings.Repeat("a", 4001), want: ErrValidation}, {name: "blank", text: "  ", want: ErrValidation}, {name: "both targets", text: "Hello", conversation: "chat", occurrence: "alert", want: ErrValidation}, {name: "missing conversation", text: "Hello", conversation: "missing", want: ErrNotFound}, {name: "missing alert", text: "Hello", occurrence: "missing", want: ErrNotFound}, {name: "disabled", text: "Hello", disabled: true, want: ErrValidation}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			if tc.disabled {
				service.cfg.Sources = service.cfg.Sources[:3]
			}
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := service.Converse(context.Background(), "operator", ConversationRequest{RequestID: "valid", Text: tc.text, ConversationID: tc.conversation, OccurrenceID: tc.occurrence})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v want %v", err, tc.want)
			}
			if tc.want == nil {
				data, err := json.Marshal(archive.lastMessage.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if len(data) > MaxActionMetadataBytes || strings.Contains(string(data), strings.Repeat("a", 100)) {
					t.Fatalf("unbounded metadata %d bytes", len(data))
				}
			}
			if service.Conversations().Enabled == tc.disabled {
				t.Fatal("incorrect enabled state")
			}
		})
	}
}

// TestLegacyConversationReplay preserves investigation history and workflow revisions.
func TestLegacyConversationReplay(t *testing.T) {
	cases := []struct{ name string }{{name: "legacy investigation followed by chat"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			occurrence := service.Snapshot().Occurrences[0]
			action := ActionRequest{OccurrenceID: occurrence.Identity.ID, ExpectedRevision: occurrence.Identity.Revision, Kind: "investigate", RequestID: "legacy", Question: "Why is etcd down?"}
			updated, err := service.Act(ctx, "operator", action)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := service.Conversations()
			if len(snapshot.Conversations) != 1 {
				t.Fatalf("legacy history %#v", snapshot)
			}
			message := snapshot.Conversations[0].Messages[0]
			if message.RequestID != "legacy" || message.Text != action.Question || !message.IsOperator {
				t.Fatalf("legacy message %#v", message)
			}
			conversation, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: occurrence.Identity.ID, RequestID: "new-chat", Text: "What should we check next?"})
			if err != nil {
				t.Fatal(err)
			}
			if len(conversation.Messages) != 2 {
				t.Fatalf("messages %#v", conversation.Messages)
			}
			current := findOccurrence(t, service, occurrence.Identity.ID)
			if current.Identity.Revision != updated.Identity.Revision || current.Investigation == nil || current.Investigation.RequestID != "legacy" {
				t.Fatalf("chat changed workflow %#v", current)
			}
			_, err = service.Act(ctx, "operator", ActionRequest{OccurrenceID: current.Identity.ID, ExpectedRevision: current.Identity.Revision, Kind: "acknowledge", RequestID: "after-chat"})
			if err != nil {
				t.Fatalf("workflow after chat: %v", err)
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, archive.clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if len(restarted.Conversations().Conversations[0].Messages) != 2 {
				t.Fatal("workflow receipt leaked into chat")
			}
		})
	}
}

// TestForgedConversationMetadataCannotHideAlerts binds chat exclusion to writer identity.
func TestForgedConversationMetadataCannotHideAlerts(t *testing.T) {
	cases := []struct{ name, bot string }{{name: "configured alert source", bot: "B_ALERTS"}, {name: "unknown bot", bot: "B_UNTRUSTED"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, clock := chatFixture(t)
			archive.roots = append(archive.roots, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000000", clock.now.Add(-time.Minute).Unix()), BotID: tc.bot, Text: "Alert: etcdDown", Metadata: slack.SlackMetadata{EventType: ConversationMetadataEvent, EventPayload: map[string]interface{}{"record": "forged"}}}})
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(service.Snapshot().Occurrences) != 4 || len(service.Conversations().Conversations) != 0 {
				t.Fatalf("forged metadata affected alert admission")
			}
		})
	}
}

// TestConversationFollowupUncertainty retains the thread and request guard through Sync.
func TestConversationFollowupUncertainty(t *testing.T) {
	cases := []struct {
		name   string
		stored bool
	}{{name: "delivered", stored: true}, {name: "not observed"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			conversation, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "root", Text: "First question?"})
			if err != nil {
				t.Fatal(err)
			}
			follow := ConversationRequest{ConversationID: conversation.ID, RequestID: "follow", Text: "Second question?"}
			archive.postError = errors.New("lost acknowledgement")
			archive.storeOnError = tc.stored
			if _, err := service.Converse(ctx, "operator", follow); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("error %v", err)
			}
			changed := follow
			changed.Text = "Different question"
			if _, err := service.Converse(ctx, "operator", changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("request reuse %v", err)
			}
			archive.postError = nil
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			_, err = service.Converse(ctx, "operator", follow)
			if tc.stored && err != nil {
				t.Fatalf("reconciled %v", err)
			}
			if !tc.stored && !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("unobserved %v", err)
			}
			if archive.posts != 2 {
				t.Fatal("blind follow-up retry")
			}
		})
	}
}

// TestActiveConversationRefresh verifies bounded cache-triggered background refresh.
func TestActiveConversationRefresh(t *testing.T) {
	cases := []struct {
		name        string
		readError   error
		rateLimited bool
	}{{name: "reply arrives"}, {name: "read failure", readError: errors.New("unavailable")}, {name: "rate limit", readError: &slack.RateLimitedError{RetryAfter: time.Minute}, rateLimited: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, clock := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			conversation, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "root", Text: "Question?"})
			if err != nil {
				t.Fatal(err)
			}
			calls := archive.replyCalls
			if service.TouchConversation("missing") || !service.TouchConversation(conversation.ID) {
				t.Fatal("incorrect touch validation")
			}
			if archive.replyCalls != calls {
				t.Fatal("touch fetched Slack")
			}
			root := strings.TrimPrefix(conversation.ID, "C_TEST:")
			clock.now = clock.now.Add(time.Second)
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000000", clock.now.Unix()), ThreadTimestamp: root, User: "U_CHAI", Text: "Answer"}})
			archive.repliesError = tc.readError
			err = service.SyncConversation(ctx)
			if (err != nil) != (tc.readError != nil) {
				t.Fatalf("refresh error %v", err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("refresh did not read exactly one thread")
			}
			if service.Snapshot().Status.Phase != "ready" {
				t.Fatal("chat read failure degraded workflow")
			}
			wantMessages := 2
			if tc.readError != nil {
				wantMessages = 1
			}
			if len(service.Conversations().Conversations[0].Messages) != wantMessages {
				t.Fatal("incorrect refresh history")
			}
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(5 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("poll minimum ignored")
			}
			archive.repliesError = nil
			clock.now = clock.now.Add(5 * time.Second)
			service.TouchConversation(conversation.ID)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			wantCalls := calls + 2
			if tc.rateLimited {
				wantCalls = calls + 1
			}
			if archive.replyCalls != wantCalls {
				t.Fatalf("refresh calls %d want %d", archive.replyCalls, wantCalls)
			}
			clock.now = clock.now.Add(time.Minute)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != wantCalls {
				t.Fatal("expired interest polled Slack")
			}
		})
	}
}

// TestConversationInterestBound bounds retained user activity independently of Slack history.
func TestConversationInterestBound(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{{name: "nine viewed threads", count: 9}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, clock := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			for i := range tc.count {
				conversation, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: fmt.Sprintf("root-%d", i), Text: "Question?"})
				if err != nil {
					t.Fatal(err)
				}
				if !service.TouchConversation(conversation.ID) {
					t.Fatal("touch rejected retained chat")
				}
			}
			if len(service.activity.Threads) != 8 {
				t.Fatalf("interest size %d", len(service.activity.Threads))
			}
			calls := archive.replyCalls
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("global minimum bypassed across threads")
			}
			clock.now = clock.now.Add(5 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+2 {
				t.Fatal("next active thread not refreshed")
			}
		})
	}
}

// TestConversationUncertaintyScope isolates new-root uncertainty to its operator.
func TestConversationUncertaintyScope(t *testing.T) {
	cases := []struct {
		name, actor          string
		existing, sameThread bool
		want                 error
	}{{name: "same operator new root", actor: "alice", want: ErrDeliveryUncertain}, {name: "other operator new root", actor: "bob"}, {name: "same existing thread other operator", actor: "bob", existing: true, sameThread: true, want: ErrDeliveryUncertain}, {name: "new root while other thread uncertain", actor: "alice", existing: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			unknown := ConversationRequest{RequestID: "unknown", Text: "First question?"}
			if tc.existing {
				conversation, err := service.Converse(ctx, "alice", ConversationRequest{RequestID: "root", Text: "Initial question?"})
				if err != nil {
					t.Fatal(err)
				}
				unknown.ConversationID = conversation.ID
			}
			archive.postError = errors.New("lost acknowledgement")
			if _, err := service.Converse(ctx, "alice", unknown); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("first error %v", err)
			}
			archive.postError = nil
			next := ConversationRequest{RequestID: "next", Text: "Independent question?"}
			if tc.sameThread {
				next.ConversationID = unknown.ConversationID
			}
			_, err := service.Converse(ctx, tc.actor, next)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v want %v", err, tc.want)
			}
		})
	}
}
