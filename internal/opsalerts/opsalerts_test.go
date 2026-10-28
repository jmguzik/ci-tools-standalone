package opsalerts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// testClassification follows the release file's schema without duplicating its catalog.
const testClassification = `version: 1
defaults: {color: orange, category: other}
source_policies:
  chai_reports: {kind: report, color: green, category: chai_reports, view: chai_reports}
  ship_outages:
    kind: alert
    color: yellow
    category: other
    component_categories:
    - {component: Build Farm, category: cluster_fleet}
    - {component: Downstream CI, subcomponent: CI-Config, category: repo_automation}
platform_alerts:
- {alertname_glob: '*etcd*', color: red, category: cluster_fleet}
legacy_alerts:
- {alertname: hive-blocked-deprovision, color: orange, category: cloud_cleanup}
`

// fixedClock advances expiry without sleeps.
type fixedClock struct{ now time.Time }

// Now implements Clock.
func (c *fixedClock) Now() time.Time { return c.now }

// memoryArchive models confirmed, rejected and ambiguously delivered thread writes.
type memoryArchive struct {
	roots          []slack.Message
	replies        map[string][]slack.Message
	clock          *fixedClock
	posts          int
	postError      error
	storeOnError   bool
	historyError   error
	repliesError   error
	replyCalls     int
	lastText       string
	reactionError  error
	reactions      []reactionCall
	cancelOnReply  context.CancelFunc
	advanceOnReply time.Duration
}

// reactionCall captures only optional presentation writes.
type reactionCall struct {
	Root, Emoji string
	Present     bool
}

// History implements Archive.
func (a *memoryArchive) History(context.Context, string, time.Time) ([]slack.Message, error) {
	return append([]slack.Message(nil), a.roots...), a.historyError
}

// Replies implements Archive.
func (a *memoryArchive) Replies(_ context.Context, _ string, root string) ([]slack.Message, error) {
	a.replyCalls++
	a.clock.now = a.clock.now.Add(a.advanceOnReply)
	if a.cancelOnReply != nil {
		a.cancelOnReply()
	}
	return append([]slack.Message(nil), a.replies[root]...), a.repliesError
}

// Post implements Archive without automatically retrying writes.
func (a *memoryArchive) Post(_ context.Context, _ string, root, id, text string) (string, error) {
	a.posts++
	a.lastText = text
	ts := fmt.Sprintf("%d.%06d", a.clock.Now().Unix(), a.posts)
	if a.postError == nil || a.storeOnError {
		a.replies[root] = append(a.replies[root], slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: root, User: "U_WRITER", BotID: "B_WRITER", ClientMsgID: id, Text: text}})
		for i := range a.roots {
			if a.roots[i].Timestamp == root {
				a.roots[i].ReplyCount++
				a.roots[i].LatestReply = ts
			}
		}
	}
	return ts, a.postError
}

// SetReaction implements decoration independently of state.
func (a *memoryArchive) SetReaction(_ context.Context, _ string, root, emoji string, present bool) error {
	a.reactions = append(a.reactions, reactionCall{Root: root, Emoji: emoji, Present: present})
	return a.reactionError
}

// fixture creates retained alerts with repeat identifiers and a separate source.
func fixture(t *testing.T) (*Service, *memoryArchive, *fixedClock) {
	t.Helper()
	clock := &fixedClock{now: time.Date(2026, 10, 28, 5, 0, 0, 0, time.UTC)}
	config := Config{WorkspaceID: "T_TEST", ChannelID: "C_TEST", WorkspaceURL: "https://example.slack.com", Cutover: clock.now.Add(-24 * time.Hour), Writer: Identity{UserID: "U_WRITER", BotID: "B_WRITER"}, Sources: []Source{{Name: "alerts", Kind: "alertmanager", BotIDs: []string{"B_ALERTS"}}, {Name: "prow", Kind: "prow", BotIDs: []string{"B_PROW"}}, {Name: "ship", Kind: "ship", BotIDs: []string{"B_SHIP"}}, {Name: "chai", Kind: "chai", UserIDs: []string{"U_CHAI"}}}}
	classifier, err := LoadClassification([]byte(testClassification))
	if err != nil {
		t.Fatal(err)
	}
	archive := &memoryArchive{clock: clock, replies: map[string][]slack.Message{}}
	for i := range 3 {
		archive.roots = append(archive.roots, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000000", clock.now.Add(-time.Hour).Unix()+int64(i)), BotID: "B_ALERTS", Text: "Alert: etcdDown"}})
	}
	svc, err := NewService(config, classifier, archive, clock)
	if err != nil {
		t.Fatal(err)
	}
	return svc, archive, clock
}

// findOccurrence avoids depending on queue sort order in assertions.
func findOccurrence(t *testing.T, s *Service, id string) Occurrence {
	t.Helper()
	for _, o := range s.Snapshot().Occurrences {
		if o.Identity.ID == id {
			return o
		}
	}
	t.Fatalf("missing occurrence %s", id)
	return Occurrence{}
}

// TestLoadClassification validates values, schema and whole-name globs.
func TestLoadClassification(t *testing.T) {
	cases := []struct {
		name, input string
		wantError   bool
	}{
		{name: "release schema", input: testClassification},
		{name: "version", input: strings.Replace(testClassification, "version: 1", "version: 2", 1), wantError: true},
		{name: "invalid color", input: strings.Replace(testClassification, "color: red", "color: green", 1), wantError: true},
		{name: "invalid category", input: strings.Replace(testClassification, "category: cloud_cleanup", "category: invented", 1), wantError: true},
		{name: "invalid glob", input: strings.Replace(testClassification, "'*etcd*'", "'['", 1), wantError: true},
		{name: "unknown field", input: testClassification + "unexpected: true\n", wantError: true},
		{name: "policy fixed", input: strings.Replace(testClassification, "color: yellow", "color: red", 1), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadClassification([]byte(tc.input))
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v, want error %v", err, tc.wantError)
			}
			if err != nil && !errors.Is(err, ErrValidation) {
				t.Fatalf("wrong sentinel: %v", err)
			}
		})
	}
}

