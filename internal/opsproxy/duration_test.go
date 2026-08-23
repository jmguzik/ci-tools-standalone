package opsproxy

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestAckUntil(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 23, 15, 4, 5, 0, time.UTC) // Sunday
	testCases := []struct {
		name    string
		spec    string
		now     time.Time
		want    time.Time
		wantErr bool
	}{
		{name: "2h", spec: "2h", now: now, want: now.Add(2 * time.Hour)},
		{name: "4h", spec: "4h", now: now, want: now.Add(4 * time.Hour)},
		{name: "8h", spec: "8h", now: now, want: now.Add(8 * time.Hour)},
		{name: "16h", spec: "16h", now: now, want: now.Add(16 * time.Hour)},
		{name: "24h", spec: "24h", now: now, want: now.Add(24 * time.Hour)},
		{name: "2d", spec: "2d", now: now, want: now.Add(48 * time.Hour)},
		{
			name: "monday from Sunday",
			spec: "monday",
			now:  now,
			want: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "monday from Monday afternoon is next week",
			spec: "monday",
			now:  time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC),
			want: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "monday from Monday 00:00 UTC is next week not today",
			spec: "monday",
			now:  time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
		},
		{name: "refuse forever", spec: "forever", now: now, wantErr: true},
		{name: "refuse 1h", spec: "1h", now: now, wantErr: true},
		{name: "refuse empty", spec: "", now: now, wantErr: true},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := AckUntil(tc.spec, tc.now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("endsAt mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNextMondayUTC(t *testing.T) {
	t.Parallel()
	got := NextMondayUTC(time.Date(2026, 8, 26, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600)))
	want := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}
