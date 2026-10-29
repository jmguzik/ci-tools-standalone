package opsalerts

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestPreviewScopes keeps confirmation within the selected source/channel and retained history.
func TestPreviewScopes(t *testing.T) {
	cases := []struct {
		name, kind, pattern, target, phase string
		count, wantCount                   int
		wantError                          error
	}{
		{name: "current open matches only", kind: "close", pattern: "^etcdDown$", target: "0", wantCount: 2},
		{name: "WIP preview", kind: "acknowledge", pattern: "^etcdDown$", target: "0", wantCount: 2},
		{name: "snooze preview", kind: "snooze", pattern: "^etcdDown$", target: "0", wantCount: 2},
		{name: "missing target", kind: "close", pattern: "^etcdDown$", target: "missing", wantError: ErrNotFound},
		{name: "different expression", kind: "close", pattern: "^another$", target: "0", wantError: ErrValidation},
		{name: "empty matching", kind: "close", pattern: ".*", target: "0", wantError: ErrValidation},
		{name: "invalid expression", kind: "close", pattern: "[", target: "0", wantError: ErrValidation},
		{name: "unsupported action", kind: "investigate", pattern: "^etcdDown$", target: "0", wantError: ErrValidation},
		{name: "degraded snapshot", kind: "close", pattern: "^etcdDown$", target: "0", phase: "degraded", wantError: ErrNotReady},
		{name: "explicit batch cap", kind: "close", pattern: "^etcdDown$", target: "0", count: 201, wantError: ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, archive, clock := fixture(t)
			cache := svc.currentCache()
			cache.Coverage.Phase = "ready"
			if tc.phase != "" {
				cache.Coverage.Phase = tc.phase
			}
			count := tc.count
			if count == 0 {
				count = 7
			}
			for i := range count {
				identity := fmt.Sprint(i)
				o := Occurrence{Identity: OccurrenceIdentity{ID: identity, Channel: "C_TEST", Timestamp: fmt.Sprintf("%d.000001", clock.now.Add(-time.Hour).Unix()+int64(i)), Revision: "1"}, Message: Message{Source: "alerts", Identifiers: []string{"etcdDown"}, ReportedAt: clock.now.Add(-time.Hour)}, View: "feed"}
				if tc.count == 0 {
					switch i {
					case 2:
						o.Identity.Channel = "C_OTHER"
					case 3:
						o.Message.Source = "other"
					case 4:
						o.Classification.Category = "chai_reports"
						o.View = "chai_reports"
					case 5:
						o.Message.ReportedAt = clock.now.Add(-49 * time.Hour)
					case 6:
						o.State.ClosedBy = "operator"
						o.State.ClosedAt = &clock.now
					}
				}
				cache.Occurrences[identity] = o
			}
			svc.publish(cache)
			got, err := svc.Preview(PreviewRequest{OccurrenceID: tc.target, Kind: tc.kind, Pattern: tc.pattern})
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("want %v, got %v", tc.wantError, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.wantCount, len(got)); diff != "" {
					t.Errorf("unexpected matches (-want +got):\n%s", diff)
				}
			}
			if archive.posts != 0 || archive.replyCalls != 0 {
				t.Fatalf("preview called Slack: posts=%d replies=%d", archive.posts, archive.replyCalls)
			}
		})
	}
}