// TestNormalization covers trust, precedence and occurrence boundaries.
func TestNormalization(t *testing.T) {
	cases := []struct {
		name, bot, user, text, subtype, thread string
		want                                   bool
		color, category, origin                string
		identifiers                            []string
	}{
		{name: "trusted marker", bot: "B_ALERTS", text: "etcdDown [ops_color=yellow ops_category=ci_platform]", want: true, color: "yellow", category: "ci_platform", origin: "source", identifiers: []string{"etcdDown"}},
		{name: "fallback", bot: "B_ALERTS", text: "Alert: etcdDown", want: true, color: "red", category: "cluster_fleet", origin: "fallback", identifiers: []string{"etcdDown"}},
		{name: "legacy", bot: "B_ALERTS", text: "alertname=hive-blocked-deprovision", want: true, color: "orange", category: "cloud_cleanup", origin: "fallback", identifiers: []string{"hive-blocked-deprovision"}},
		{name: "forged human", user: "U_HUMAN", text: "Alert: etcdDown [ops_color=red ops_category=ci_platform]"},
		{name: "forged unknown bot", bot: "B_UNKNOWN", text: "Alert: etcdDown [ops_color=red ops_category=ci_platform]", want: true, color: "orange", category: "other", origin: "default"},
		{name: "blank author", text: "[ops_color=red ops_category=ci_platform]"},
		{name: "reserved green", bot: "B_ALERTS", text: "Alert: unknown [ops_color=green ops_category=chai_reports]", want: true, color: "orange", category: "other", origin: "default", identifiers: []string{"unknown"}},
		{name: "grouped", bot: "B_ALERTS", text: "First [ops_color=yellow ops_category=ci_platform]\nSecond [ops_color=red ops_category=cluster_fleet]", want: true, color: "red", category: "other", origin: "source", identifiers: []string{"First", "Second"}},
		{name: "grouped fallback for unmarked alert", bot: "B_ALERTS", text: "First [ops_color=yellow ops_category=ci_platform]\nAlert: etcdDown", want: true, color: "red", category: "other", origin: "source", identifiers: []string{"First", "etcdDown"}},
		{name: "grouped marker cannot cover identifier substring", bot: "B_ALERTS", text: "Alert: etcdDown\nCustometcdDown [ops_color=yellow ops_category=ci_platform]", want: true, color: "red", category: "other", origin: "source", identifiers: []string{"CustometcdDown", "etcdDown"}},
		{name: "scheduled chai", user: "U_CHAI", text: "[Scheduled task: Daily CI] Report [ops_color=red ops_category=ci_platform]", want: true, color: "green", category: "chai_reports", origin: "policy"},
		{name: "conversational chai", user: "U_CHAI", text: "Here are my findings"},
		{name: "ship outage", bot: "B_SHIP", text: "🚨 Outage Detected: Build Farm/Prow\n[ops_color=red ops_category=ci_platform]", want: true, color: "yellow", category: "cluster_fleet", origin: "policy", identifiers: []string{"Build Farm/Prow"}},
		{name: "ship ci config", bot: "B_SHIP", text: "🚨 Outage Detected: Downstream CI/CI-Config", want: true, color: "yellow", category: "repo_automation", origin: "policy", identifiers: []string{"Downstream CI/CI-Config"}},
		{name: "ship outage caused by upgrade", bot: "B_SHIP", text: "🚨 Outage Detected: Build Farm/Prow\nDescription: an upgrade caused this outage", want: true, color: "yellow", category: "cluster_fleet", origin: "policy", identifiers: []string{"Build Farm/Prow"}},
		{name: "ship unknown component body cannot override", bot: "B_SHIP", text: "🚨 Outage Detected: Something/Else\nDescription: unlike Build Farm", want: true, color: "yellow", category: "other", origin: "policy", identifiers: []string{"Something/Else"}},
		{name: "ship upgrade", bot: "B_SHIP", text: "Build Farm upgrade, no outage expected", want: true, color: "orange", category: "other", origin: "default"},
		{name: "prow job", bot: "B_PROW", text: "Job periodic-ci-cleanup failed [ops_color=yellow ops_category=cloud_cleanup]", want: true, color: "yellow", category: "cloud_cleanup", origin: "source", identifiers: []string{"periodic-ci-cleanup"}},
		{name: "writer root", user: "U_WRITER", text: "test"},
		{name: "ordinary reply", bot: "B_ALERTS", text: "Alert: etcdDown", thread: "123.000000"},
		{name: "system message", bot: "B_ALERTS", text: "Alert: etcdDown", subtype: slack.MsgSubTypeChannelJoin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			m := slack.Message{Msg: slack.Msg{Timestamp: a.roots[0].Timestamp, BotID: tc.bot, User: tc.user, Text: tc.text, SubType: tc.subtype, ThreadTimestamp: tc.thread}}
			o, admit, err := s.normalize(m)
			if err != nil {
				t.Fatal(err)
			}
			if admit != tc.want {
				t.Fatalf("admitted=%v want=%v", admit, tc.want)
			}
			if !admit {
				return
			}
			want := Classification{Color: tc.color, Category: tc.category, Origin: tc.origin}
			if diff := cmp.Diff(want, o.Classification); diff != "" {
				t.Errorf("classification (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.identifiers, o.Message.Identifiers); diff != "" {
				t.Errorf("identifiers (-want +got):\n%s", diff)
			}
		})
	}
}

