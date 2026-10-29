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

// scopedAction describes one operator mutation in a replayable workflow sequence.
type scopedAction struct {
	kind, scope, actor string
	target             int
}

// applyScopedAction always uses the current occurrence revision.
func applyScopedAction(t *testing.T, s *Service, a *memoryArchive, action scopedAction) Occurrence {
	t.Helper()
	id := "C_TEST:" + a.roots[action.target].Timestamp
	o := findOccurrence(t, s, id)
	req := ActionRequest{OccurrenceID: id, Kind: action.kind, Scope: action.scope, ExpectedRevision: o.Identity.Revision, RequestID: fmt.Sprintf("action_%d", a.posts+1)}
	result, err := s.Act(context.Background(), action.actor, req)
	if err != nil {
		t.Fatalf("%s %s: %v", action.kind, action.scope, err)
	}
	posts := a.posts
	retry, err := s.Act(context.Background(), action.actor, req)
	if err != nil || a.posts != posts {
		t.Fatalf("idempotent retry posted: posts=%d error=%v", a.posts, err)
	}
	if diff := cmp.Diff(result, retry); diff != "" {
		t.Fatalf("retry (-want +got):\n%s", diff)
	}
	return result
}

// restartScopedService verifies complete durable replay against the current snapshot.
func restartScopedService(t *testing.T, s *Service, a *memoryArchive, clock *fixedClock) *Service {
	t.Helper()
	before := s.Snapshot()
	restarted, err := NewService(s.cfg, s.classifier, a, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before.Occurrences, restarted.Snapshot().Occurrences); diff != "" {
		t.Fatalf("replay (-want +got):\n%s", diff)
	}
	return restarted
}

