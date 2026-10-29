package opsalerts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestDecorationStartup covers restart reconciliation without assuming complete user lists.
func TestDecorationStartup(t *testing.T) {
	cases := []struct {
		name      string
		reactions []slack.ItemReaction
		ack       bool
		want      []reactionCall
	}{
		{name: "untouched feed"},
		{name: "unrelated reaction", reactions: []slack.ItemReaction{{Name: "heart", Users: []string{"U_OTHER"}}}},
		{name: "stale own status", reactions: []slack.ItemReaction{{Name: "eyes", Users: []string{"U_WRITER"}}}, want: []reactionCall{{Emoji: "eyes"}}},
		{name: "other users remain", reactions: []slack.ItemReaction{{Name: "eyes", Users: []string{"U_OTHER"}}}, want: []reactionCall{{Emoji: "eyes"}}},
		{name: "truncated users", reactions: []slack.ItemReaction{{Name: "zzz", Count: 2, Users: []string{"U_OTHER"}}}, want: []reactionCall{{Emoji: "zzz"}}},
		{name: "desired own status preserved", ack: true, reactions: []slack.ItemReaction{{Name: "eyes", Users: []string{"U_WRITER"}}}},
		{name: "desired status needs writer", ack: true, reactions: []slack.ItemReaction{{Name: "eyes", Count: 2, Users: []string{"U_OTHER"}}}, want: []reactionCall{{Emoji: "eyes", Present: true}}},
		{name: "desired icon and stale status", ack: true, reactions: []slack.ItemReaction{{Name: "eyes", Users: []string{"U_WRITER"}}, {Name: "zzz", Users: []string{"U_WRITER"}}}, want: []reactionCall{{Emoji: "zzz"}}},
		{name: "all stale statuses", reactions: []slack.ItemReaction{{Name: "eyes"}, {Name: "zzz"}, {Name: "white_check_mark"}}, want: []reactionCall{{Emoji: "eyes"}, {Emoji: "zzz"}, {Emoji: "white_check_mark"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			a.roots = a.roots[:1]
			ctx := context.Background()
			if tc.ack {
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				decorationAction(t, s, a, "acknowledge", "startup-ack")
			}
			a.roots[0].Reactions = tc.reactions
			a.reactions = nil
			restarted, err := NewService(s.cfg, s.classifier, a, c)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := restarted.Sync(ctx); err != nil {
					t.Fatal(err)
				}
			}
			want := append([]reactionCall(nil), tc.want...)
			for i := range want {
				want[i].Root = a.roots[0].Timestamp
			}
			if diff := cmp.Diff(want, a.reactions); diff != "" {
				t.Errorf("unexpected writes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.reactions, a.roots[0].Reactions); diff != "" {
				t.Errorf("changed other users' metadata (-want +got):\n%s", diff)
			}
		})
	}
}

// decorationAction saves a real workflow transition for decoration regression tests.
func decorationAction(t *testing.T, s *Service, a *memoryArchive, kind, requestID string) {
	t.Helper()
	id := "C_TEST:" + a.roots[0].Timestamp
	o := findOccurrence(t, s, id)
	if _, err := s.Act(context.Background(), "operator", ActionRequest{OccurrenceID: id, Kind: kind, ExpectedRevision: o.Identity.Revision, RequestID: requestID}); err != nil {
		t.Fatal(err)
	}
}

// TestDecorationPartialSuccess checks that retrying one icon does not repeat successful writes.
func TestDecorationPartialSuccess(t *testing.T) {
	failure := errors.New("reaction unavailable")
	cases := []struct {
		name   string
		kind   string
		errors []error
		want   []reactionCall
	}{
		{name: "successful removal is retained", kind: "snooze", errors: []error{nil, failure}, want: []reactionCall{{Emoji: "eyes"}, {Emoji: "zzz", Present: true}, {Emoji: "zzz", Present: true}}},
		{name: "successful addition is retained", kind: "close", errors: []error{failure, nil}, want: []reactionCall{{Emoji: "eyes"}, {Emoji: "white_check_mark", Present: true}, {Emoji: "eyes"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			a.roots = a.roots[:1]
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			decorationAction(t, s, a, "acknowledge", "partial-ack")
			a.reactions = nil
			a.reactionErrors = tc.errors
			decorationAction(t, s, a, tc.kind, "partial-change")
			for range 2 {
				if err := s.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			want := append([]reactionCall(nil), tc.want...)
			for i := range want {
				want[i].Root = a.roots[0].Timestamp
			}
			if diff := cmp.Diff(want, a.reactions); diff != "" {
				t.Errorf("unexpected writes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDecorationBatchStops exercises optional write circuit breakers with a fixed clock.
func TestDecorationBatchStops(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		limited bool
		cancel  bool
	}{
		{name: "wrapped rate limit", err: fmt.Errorf("reaction: %w", &slack.RateLimitedError{RetryAfter: 2 * time.Minute}), limited: true},
		{name: "wrapped missing scope", err: fmt.Errorf("reaction: %w", slack.SlackErrorResponse{Err: "missing_scope"})},
		{name: "canceled during reaction", err: context.Canceled, cancel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			for i := range a.roots {
				a.roots[i].Reactions = []slack.ItemReaction{{Name: "zzz", Users: []string{"U_WRITER"}}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				a.cancelOnReaction = cancel
			}
			a.reactionError = tc.err
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if len(a.reactions) != 1 || s.Snapshot().Status.Phase != "ready" {
				t.Fatalf("batch did not stop while ready: writes=%d status=%+v", len(a.reactions), s.Snapshot().Status)
			}
			if tc.cancel {
				return
			}
			a.reactionError = nil
			c.now = c.now.Add(2*time.Minute - time.Nanosecond)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(a.reactions) != 1 {
				t.Fatal("reaction retried before deadline or after missing_scope")
			}
			decorationAction(t, s, a, "acknowledge", "breaker-ack")
			if a.posts != 1 || len(a.reactions) != 1 || s.Snapshot().Status.Phase != "ready" {
				t.Fatal("reaction suppression blocked workflow or allowed optional writes")
			}
			c.now = c.now.Add(time.Nanosecond)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.limited {
				want = 5 // Three stale removals and the newly acknowledged icon resume.
			}
			if len(a.reactions) != want {
				t.Fatalf("deadline writes: want %d, got %+v", want, a.reactions)
			}
			if !tc.limited {
				restarted, err := NewService(s.cfg, s.classifier, a, c)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(a.reactions) != 5 {
					t.Fatalf("restart did not resume unsynchronized icons: %+v", a.reactions)
				}
			}
		})
	}
}