// TestScheduledTaskMetadata exposes only trusted Chai task IDs to the report view.
func TestScheduledTaskMetadata(t *testing.T) {
	cases := []struct {
		name, user, bot, text, wantTask, wantCategory string
		task                                          any
	}{
		{name: "metadata-only scheduled report", user: "U_CHAI", text: "Coverage: checked the configured scope", task: " pending_workloads_scan ", wantTask: "pending_workloads_scan", wantCategory: "chai_reports"},
		{name: "metadata survives a changed header", user: "U_CHAI", text: "[Scheduled task: old_name | Instructions: changed/path.md]\nFindings", task: "ci_control_plane_deployments", wantTask: "ci_control_plane_deployments", wantCategory: "chai_reports"},
		{name: "non-string metadata leaves header fallback", user: "U_CHAI", text: "[Scheduled task: csr_nodes_machine_health]\nFindings", task: 17, wantCategory: "chai_reports"},
		{name: "unknown producer cannot claim a Chai task", bot: "B_UNKNOWN", text: "[Scheduled task: pending_workloads_scan]\nFindings", task: "pending_workloads_scan", wantCategory: "other"},
		{name: "trusted alert producer cannot claim a Chai task", bot: "B_ALERTS", text: "Alert: etcdDown", task: "pending_workloads_scan", wantCategory: "cluster_fleet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, archive, _ := fixture(t)
			message := slack.Message{Msg: slack.Msg{Timestamp: archive.roots[0].Timestamp, User: tc.user, BotID: tc.bot, Text: tc.text, Metadata: slack.SlackMetadata{EventType: "scheduled_report", EventPayload: map[string]interface{}{"scheduled_task": tc.task}}}}
			got, admitted, err := s.normalize(message)
			if err != nil || !admitted {
				t.Fatalf("expected admitted report or alert, got admitted=%v error=%v", admitted, err)
			}
			if diff := cmp.Diff([]string{tc.wantTask, tc.wantCategory}, []string{got.Message.ScheduledTask, got.Classification.Category}); diff != "" {
				t.Errorf("unexpected task/category (-want +got):\n%s", diff)
			}
		})
	}
}

// TestIdentityConfiguration prevents conflicting or empty trust assignments.
func TestIdentityConfiguration(t *testing.T) {
	cases := []struct{ name, change string }{{"blank identity", "blank"}, {"overlap writer", "writer"}, {"overlap sources", "overlap"}, {"bad regexp", "regexp"}, {"empty regexp", "emptyregexp"}, {"insecure permalink", "url"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := s.cfg
			cfg.Sources = append([]Source(nil), cfg.Sources...)
			switch tc.change {
			case "blank":
				cfg.Sources[0].BotIDs = []string{""}
			case "writer":
				cfg.Sources[0].UserIDs = []string{"U_WRITER"}
			case "overlap":
				cfg.Sources[1].BotIDs = []string{"B_ALERTS"}
			case "regexp":
				cfg.Sources[0].IdentifierPattern = "["
			case "emptyregexp":
				cfg.Sources[0].IdentifierPattern = ".*"
			case "url":
				cfg.WorkspaceURL = "http://example.slack.com"
			}
			_, err := NewService(cfg, s.classifier, s.archive, s.clock)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("want validation, got %v", err)
			}
		})
	}
}