// TestScopedRules covers current and future firings, identifier requirements and boundaries.
func TestScopedRules(t *testing.T) {
	cases := []struct {
		name, kind, scope, view string
		noIdentifier, family    bool
	}{
		{name: "legacy snooze family", kind: "snooze", view: "snoozed", family: true},
		{name: "explicit snooze family", kind: "snooze", scope: "regex", view: "snoozed", family: true},
		{name: "individual snooze without identifier", kind: "snooze", scope: "occurrence", view: "snoozed", noIdentifier: true},
		{name: "regex WIP", kind: "acknowledge", scope: "regex", view: "acknowledged", family: true},
		{name: "individual WIP", kind: "acknowledge", scope: "occurrence", view: "acknowledged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			a.roots[2].BotID = "B_PROW"
			a.roots[2].Text = "Alert: etcdDown"
			if tc.noIdentifier {
				a.roots[0].Text = "unstructured alert"
			}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := applyScopedAction(t, s, a, scopedAction{kind: tc.kind, scope: tc.scope, actor: "owner"})
			if result.View != tc.view {
				t.Fatalf("view=%s want %s", result.View, tc.view)
			}
			if tc.kind == "snooze" && (result.Snooze == nil || result.Snooze.Scope != tc.scope) {
				t.Fatalf("missing snooze scope: %+v", result.Snooze)
			}
			if tc.noIdentifier && result.Snooze.OccurrenceID != result.Identity.ID {
				t.Fatal("individual snooze lacks occurrence identity")
			}
			for index := 1; index < 3; index++ {
				want := "feed"
				if tc.family && index == 1 {
					want = tc.view
				}
				if got := findOccurrence(t, s, "C_TEST:"+a.roots[index].Timestamp).View; got != want {
					t.Fatalf("target %d view=%s want %s", index, got, want)
				}
			}
			// A different channel cannot inherit either kind of rule.
			other := s.currentCache().Occurrences[result.Identity.ID]
			other.Identity.Channel = "C_OTHER"
			other.Identity.ID = "C_OTHER:" + other.Identity.Timestamp
			other.State = Workflow{}
			if got := effectiveOccurrence(other, s.currentCache(), clock.now).View; got != "feed" {
				t.Fatalf("cross-channel rule: %s", got)
			}
			other.Identity.Channel = "C_TEST"
			other.Message.Source = "other-producer"
			if got := effectiveOccurrence(other, s.currentCache(), clock.now).View; got != "feed" {
				t.Fatalf("cross-source rule with same identifiers: %s", got)
			}
			clock.now = clock.now.Add(time.Minute)
			a.roots = append(a.roots, historyRoot(clock.now))
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantFuture := "feed"
			if tc.family {
				wantFuture = tc.view
			}
			if got := findOccurrence(t, s, "C_TEST:"+a.roots[3].Timestamp).View; got != wantFuture {
				t.Fatalf("future view=%s want %s", got, wantFuture)
			}
			s = restartScopedService(t, s, a, clock)
			clock.now = nextSixUTC(clock.now)
			wantExpired := "feed"
			if tc.kind == "acknowledge" && !tc.family {
				wantExpired = "acknowledged"
			}
			if got := findOccurrence(t, s, result.Identity.ID).View; got != wantExpired {
				t.Fatalf("reset view=%s want %s", got, wantExpired)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestWIPRuleOverrides guards independent ownership and temporary overlays across all actions.
func TestWIPRuleOverrides(t *testing.T) {
	cases := []struct {
		name                 string
		actions              []scopedAction
		before, after, owner string
	}{
		{name: "expiry clears family", before: "acknowledged", after: "feed", actions: []scopedAction{{kind: "acknowledge", scope: "regex", actor: "family"}}},
		{name: "release overrides family", before: "feed", after: "feed", actions: []scopedAction{{kind: "acknowledge", scope: "regex", actor: "family"}, {kind: "unacknowledge", scope: "occurrence", actor: "release"}}},
		{name: "independent ownership survives", before: "acknowledged", after: "acknowledged", owner: "individual", actions: []scopedAction{{kind: "acknowledge", scope: "occurrence", actor: "individual"}, {kind: "acknowledge", scope: "regex", actor: "family"}}},
		{name: "individual ownership replaces family", before: "acknowledged", after: "acknowledged", owner: "individual", actions: []scopedAction{{kind: "acknowledge", scope: "regex", actor: "family"}, {kind: "acknowledge", scope: "occurrence", actor: "individual"}}},
		{name: "family cancellation restores independent", before: "acknowledged", after: "acknowledged", owner: "individual", actions: []scopedAction{{kind: "acknowledge", scope: "occurrence", actor: "individual"}, {kind: "acknowledge", scope: "regex", actor: "family"}, {kind: "unacknowledge", scope: "regex", actor: "cancel", target: 1}}},
		{name: "family snooze close reopen reset", before: "snoozed", after: "feed", actions: []scopedAction{{kind: "acknowledge", scope: "regex", actor: "family"}, {kind: "snooze", scope: "occurrence", actor: "snoozer"}, {kind: "close", actor: "closer"}, {kind: "reopen", actor: "opener"}}},
		{name: "closed has priority", before: "closed", after: "closed", actions: []scopedAction{{kind: "close", actor: "closer"}, {kind: "acknowledge", scope: "regex", actor: "family", target: 1}}},
		{name: "newer family supersedes release", before: "acknowledged", after: "feed", actions: []scopedAction{{kind: "acknowledge", scope: "regex", actor: "old"}, {kind: "unacknowledge", scope: "occurrence", actor: "release"}, {kind: "acknowledge", scope: "regex", actor: "new", target: 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, action := range tc.actions {
				applyScopedAction(t, s, a, action)
			}
			id := "C_TEST:" + a.roots[0].Timestamp
			before := findOccurrence(t, s, id)
			if before.View != tc.before {
				t.Fatalf("before=%s want %s", before.View, tc.before)
			}
			if tc.name == "release overrides family" && findOccurrence(t, s, "C_TEST:"+a.roots[1].Timestamp).View != "acknowledged" {
				t.Fatal("individual release canceled family")
			}
			s = restartScopedService(t, s, a, clock)
			clock.now = nextSixUTC(clock.now)
			after := findOccurrence(t, s, id)
			if diff := cmp.Diff([]string{tc.after, tc.owner}, []string{after.View, after.State.AcknowledgedBy}); diff != "" {
				t.Fatalf("reset (-want +got):\n%s", diff)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestScopedValidation confines scope and patterns to the actions that implement them.
func TestScopedValidation(t *testing.T) {
	cases := []struct {
		name, kind, scope, pattern string
		wantError                  bool
	}{
		{name: "regex acknowledgement", kind: "acknowledge", scope: "regex", pattern: "etcd.*"},
		{name: "individual acknowledgement pattern", kind: "acknowledge", scope: "occurrence", pattern: "etcd.*", wantError: true},
		{name: "individual snooze pattern", kind: "snooze", scope: "occurrence", pattern: "etcd.*", wantError: true},
		{name: "unknown scope", kind: "snooze", scope: "all", wantError: true},
		{name: "close scope", kind: "close", scope: "regex", wantError: true},
		{name: "cancel pattern", kind: "unacknowledge", scope: "regex", pattern: "etcd.*", wantError: true},
		{name: "empty matching regex", kind: "acknowledge", scope: "regex", pattern: ".*", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ActionRequest{OccurrenceID: "C:1", ExpectedRevision: "1", RequestID: "request", Kind: tc.kind, Scope: tc.scope, Pattern: tc.pattern}
			err := validateAction("operator", req)
			if (err != nil) != tc.wantError {
				t.Fatalf("want error %v, got %v", tc.wantError, err)
			}
		})
	}
}

// TestScopedUncertainDelivery ensures family assignments appear only after confirmed recovery.
func TestScopedUncertainDelivery(t *testing.T) {
	cases := []struct {
		name   string
		stored bool
	}{{name: "absent"}, {name: "delivered", stored: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _ := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			o := findOccurrence(t, s, "C_TEST:"+a.roots[0].Timestamp)
			req := ActionRequest{OccurrenceID: o.Identity.ID, ExpectedRevision: o.Identity.Revision, RequestID: "uncertain", Kind: "acknowledge", Scope: "regex"}
			a.postError = errors.New("timeout")
			a.storeOnError = tc.stored
			if _, err := s.Act(context.Background(), "owner", req); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("uncertainty=%v", err)
			}
			if got := findOccurrence(t, s, "C_TEST:"+a.roots[1].Timestamp).View; got != "feed" {
				t.Fatalf("unconfirmed family=%s", got)
			}
			a.postError = nil
			_, err := s.Act(context.Background(), "owner", req)
			if tc.stored && err != nil || !tc.stored && !errors.Is(err, ErrDeliveryUncertain) || a.posts != 1 {
				t.Fatalf("retry: posts=%d error=%v", a.posts, err)
			}
		})
	}
}

// TestWIPBufferedRecovery keeps rules when their creating parent leaves the visible queue.
func TestWIPBufferedRecovery(t *testing.T) {
	cases := []struct {
		name   string
		cancel bool
	}{{name: "expiry"}, {name: "cancel", cancel: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			a.roots = []slack.Message{historyRoot(clock.now.Add(-DefaultHistoryWindow + time.Second)), historyRoot(clock.now.Add(-time.Minute))}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "family"})
			clock.now = clock.now.Add(2 * time.Second)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			s = restartScopedService(t, s, a, clock)
			if got := s.Snapshot().Occurrences; len(got) != 1 || got[0].View != "acknowledged" {
				t.Fatalf("buffered family lost: %+v", got)
			}
			if tc.cancel {
				applyScopedAction(t, s, a, scopedAction{kind: "unacknowledge", scope: "regex", actor: "cancel", target: 1})
			}
			clock.now = nextSixUTC(clock.now)
			s = restartScopedService(t, s, a, clock)
			if got := s.Snapshot().Occurrences[0].View; got != "feed" {
				t.Fatalf("expired family=%s", got)
			}
		})
	}
}

// TestHistoricalWIPOverrides recovers explicit ownership after its old family parent is pruned.
func TestHistoricalWIPOverrides(t *testing.T) {
	cases := []struct {
		name, kind, scope, view, owner string
		independent                    bool
	}{
		{name: "release", kind: "unacknowledge", scope: "occurrence", view: "feed"},
		{name: "legacy release", kind: "unacknowledge", view: "feed"},
		{name: "assign", kind: "acknowledge", scope: "occurrence", view: "acknowledged", owner: "new"},
		{name: "replace independent", kind: "acknowledge", scope: "occurrence", view: "acknowledged", owner: "new", independent: true},
		{name: "legacy replace independent", kind: "acknowledge", view: "acknowledged", owner: "new", independent: true},
		{name: "cancel", kind: "unacknowledge", scope: "regex", view: "feed"},
		{name: "close while family active", kind: "close", view: "closed"},
		{name: "snooze while family active", kind: "snooze", scope: "occurrence", view: "feed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			s.cfg.Cutover = time.Time{}
			a.roots = []slack.Message{historyRoot(clock.now.Add(-DefaultHistoryWindow + time.Second)), historyRoot(clock.now.Add(-time.Minute))}
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.independent {
				applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "occurrence", actor: "old", target: 1})
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "family"})
			applyScopedAction(t, s, a, scopedAction{kind: tc.kind, scope: tc.scope, actor: "new", target: 1})
			clock.now = clock.now.Add(25 * time.Hour)
			s = restartScopedService(t, s, a, clock)
			after := s.Snapshot().Occurrences
			if len(after) != 1 {
				t.Fatalf("visible parents=%d", len(after))
			}
			if diff := cmp.Diff([]string{tc.view, tc.owner}, []string{after[0].View, after[0].State.AcknowledgedBy}); diff != "" {
				t.Fatalf("pruned-parent replay (-want +got):\n%s", diff)
			}
			if len(s.currentCache().Threads) != 1 {
				t.Fatal("old rule parent was not pruned")
			}
		})
	}
}

