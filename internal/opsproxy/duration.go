package opsproxy

import (
	"fmt"
	"time"
)

// AckUntil returns the silence endsAt for a locked duration spec.
// monday is next Monday 00:00 UTC; if now is already Monday, that is the following Monday.
func AckUntil(spec string, now time.Time) (time.Time, error) {
	now = now.UTC()
	switch spec {
	case "2h":
		return now.Add(2 * time.Hour), nil
	case "4h":
		return now.Add(4 * time.Hour), nil
	case "8h":
		return now.Add(8 * time.Hour), nil
	case "16h":
		return now.Add(16 * time.Hour), nil
	case "24h":
		return now.Add(24 * time.Hour), nil
	case "2d":
		return now.Add(48 * time.Hour), nil
	case "monday":
		return NextMondayUTC(now), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported ack duration %q (want 2h, 4h, 8h, 16h, 24h, 2d, monday)", spec)
	}
}

// NextMondayUTC is 00:00 UTC on the next Monday after now. If now is Monday (including 00:00),
// the result is seven days later, not today.
func NextMondayUTC(now time.Time) time.Time {
	now = now.UTC()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	daysUntilMonday := (int(time.Monday) - int(now.Weekday()) + 7) % 7
	if daysUntilMonday == 0 {
		daysUntilMonday = 7
	}
	return today.AddDate(0, 0, daysUntilMonday)
}