// TestWorkflowReplay covers local state, explicit undo, deduplication and restart.
func TestWorkflowReplay(t *testing.T) {
	cases := []struct {
		name        string
		kinds       []string
		wantView    string
		ack, closed bool
	}{
		{"acknowledge", []string{"acknowledge"}, "acknowledged", true, false},
		{"close independently", []string{"close"}, "closed", false, true},
		{"ack close", []string{"acknowledge", "close"}, "closed", true, true},
		{"reopen restores ack", []string{"acknowledge", "close", "reopen"}, "acknowledged", true, false},
		{"explicit undo", []string{"acknowledge", "unacknowledge"}, "feed", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			for i, kind := range tc.kinds {
				o := findOccurrence(t, s, id)
				req := ActionRequest{OccurrenceID: id, Kind: kind, ExpectedRevision: o.Identity.Revision, RequestID: fmt.Sprintf("request-%d", i)}
				result, err := s.Act(ctx, "operator", req)
				if err != nil {
					t.Fatal(err)
				}
				posts := a.posts
				retry, err := s.Act(ctx, "operator", req)
				if err != nil || a.posts != posts || retry.Identity.Revision != result.Identity.Revision {
					t.Fatalf("retry posted or changed: %v", err)
				}
				req.Kind = "close"
				if kind == "close" {
					req.Kind = "reopen"
				}
				if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrConflict) {
					t.Fatalf("reused ID: %v", err)
				}
			}
			o := findOccurrence(t, s, id)
			if o.View != tc.wantView || (o.State.AcknowledgedAt != nil) != tc.ack || (o.State.ClosedAt != nil) != tc.closed {
				t.Fatalf("wrong workflow %+v", o)
			}
			other := findOccurrence(t, s, "C_TEST:"+a.roots[1].Timestamp)
			if other.View != "feed" {
				t.Fatal("workflow leaked to repeated occurrence")
			}
			restarted, err := NewService(s.cfg, s.classifier, a, s.clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(o, findOccurrence(t, restarted, id)); diff != "" {
				t.Errorf("restart (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSnoozeScopeExpiry verifies UTC boundaries, cross-occurrence matching and restoration.
func TestSnoozeScopeExpiry(t *testing.T) {
	cases := []struct {
		name    string
		hour    int
		wantDay int
		undo    bool
		closed  bool
	}{{"before six", 5, 28, false, false}, {"exact six", 6, 29, false, false}, {"after six", 7, 29, false, false}, {"explicit unsnooze", 5, 28, true, false}, {"closed precedes snooze", 5, 28, false, true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			c.now = time.Date(2026, 10, 28, tc.hour, 0, 0, 0, time.UTC)
			a.roots[2].BotID = "B_PROW"
			a.roots[2].Text = "Job periodic-ci-cleanup failed"
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			o := findOccurrence(t, s, id)
			o, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: o.Identity.Revision, RequestID: "ack"})
			if err != nil {
				t.Fatal(err)
			}
			o, err = s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "snooze", ExpectedRevision: o.Identity.Revision, RequestID: "snooze", Pattern: "^etcd.*$"})
			if err != nil {
				t.Fatal(err)
			}
			until := time.Date(2026, 10, tc.wantDay, 6, 0, 0, 0, time.UTC)
			if o.Snooze == nil || !o.Snooze.Until.Equal(until) {
				t.Fatalf("wrong expiry %+v", o.Snooze)
			}
			otherID := "C_TEST:" + a.roots[1].Timestamp
			if findOccurrence(t, s, otherID).View != "snoozed" {
				t.Fatal("repeat did not inherit rule")
			}
			if findOccurrence(t, s, "C_TEST:"+a.roots[2].Timestamp).View != "feed" {
				t.Fatal("rule crossed source scope")
			}
			repeat := findOccurrence(t, s, otherID)
			if _, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: otherID, Kind: "snooze", ExpectedRevision: repeat.Identity.Revision, RequestID: "extend"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("repeat extended rule: %v", err)
			}
			restarted, err := NewService(s.cfg, s.classifier, a, c)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			s = restarted
			o = findOccurrence(t, s, id)
			if tc.undo {
				o, err = s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "unsnooze", ExpectedRevision: o.Identity.Revision, RequestID: "unsnooze"})
				if err != nil {
					t.Fatal(err)
				}
				if findOccurrence(t, s, otherID).View != "feed" {
					t.Fatal("unsnooze did not cancel globally")
				}
			}
			if tc.closed {
				o, err = s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "close", ExpectedRevision: o.Identity.Revision, RequestID: "close"})
				if err != nil {
					t.Fatal(err)
				}
				if o.View != "closed" {
					t.Fatal("closed lost precedence")
				}
			}
			c.now = until
			o = findOccurrence(t, s, id)
			want := "acknowledged"
			if tc.closed {
				want = "closed"
			}
			if o.View != want || o.Snooze != nil {
				t.Fatalf("expiry lost workflow: %+v", o)
			}
		})
	}
}

