package opsalerts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// threadReferenceArchive supplies cached aliases while retaining the existing archive fake.
type threadReferenceArchive struct {
	*chatArchive
	references map[string]string
}

// ResolveThreadReference never reads Slack to resolve a test reference.
func (a *threadReferenceArchive) ResolveThreadReference(kind, name string) (string, bool) {
	id, found := a.references[kind+strings.ToLower(name)]
	return id, found
}

// TestThreadReplyText preserves intended references without turning code or HTML into notifications.
func TestThreadReplyText(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		invalid          bool
	}{
		{name: "configured Chai", text: "@chai-bot please investigate", want: "<@U_CHAI> please investigate"},
		{name: "case and sentence punctuation", text: "Thanks @CHAI-BOT.", want: "Thanks <@U_CHAI>."},
		{name: "current channel", text: "See #ops-team", want: "See <#C_TEST>"},
		{name: "cached user", text: "Ask @alice.smith", want: "Ask <@U_ALICE>"},
		{name: "canonical user", text: "<@U_OTHER|Other user>", want: "<@U_OTHER>"},
		{name: "canonical channel", text: "<#G_PRIVATE|private>", want: "<#G_PRIVATE>"},
		{name: "inline code", text: "`@chai-bot <@U_CHAI> #ops-team`", want: "`@chai-bot &lt;@U_CHAI&gt; #ops-team`"},
		{name: "fenced code", text: "```\n@unknown #unknown <@U_CHAI>\n```", want: "```\n@unknown #unknown &lt;@U_CHAI&gt;\n```"},
		{name: "email and URL fragments", text: "alice@example.com https://example.com/#unknown https://example.com/@unknown", want: "alice@example.com https://example.com/#unknown https://example.com/@unknown"},
		{name: "issue and heading", text: "See #123\n# Heading", want: "See #123\n# Heading"},
		{name: "escaped literal", text: `\@unknown \#unknown`, want: `\@unknown \#unknown`},
		{name: "HTML remains inert", text: "<script>bad</script> & *bold* _italic_", want: "&lt;script&gt;bad&lt;/script&gt; &amp; *bold* _italic_"},
		{name: "unknown user", text: "Ask @unknown", invalid: true},
		{name: "unknown Unicode name", text: "Ask @田中", invalid: true},
		{name: "unknown emoji display name", text: "Ask @👩‍💻", invalid: true},
		{name: "no partial Unicode match", text: "Ask @alice.smithé", invalid: true},
		{name: "unknown channel", text: "See #unknown", invalid: true},
		{name: "malformed user ID", text: "<@not-a-slack-id>", invalid: true},
		{name: "unclosed reference", text: "<@U_CHAI", invalid: true},
		{name: "broadcast mention", text: "@everyone", invalid: true},
		{name: "canonical broadcast", text: "<!here>", invalid: true},
		{name: "canonical user group", text: "<!subteam^S_GROUP>", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			service.archive = &threadReferenceArchive{chatArchive: archive, references: map[string]string{"@alice.smith": "U_ALICE"}}
			got, err := service.threadReplyBody(tc.text, "#ops-team")
			if tc.invalid {
				var referenceError *ThreadReferenceError
				if !errors.Is(err, ErrValidation) || !errors.As(err, &referenceError) {
					t.Fatalf("unexpected reference error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected Slack text (-want +got):\n%s", diff)
			}
		})
	}
}

// TestThreadReplyAttribution keeps visible actor identity and actionable mentions through recovery.
func TestThreadReplyAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, actor                         string
		legacy, stripped, malformed, mailto bool
	}{
		{name: "attributed header", actor: "alice@example.com"},
		{name: "escaped actor", actor: "alice&bob"},
		{name: "legacy header", actor: "alice@example.com", legacy: true},
		{name: "stripped metadata", actor: "alice@example.com", stripped: true},
		{name: "normalized actor email", actor: "alice@example.com", mailto: true},
		{name: "malformed frame", actor: "alice@example.com", malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + archive.roots[0].Timestamp
			req := ThreadReplyRequest{OccurrenceID: id, RequestID: "attributed", Text: "@chai-bot check #ops-team", ChannelName: "ops-team"}
			if _, err := service.ReplyToAlert(ctx, tc.actor, req); err != nil {
				t.Fatal(err)
			}
			wantText := attributedThreadReplyHeader(tc.actor) + "<@U_CHAI> check <#C_TEST>"
			if diff := cmp.Diff(wantText, archive.lastText); diff != "" {
				t.Errorf("unexpected posted text (-want +got):\n%s", diff)
			}
			stored := &archive.replies[archive.roots[0].Timestamp][0]
			if tc.legacy {
				stored.Text = threadReplyHeader + "<@U_CHAI> check <#C_TEST>"
			}
			if tc.stripped {
				stored.Metadata = slack.SlackMetadata{}
			}
			if tc.mailto {
				stored.Text = threadReplyPrefix + "<mailto:alice@example.com|alice@example.com>):\n\n<@U_CHAI> check <#C_TEST>"
			}
			if tc.malformed {
				stored.Text = "Unframed reply <@U_CHAI> check <#C_TEST>"
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req.ReconcileOnly = true
			view, err := restarted.ReplyToAlert(ctx, tc.actor, req)
			if tc.stripped || tc.malformed {
				if !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("unexpected corrupt-receipt result: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if view.Confirmation == nil || view.Confirmation.Actor != tc.actor {
					t.Fatalf("missing actor-bound confirmation: %#v", view.Confirmation)
				}
				if diff := cmp.Diff("<@U_CHAI> check <#C_TEST>", view.Messages[0].Text); diff != "" {
					t.Errorf("unexpected recovered body (-want +got):\n%s", diff)
				}
			}
			if archive.posts != 1 {
				t.Fatalf("recovery reposted the reply: %d posts", archive.posts)
			}
		})
	}
}

// TestUnresolvedThreadReply rejects unresolved handles before a message or pending write exists.
func TestUnresolvedThreadReply(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{name: "user", text: "@missing please check"},
		{name: "channel", text: "See #missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			req := ThreadReplyRequest{OccurrenceID: "C_TEST:" + archive.roots[0].Timestamp, RequestID: "unknown", Text: tc.text}
			if _, err := service.ReplyToAlert(context.Background(), "operator", req); !errors.Is(err, ErrValidation) {
				t.Fatalf("unexpected unresolved-reference error: %v", err)
			}
			if archive.posts != 0 || len(service.currentCache().ConversationPending) != 0 {
				t.Fatal("invalid mention initiated a write")
			}
		})
	}
}
