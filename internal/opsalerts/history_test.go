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

// historyRoot places a trusted producer parent at an exact rolling boundary.
func historyRoot(at time.Time) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.%06d", at.Unix(), at.Nanosecond()/1000), BotID: "B_ALERTS", Text: "Alert: etcdDown"}}
}

// TestHistoryConfiguration applies the default without requiring a fixed go-live timestamp.
func TestHistoryConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		window    time.Duration
		want      time.Duration
		wantError bool
	}{
		{name: "default", want: 48 * time.Hour},
		{name: "override", window: time.Hour, want: time.Hour},
		{name: "negative rejected", window: -time.Second, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, archive, clock := fixture(t)
			cfg := s.cfg
			cfg.Cutover, cfg.HistoryWindow = time.Time{}, tc.window
			got, err := NewService(cfg, s.classifier, archive, clock)
			if (err != nil) != tc.wantError {
				t.Fatalf("want error %v, got %v", tc.wantError, err)
			}
			if err != nil {
				return
			}
			if got.cfg.HistoryWindow != tc.want {
				t.Fatalf("want window %v, got %v", tc.want, got.cfg.HistoryWindow)
			}
		})
	}
}

// TestRollingHistory bounds visibility at reads and recovery, including inclusive endpoints.
func TestRollingHistory(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
		floor  time.Duration
	}{
		{name: "default without cutover", window: DefaultHistoryWindow},
		{name: "override", window: 2 * time.Hour},
		{name: "optional go live floor", window: DefaultHistoryWindow, floor: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			if tc.window != DefaultHistoryWindow {
				s.cfg.HistoryWindow = tc.window
			}
			start := clock.now.Add(-tc.window)
			if tc.floor != 0 {
				s.cfg.Cutover = clock.now.Add(-tc.floor)
				start = s.cfg.Cutover
			}
			a.roots = []slack.Message{historyRoot(start.Add(-time.Microsecond)), historyRoot(start), historyRoot(clock.now), historyRoot(clock.now.Add(time.Microsecond))}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot := s.Snapshot()
			if len(snapshot.Occurrences) != 2 || !snapshot.Status.Cutover.Equal(start) {
				t.Fatalf("unexpected boundary snapshot: %+v", snapshot)
			}
			clock.now = clock.now.Add(time.Microsecond)
			// Future parents were excluded from the scan, even after time advances.
			want := 1
			if tc.floor != 0 {
				want = 2
			}
			read := s.Snapshot()
			wantStart := start.Add(time.Microsecond)
			if tc.floor != 0 {
				wantStart = start
			}
			if !read.Status.Cutover.Equal(wantStart) {
				t.Fatalf("coverage start did not advance: want %v, got %v", wantStart, read.Status.Cutover)
			}
			if got := len(read.Occurrences); got != want {
				t.Fatalf("read-time age out: want %d occurrences, got %d", want, got)
			}
			restarted, err := NewService(s.cfg, s.classifier, a, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			want++ // The formerly future parent is now visible after recovery.
			if got := len(restarted.Snapshot().Occurrences); got != want {
				t.Fatalf("recovery: want %d occurrences, got %d", want, got)
			}
			clock.now = clock.now.Add(tc.window + 25*time.Hour)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			cache := s.currentCache()
			if diff := cmp.Diff([]int{0, 0, 0, 0, 0}, []int{len(cache.Threads), len(cache.Occurrences), len(cache.Records), len(cache.Pending), len(cache.Decorations.Icons)}); diff != "" {
				t.Errorf("expired caches (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBufferedSnooze keeps active rules after their parent leaves the visible window.
func TestBufferedSnooze(t *testing.T) {
	cases := []struct {
		name    string
		restart bool
	}{
		{name: "same process"},
		{name: "restart", restart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			a.roots = []slack.Message{historyRoot(clock.now.Add(-DefaultHistoryWindow + time.Second)), historyRoot(clock.now.Add(-time.Minute))}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			old := s.Snapshot().Occurrences[1]
			_, err := s.Act(context.Background(), "operator", ActionRequest{OccurrenceID: old.Identity.ID, ExpectedRevision: old.Identity.Revision, Kind: "snooze", RequestID: "rule", Pattern: "etcd.*"})
			if err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(2 * time.Second)
			if tc.restart {
				s, err = NewService(s.cfg, s.classifier, a, clock)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot := s.Snapshot()
			if len(snapshot.Occurrences) != 1 || snapshot.Occurrences[0].View != "snoozed" {
				t.Fatalf("buffered rule lost: %+v", snapshot)
			}
			clock.now = nextSixUTC(clock.now)
			if got := s.Snapshot().Occurrences[0].View; got != "feed" {
				t.Fatalf("expired rule still active: %s", got)
			}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.currentCache().Rules) != 0 {
				t.Fatal("expired rule retained after sync")
			}
		})
	}
}

// TestHistoricalUnsnooze validates occurrence history independently of expired cross-thread rules.
func TestHistoricalUnsnooze(t *testing.T) {
	cases := []struct {
		name         string
		age          time.Duration
		postDelay    time.Duration
		invalidState bool
		wantError    bool
	}{
		{name: "historical missing parent", age: 24 * time.Hour},
		{name: "pre reset undo confirmed at reset", age: 23*time.Hour + time.Second, postDelay: time.Second},
		{name: "pre reset undo confirmed after reset", age: 23*time.Hour + time.Second, postDelay: 2 * time.Second},
		{name: "late historical workflow still validated", age: 23*time.Hour + time.Second, postDelay: time.Second, invalidState: true, wantError: true},
		{name: "current missing parent fails closed", age: time.Minute, wantError: true},
		{name: "historical workflow still validated", age: 24 * time.Hour, invalidState: true, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			a.roots = []slack.Message{historyRoot(clock.now.Add(-30 * time.Hour))}
			o, _, err := s.normalize(a.roots[0])
			if err != nil {
				t.Fatal(err)
			}
			at := clock.now.Add(-tc.age)
			r := actionRecord{Version: 1, Request: ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, RequestID: "undo", Kind: "unsnooze"}, Actor: "operator", At: at, Unsnooze: "rule-on-pruned-parent"}
			if tc.invalidState {
				r.State.AcknowledgedAt, r.State.AcknowledgedBy = &at, "operator"
			}
			message, err := s.actionMessage(r, o)
			if err != nil {
				t.Fatal(err)
			}
			a.replies[o.Identity.Timestamp] = []slack.Message{{Msg: slack.Msg{Timestamp: historyRoot(at.Add(tc.postDelay)).Timestamp, ThreadTimestamp: o.Identity.Timestamp, User: "U_WRITER", Text: message.Text, Metadata: message.Metadata}}}
			err = s.Sync(context.Background())
			if (err != nil) != tc.wantError {
				t.Fatalf("want error %v, got %v", tc.wantError, err)
			}
		})
	}
}

// TestExpiredAction refuses both new and idempotent requests for hidden roots.
func TestExpiredAction(t *testing.T) {
	cases := []struct {
		name    string
		reload  bool
		advance time.Duration
		pending bool
	}{
		{name: "already expired"},
		{name: "expires during reload", reload: true, advance: 2 * time.Second},
		{name: "pruned during slow reload", reload: true, advance: 25 * time.Hour},
		{name: "uncertain delivery expires", pending: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			a.roots = []slack.Message{historyRoot(clock.now.Add(-DefaultHistoryWindow + time.Second))}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			o := s.Snapshot().Occurrences[0]
			req := ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, Kind: "acknowledge", RequestID: "request"}
			if tc.pending {
				a.postError = errors.New("connection lost")
				if _, err := s.Act(context.Background(), "operator", req); !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("expected uncertain delivery, got %v", err)
				}
			}
			posts := a.posts
			if tc.reload {
				a.advanceOnReply = tc.advance
			} else {
				clock.now = clock.now.Add(2 * time.Second)
			}
			if _, err := s.Act(context.Background(), "operator", req); !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected expired target rejection, got %v", err)
			}
			if a.posts != posts {
				t.Fatal("expired action posted to Slack")
			}
			clock.now = clock.now.Add(DefaultHistoryWindow + 25*time.Hour)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.currentCache().Pending) != 0 {
				t.Fatal("expired delivery guard retained")
			}
		})
	}
}