// TestDeliveryAndConcurrency prevents optimistic success and duplicate uncertain posts.
func TestDeliveryAndConcurrency(t *testing.T) {
	cases := []struct {
		name       string
		postError  error
		store      bool
		want       error
		reconciled bool
	}{
		{"explicit rejection", fmt.Errorf("%w: denied", ErrDeliveryFailed), false, ErrDeliveryFailed, false},
		{"uncertain absent", errors.New("transport timeout"), false, ErrDeliveryUncertain, false},
		{"uncertain delivered", errors.New("transport timeout"), true, ErrDeliveryUncertain, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			o := findOccurrence(t, s, id)
			req := ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: o.Identity.Revision, RequestID: "first"}
			a.postError = tc.postError
			a.storeOnError = tc.store
			_, err := s.Act(ctx, "operator", req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if findOccurrence(t, s, id).State.AcknowledgedAt != nil {
				t.Fatal("unconfirmed write applied")
			}
			a.postError = nil
			a.storeOnError = false
			_, err = s.Act(ctx, "operator", req)
			if tc.reconciled {
				if err != nil || a.posts != 1 {
					t.Fatalf("failed reconciliation: posts=%d err=%v", a.posts, err)
				}
			} else if errors.Is(tc.want, ErrDeliveryFailed) {
				if err != nil || a.posts != 2 {
					t.Fatalf("safe rejected retry failed: %v", err)
				}
			} else {
				if !errors.Is(err, ErrDeliveryUncertain) || a.posts != 1 {
					t.Fatalf("blind uncertain repost: %v", err)
				}
			}
			req.Kind = "close"
			if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrConflict) {
				t.Fatalf("request ID reused: %v", err)
			}
		})
	}
}

// TestChaiInvestigation verifies one same-thread request, reply attribution and failure overlays.
func TestChaiInvestigation(t *testing.T) {
	cases := []struct {
		name    string
		failure error
		store   bool
	}{{"confirmed", nil, false}, {"uncertain", errors.New("timeout"), false}, {"reconciled uncertain", errors.New("timeout"), true}, {"rejected", ErrDeliveryFailed, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			o := findOccurrence(t, s, id)
			req := ActionRequest{OccurrenceID: id, Kind: "investigate", ExpectedRevision: o.Identity.Revision, RequestID: "chai-1", Question: "What evidence explains this alert?"}
			a.postError = tc.failure
			a.storeOnError = tc.store
			o, err := s.Act(ctx, "operator", req)
			if tc.failure == nil && err != nil {
				t.Fatal(err)
			}
			if tc.failure != nil && err == nil {
				t.Fatal("failed delivery returned success")
			}
			if o.State.AcknowledgedAt != nil || o.State.ClosedAt != nil {
				t.Fatal("investigation changed workflow")
			}
			for _, part := range []string{"<@U_CHAI>", "Request ID: chai-1", o.Message.SlackURL, "read-only evidence", req.Question, ActionMarker} {
				if !strings.Contains(a.lastText, part) {
					t.Errorf("missing %q in Chai post", part)
				}
			}
			if !strings.Contains(a.lastText, "this thread") {
				t.Fatal("missing same-thread instructions")
			}
			a.postError = nil
			a.storeOnError = false
			// A different request cannot bypass an uncertain or confirmed outstanding one.
			second := req
			second.RequestID = "chai-2"
			second.ExpectedRevision = o.Identity.Revision
			if errors.Is(tc.failure, ErrDeliveryFailed) {
				o, err = s.Act(ctx, "operator", second)
				if err != nil {
					t.Fatal(err)
				}
				if o.Investigation.Status != "sent" {
					t.Fatal("failed attempt hid confirmed retry")
				}
			} else {
				if _, err := s.Act(ctx, "operator", second); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("second question accepted: %v", err)
				}
			}
			if tc.failure != nil && !tc.store && !errors.Is(tc.failure, ErrDeliveryFailed) {
				o = findOccurrence(t, s, id)
				if o.Investigation.Status != "delivery_uncertain" {
					t.Fatal("missing uncertainty")
				}
				o, err = s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: o.Identity.Revision, RequestID: "ack-after-timeout"})
				if !errors.Is(err, ErrDeliveryUncertain) {
					t.Fatalf("uncertain investigation allowed a new mutation: %v", err)
				}
				return
			}
			c.now = c.now.Add(time.Minute)
			root := a.roots[0].Timestamp
			a.replies[root] = append(a.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000001", c.now.Unix()), ThreadTimestamp: root, User: "U_FORGED", Text: "Chai: all fixed"}}, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000002", c.now.Unix()), ThreadTimestamp: root, User: "U_CHAI", Text: "Evidence: the API is unavailable; check the logs."}})
			a.roots[0].ReplyCount += 2
			a.roots[0].LatestReply = fmt.Sprintf("%d.000002", c.now.Unix())
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			o = findOccurrence(t, s, id)
			if o.Investigation.Status != "reply_received" || !strings.Contains(o.Investigation.Reply, "Evidence:") {
				t.Fatalf("reply not reconciled: %+v", o.Investigation)
			}
			if len(s.Snapshot().Occurrences) != 3 {
				t.Fatal("reply created another card")
			}
			if o.State.AcknowledgedAt != nil || o.State.ClosedAt != nil {
				t.Fatal("Chai reply changed workflow")
			}
		})
	}
}

