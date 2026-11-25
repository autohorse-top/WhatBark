package api

import (
	"fmt"
	"time"

	"whatbark/internal/store"
)

// taskTimeLayouts lists the timestamp layouts used by the tasks table:
// created_at comes from the schema default datetime('now')
// ("2006-01-02 15:04:05", UTC); started_at/finished_at are RFC3339 UTC.
var taskTimeLayouts = []string{"2006-01-02 15:04:05", time.RFC3339}

// parseTaskTime parses a task timestamp in either layout (UTC).
func parseTaskTime(ts string) (time.Time, bool) {
	for _, layout := range taskTimeLayouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// taskDuration computes the server-side task duration for a row.
//
// Rules:
//   - queued: not started yet, ok=false (keys omitted from the row).
//   - terminal (done/failed/canceled): end = finished_at,
//     start = started_at (falling back to created_at).
//   - running: end = now, start = started_at (falling back to created_at).
//
// A negative span (clock skew, malformed data) clamps to 0; a positive span
// rounds to whole seconds with a 1s floor so sub-second work reads "1s".
// The label uses language-neutral units: "52s", "4m 51s", "1h 2m".
func taskDuration(t store.Task, now time.Time) (sec int, label string, ok bool) {
	if t.Status == store.TaskQueued {
		return 0, "", false
	}
	startRaw := t.CreatedAt
	if t.StartedAt != nil && *t.StartedAt != "" {
		startRaw = *t.StartedAt
	}
	start, startOK := parseTaskTime(startRaw)
	if !startOK {
		return 0, "", false
	}
	end := now
	if t.Status == store.TaskDone || t.Status == store.TaskFailed || t.Status == store.TaskCanceled {
		if t.FinishedAt != nil {
			if f, ok := parseTaskTime(*t.FinishedAt); ok {
				end = f
			}
		}
	}
	d := end.Sub(start)
	if d < 0 {
		d = 0
	}
	sec = int(d.Round(time.Second) / time.Second)
	if d > 0 && sec == 0 {
		sec = 1
	}
	return sec, formatTaskDuration(sec), true
}

// formatTaskDuration renders whole seconds in the smallest meaningful units.
func formatTaskDuration(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		m, s := sec/60, sec%60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		h, m := sec/3600, (sec%3600)/60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
}