// TestWIPRevisionConflicts prevents stale actions from applying to a replacement family rule.
func TestWIPRevisionConflicts(t *testing.T) {
	cases := []struct{ name, kind, scope, change string }{
		{name: "family cancel after replacement", kind: "unacknowledge", scope: "regex", change: "replace"},
		{name: "individual release after replacement", kind: "unacknowledge", scope: "occurrence", change: "replace"},
		{name: "individual ownership after replacement", kind: "acknowledge", scope: "occurrence", change: "replace"},
		{name: "family cancel after reset", kind: "unacknowledge", scope: "regex", change: "expire"},
		{name: "individual release after reset", kind: "unacknowledge", scope: "occurrence", change: "expire"},
		{name: "individual ownership after cancel", kind: "acknowledge", scope: "occurrence", change: "cancel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "first"})
			id := "C_TEST:" + a.roots[1].Timestamp
			displayed := findOccurrence(t, s, id)
			rawRevision := s.currentCache().Occurrences[id].Identity.Revision
			if displayed.Identity.Revision == rawRevision {
				t.Fatal("displayed revision omitted family rule")
			}
			req := ActionRequest{OccurrenceID: id, Kind: tc.kind, Scope: tc.scope, ExpectedRevision: displayed.Identity.Revision, RequestID: "stale"}
			switch tc.change {
			case "replace":
				applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "second", target: 2})
			case "cancel":
				applyScopedAction(t, s, a, scopedAction{kind: "unacknowledge", scope: "regex", actor: "cancel"})
			case "expire":
				clock.now = nextSixUTC(clock.now)
			}
			if s.currentCache().Occurrences[id].Identity.Revision != rawRevision {
				t.Fatal("other-parent rule changed immutable revision")
			}
			current := findOccurrence(t, s, id)
			if current.Identity.Revision == displayed.Identity.Revision {
				t.Fatal("effective revision did not change")
			}
			posts := a.posts
			if _, err := s.Act(context.Background(), "stale-operator", req); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale action error=%v", err)
			}
			if a.posts != posts {
				t.Fatal("stale action posted")
			}
			if diff := cmp.Diff(current, findOccurrence(t, s, id)); diff != "" {
				t.Fatalf("stale action mutated state (-want +got):\n%s", diff)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestWIPRevisionIdempotency keeps confirmed retries safe after a family replacement.
func TestWIPRevisionIdempotency(t *testing.T) {
	cases := []struct{ name, kind, scope string }{
		{name: "close", kind: "close"},
		{name: "individual ownership", kind: "acknowledge", scope: "occurrence"},
		{name: "individual release", kind: "unacknowledge", scope: "occurrence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "first"})
			id := "C_TEST:" + a.roots[1].Timestamp
			displayed := findOccurrence(t, s, id)
			req := ActionRequest{OccurrenceID: id, Kind: tc.kind, Scope: tc.scope, ExpectedRevision: displayed.Identity.Revision, RequestID: "confirmed"}
			if _, err := s.Act(context.Background(), "operator", req); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "second", target: 2})
			current := findOccurrence(t, s, id)
			posts := a.posts
			result, err := s.Act(context.Background(), "operator", req)
			if err != nil || a.posts != posts {
				t.Fatalf("retry posts=%d want %d, error=%v", a.posts, posts, err)
			}
			if diff := cmp.Diff(current, result); diff != "" {
				t.Fatalf("retry snapshot (-want +got):\n%s", diff)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestWIPObservationAfterProducerEdit preserves durable actions when identifiers are corrected.
func TestWIPObservationAfterProducerEdit(t *testing.T) {
	cases := []struct{ name, kind, scope, view, owner string }{
		{name: "unrelated close", kind: "close", view: "closed"},
		{name: "unrelated snooze", kind: "snooze", scope: "occurrence", view: "snoozed"},
		{name: "individual ownership", kind: "acknowledge", scope: "occurrence", view: "acknowledged", owner: "individual"},
		{name: "individual release", kind: "unacknowledge", scope: "occurrence", view: "feed"},
		{name: "family cancellation", kind: "unacknowledge", scope: "regex", view: "feed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "family"})
			applyScopedAction(t, s, a, scopedAction{kind: tc.kind, scope: tc.scope, actor: "individual", target: 1})
			a.roots[1].Text = "Alert: correctedIdentifier"
			if err := s.Sync(context.Background()); err != nil {
				t.Fatalf("replay after producer edit: %v", err)
			}
			id := "C_TEST:" + a.roots[1].Timestamp
			after := findOccurrence(t, s, id)
			if diff := cmp.Diff([]string{tc.view, tc.owner}, []string{after.View, after.State.AcknowledgedBy}); diff != "" {
				t.Fatalf("edited-parent replay (-want +got):\n%s", diff)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}

// TestWIPDelayedConfirmation preserves valid actions confirmed after another parent cancels the rule.
func TestWIPDelayedConfirmation(t *testing.T) {
	cases := []struct {
		name, kind, scope, view, owner string
		invalidChronology              bool
	}{
		{name: "close", kind: "close", view: "closed"},
		{name: "snooze", kind: "snooze", scope: "occurrence", view: "snoozed"},
		{name: "individual ownership", kind: "acknowledge", scope: "occurrence", view: "acknowledged", owner: "delayed"},
		{name: "individual release", kind: "unacknowledge", scope: "occurrence", view: "feed"},
		{name: "family cancellation", kind: "unacknowledge", scope: "regex", view: "feed"},
		{name: "reject generated after cancellation", kind: "close", invalidChronology: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, clock := fixture(t)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			applyScopedAction(t, s, a, scopedAction{kind: "acknowledge", scope: "regex", actor: "family"})
			id := "C_TEST:" + a.roots[1].Timestamp
			displayed := findOccurrence(t, s, id)
			req := ActionRequest{OccurrenceID: id, Kind: tc.kind, Scope: tc.scope, ExpectedRevision: displayed.Identity.Revision, RequestID: "delayed"}
			prepared, err := s.prepareAction("delayed", req, displayed, s.currentCache())
			if err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(time.Minute)
			applyScopedAction(t, s, a, scopedAction{kind: "unacknowledge", scope: "regex", actor: "cancel", target: 2})
			clock.now = clock.now.Add(time.Minute)
			if tc.invalidChronology {
				prepared.At = clock.now
			}
			text, err := s.actionText(prepared, displayed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Post(context.Background(), s.cfg.ChannelID, displayed.Identity.Timestamp, req.RequestID, text); err != nil {
				t.Fatal(err)
			}
			err = s.Sync(context.Background())
			if tc.invalidChronology {
				if err == nil {
					t.Fatal("accepted an observation generated after cancellation")
				}
				return
			}
			if err != nil {
				t.Fatalf("delayed confirmation degraded archive: %v", err)
			}
			if s.Snapshot().Status.Phase != "ready" || len(s.currentCache().WIPRules) != 0 {
				t.Fatal("canceled family became active")
			}
			after := findOccurrence(t, s, id)
			if diff := cmp.Diff([]string{tc.view, tc.owner}, []string{after.View, after.State.AcknowledgedBy}); diff != "" {
				t.Fatalf("late action (-want +got):\n%s", diff)
			}
			for _, index := range []int{0, 2} {
				if got := findOccurrence(t, s, "C_TEST:"+a.roots[index].Timestamp).View; got != "feed" {
					t.Fatalf("canceled sibling %d=%s", index, got)
				}
			}
			posts := a.posts
			if _, err := s.Act(context.Background(), "delayed", req); err != nil || a.posts != posts {
				t.Fatalf("late confirmed retry: posts=%d error=%v", a.posts, err)
			}
			restartScopedService(t, s, a, clock)
		})
	}
}