// TestCoverageAndRootLifecycle tests atomic recovery, thread caching and edits/deletions.
func TestCoverageAndRootLifecycle(t *testing.T) {
	cases := []struct{ name, scenario string }{{"bootstrap coverage", "bootstrap"}, {"failed scan retains cache", "history"}, {"thread failure retains cache", "replies"}, {"cached threads", "cache"}, {"edited parent same occurrence", "edit"}, {"deleted parent removed", "delete"}, {"malformed trusted record", "malformed"}, {"forged action ignored", "forged"}, {"stale revision", "stale"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			ctx := context.Background()
			id := "C_TEST:" + a.roots[0].Timestamp
			req := ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: a.roots[0].Timestamp, RequestID: "ack"}
			if tc.scenario == "bootstrap" {
				if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrNotReady) {
					t.Fatal(err)
				}
				a.historyError = errors.New("offline")
				if err := s.Sync(ctx); err == nil {
					t.Fatal("missing sync error")
				}
				if s.Snapshot().Status.Phase != "catching_up" {
					t.Fatal("bootstrap incorrectly ready")
				}
				return
			}
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			before := s.Snapshot()
			switch tc.scenario {
			case "history":
				a.historyError = errors.New("offline")
			case "replies":
				a.roots[0].ReplyCount++
				a.repliesError = errors.New("partial thread")
			case "cache":
				calls := a.replyCalls
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				if calls != a.replyCalls {
					t.Fatal("unchanged thread refetched")
				}
				return
			case "edit":
				a.roots[0].Text = "Alert: hive-blocked-deprovision"
				a.roots[0].Edited = &slack.Edited{Timestamp: a.roots[1].Timestamp}
				a.roots = append(a.roots, a.roots[0])
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				if len(s.Snapshot().Occurrences) != 3 || findOccurrence(t, s, id).Classification.Category != "cloud_cleanup" {
					t.Fatal("edit became duplicate or was not applied")
				}
				return
			case "delete":
				a.roots = a.roots[1:]
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				if len(s.Snapshot().Occurrences) != 2 {
					t.Fatal("deleted occurrence retained")
				}
				return
			case "malformed", "forged":
				user := "U_WRITER"
				if tc.scenario == "forged" {
					user = "U_HUMAN"
				}
				a.replies[a.roots[0].Timestamp] = []slack.Message{{Msg: slack.Msg{Timestamp: a.roots[1].Timestamp, ThreadTimestamp: a.roots[0].Timestamp, User: user, Text: ActionMarker + "{broken}"}}}
				a.roots[0].ReplyCount++
				if tc.scenario == "forged" {
					if err := s.Sync(ctx); err != nil {
						t.Fatal(err)
					}
					if findOccurrence(t, s, id).State.AcknowledgedAt != nil {
						t.Fatal("forged record applied")
					}
					return
				}
			case "stale":
				if _, err := s.Act(ctx, "operator", req); err != nil {
					t.Fatal(err)
				}
				req.Kind = "close"
				req.RequestID = "stale"
				if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrConflict) {
					t.Fatalf("stale mutation accepted: %v", err)
				}
				return
			}
			if err := s.Sync(ctx); err == nil {
				t.Fatal("partial recovery returned success")
			}
			after := s.Snapshot()
			if after.Status.Phase != "degraded" || after.Status.Error == "" {
				t.Fatalf("wrong coverage %+v", after.Status)
			}
			if diff := cmp.Diff(before.Occurrences, after.Occurrences); diff != "" {
				t.Errorf("partial scan changed cache (-want +got):\n%s", diff)
			}
			if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrNotReady) {
				t.Fatalf("degraded mutation accepted: %v", err)
			}
		})
	}
}

// TestActionValidation rejects unbounded expressions and fields before Slack writes.
func TestActionValidation(t *testing.T) {
	cases := []struct {
		name string
		req  ActionRequest
	}{
		{"missing request id", ActionRequest{Kind: "acknowledge"}},
		{"invalid regexp", ActionRequest{Kind: "snooze", Pattern: "[", RequestID: "bad"}},
		{"empty matching regexp", ActionRequest{Kind: "snooze", Pattern: ".*", RequestID: "bad"}},
		{"oversized regexp", ActionRequest{Kind: "snooze", Pattern: strings.Repeat("a", 513), RequestID: "bad"}},
		{"nonmatching regexp", ActionRequest{Kind: "snooze", Pattern: "^different$", RequestID: "bad"}},
		{"blank question", ActionRequest{Kind: "investigate", RequestID: "bad"}},
		{"record injection", ActionRequest{Kind: "investigate", Question: ActionMarker + "{}", RequestID: "bad"}},
		{"extra fields", ActionRequest{Kind: "close", Question: "ignore", RequestID: "bad"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req := tc.req
			req.OccurrenceID = "C_TEST:" + a.roots[0].Timestamp
			req.ExpectedRevision = a.roots[0].Timestamp
			if _, err := s.Act(ctx, "operator", req); !errors.Is(err, ErrValidation) {
				t.Fatalf("wanted validation, got %v", err)
			}
			if a.posts != 0 {
				t.Fatal("invalid action posted")
			}
		})
	}
}

// TestDecorations verifies that optional icons follow durable actions and expiry.
func TestDecorations(t *testing.T) {
	cases := []struct {
		name         string
		kinds        []string
		expire, fail bool
		wantEmoji    string
		wantPresent  bool
	}{
		{name: "ack eyes", kinds: []string{"acknowledge"}, wantEmoji: "eyes", wantPresent: true},
		{name: "close check", kinds: []string{"close"}, wantEmoji: "white_check_mark", wantPresent: true},
		{name: "undo removes eyes", kinds: []string{"acknowledge", "unacknowledge"}, wantEmoji: "eyes"},
		{name: "expiry restores eyes", kinds: []string{"acknowledge", "snooze"}, expire: true, wantEmoji: "eyes", wantPresent: true},
		{name: "reaction failure retains success", kinds: []string{"acknowledge"}, fail: true, wantEmoji: "eyes", wantPresent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			a.reactions = nil
			if tc.fail {
				a.reactionError = errors.New("reaction unavailable")
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			for i, kind := range tc.kinds {
				o := findOccurrence(t, s, id)
				if _, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: kind, ExpectedRevision: o.Identity.Revision, RequestID: fmt.Sprintf("decor-%d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.expire {
				c.now = nextSixUTC(c.now)
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if s.Snapshot().Status.Phase != "ready" {
				t.Fatal("decoration failure degraded coverage")
			}
			found := false
			var present bool
			for _, call := range a.reactions {
				if call.Root == a.roots[0].Timestamp && call.Emoji == tc.wantEmoji {
					found = true
					present = call.Present
				}
			}
			if !found || present != tc.wantPresent {
				t.Fatalf("wrong icon calls: %+v", a.reactions)
			}
			if tc.fail {
				a.reactionError = nil
				a.reactions = nil
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				if len(a.reactions) == 0 {
					t.Fatal("failed icon was not retried")
				}
			}
		})
	}
}

// TestDurableRecordReplay covers duplicate deliveries and trusted corruption.
func TestDurableRecordReplay(t *testing.T) {
	cases := []struct {
		name, change string
		wantError    bool
	}{
		{name: "duplicate delivery", change: "duplicate"},
		{name: "wrong target", change: "target", wantError: true},
		{name: "reused request different record", change: "conflict", wantError: true},
		{name: "inconsistent workflow", change: "state", wantError: true},
		{name: "writer app alone is not trusted", change: "app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			o, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "acknowledge", ExpectedRevision: a.roots[0].Timestamp, RequestID: "record-1"})
			if err != nil {
				t.Fatal(err)
			}
			root := a.roots[0].Timestamp
			record, _, err := decodeRecord(a.lastText, id)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.change {
			case "target":
				record.Request.OccurrenceID = "C_TEST:" + a.roots[1].Timestamp
			case "conflict":
				record.Actor = "someone-else"
			case "state":
				record.Request.RequestID = "record-2"
				record.Request.ExpectedRevision = o.Identity.Revision
				record.Request.Kind = "close"
				record.State.ClosedBy = "operator"
			case "app":
				s.cfg.Writer.AppID = "A_WRITER"
				record.Request.RequestID = "record-2"
				record.Request.ExpectedRevision = o.Identity.Revision
				record.Request.Kind = "close"
				record.State.ClosedBy = "operator"
				now := c.Now()
				record.State.ClosedAt = &now
			}
			text, err := s.actionText(record, o)
			if err != nil {
				t.Fatal(err)
			}
			reply := slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000100", c.now.Unix()), ThreadTimestamp: root, User: "U_WRITER", BotID: "B_WRITER", Text: text}}
			if tc.change == "app" {
				reply.User = "U_UNTRUSTED"
				reply.BotID = "B_UNTRUSTED"
				reply.BotProfile = &slack.BotProfile{AppID: "A_WRITER"}
			}
			a.replies[root] = append(a.replies[root], reply)
			a.roots[0].ReplyCount++
			err = s.Sync(ctx)
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError {
				got := findOccurrence(t, s, id)
				if got.Identity.Revision != o.Identity.Revision || got.State.ClosedAt != nil {
					t.Fatal("duplicate or app-only record changed state")
				}
			}
		})
	}
}

// TestRepeatedChaiInvestigation keeps replies attached to the correct request on replay.
func TestRepeatedChaiInvestigation(t *testing.T) {
	cases := []struct {
		name                  string
		secondReply           bool
		wantStatus, wantReply string
	}{
		{name: "prior reply does not answer new request", wantStatus: "sent"},
		{name: "new reply answers new request", secondReply: true, wantStatus: "reply_received", wantReply: "Second answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, c := fixture(t)
			ctx := context.Background()
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			root := a.roots[0].Timestamp
			id := "C_TEST:" + root
			o, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "investigate", ExpectedRevision: root, RequestID: "chai-first", Question: "First question?"})
			if err != nil {
				t.Fatal(err)
			}
			c.now = c.now.Add(time.Minute)
			replyTS := fmt.Sprintf("%d.000000", c.now.Unix())
			a.replies[root] = append(a.replies[root], slack.Message{Msg: slack.Msg{Timestamp: replyTS, ThreadTimestamp: root, User: "U_CHAI", Text: "First answer"}})
			a.roots[0].ReplyCount++
			a.roots[0].LatestReply = replyTS
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			o = findOccurrence(t, s, id)
			if o.Investigation.Status != "reply_received" {
				t.Fatal("missing first reply")
			}
			c.now = c.now.Add(time.Minute)
			o, err = s.Act(ctx, "operator", ActionRequest{OccurrenceID: id, Kind: "investigate", ExpectedRevision: o.Identity.Revision, RequestID: "chai-second", Question: "Second question?"})
			if err != nil {
				t.Fatal(err)
			}
			if o.Investigation.Status != "sent" || o.Investigation.Reply != "" {
				t.Fatal("old answer attached to new question")
			}
			if tc.secondReply {
				c.now = c.now.Add(time.Minute)
				replyTS = fmt.Sprintf("%d.000000", c.now.Unix())
				a.replies[root] = append(a.replies[root], slack.Message{Msg: slack.Msg{Timestamp: replyTS, ThreadTimestamp: root, User: "U_CHAI", Text: "Second answer"}})
				a.roots[0].ReplyCount++
				a.roots[0].LatestReply = replyTS
			}
			restarted, err := NewService(s.cfg, s.classifier, a, c)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			o = findOccurrence(t, restarted, id)
			if o.Investigation.Status != tc.wantStatus || o.Investigation.Reply != tc.wantReply || o.Investigation.RequestID != "chai-second" {
				t.Fatalf("incorrect replay: %+v", o.Investigation)
			}
			if a.posts != 2 {
				t.Fatal("wrong number of requests")
			}
		})
	}
}

// TestCanceledActions ensures queued or expired requests cannot produce late writes.
func TestCanceledActions(t *testing.T) {
	cases := []struct{ name, when string }{{"already canceled", "before"}, {"canceled with queue occupied", "queued"}, {"canceled during thread reload", "reload"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch tc.when {
			case "before":
				cancel()
			case "queued":
				s.serial <- struct{}{}
				defer s.release()
				cancel()
			case "reload":
				a.cancelOnReply = cancel
			}
			_, err := s.Act(ctx, "operator", ActionRequest{OccurrenceID: "C_TEST:" + a.roots[0].Timestamp, Kind: "acknowledge", ExpectedRevision: a.roots[0].Timestamp, RequestID: "canceled"})
			if !errors.Is(err, context.Canceled) || a.posts != 0 {
				t.Fatalf("canceled action wrote: posts=%d err=%v", a.posts, err)
			}
		})
	}
}
