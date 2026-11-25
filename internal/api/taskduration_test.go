package api

import (
	"testing"
	"time"

	"whatbark/internal/store"
)

func strPtr(s string) *string { return &s }

// TestTaskDuration: table-driven unit tests for the server-computed task
// duration (task-duration-artifacts A1/A2).
func TestTaskDuration(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	created := "2026-11-13 10:00:00" // schema default layout (UTC)
	started := "2026-11-13T10:01:00Z"
	finished := "2026-11-13T10:05:52Z"

	tests := []struct {
		name    string
		task    store.Task
		wantSec int
		wantL   string
		wantOK  bool
	}{
		{
			name:   "queued omitted",
			task:   store.Task{Status: store.TaskQueued, CreatedAt: created},
			wantOK: false,
		},
		{
			name:    "done spans started->finished",
			task:    store.Task{Status: store.TaskDone, CreatedAt: created, StartedAt: strPtr(started), FinishedAt: strPtr(finished)},
			wantSec: 292,
			wantL:   "4m 52s",
			wantOK:  true,
		},
		{
			name:    "running uses now as end",
			task:    store.Task{Status: store.TaskRunning, CreatedAt: created, StartedAt: strPtr(started)},
			wantSec: 7140,
			wantL:   "1h 59m",
			wantOK:  true,
		},
		{
			name:    "failed without started falls back to created",
			task:    store.Task{Status: store.TaskFailed, CreatedAt: created, FinishedAt: strPtr(finished)},
			wantSec: 352,
			wantL:   "5m 52s",
			wantOK:  true,
		},
		{
			name:    "canceled without finished falls back to now",
			task:    store.Task{Status: store.TaskCanceled, CreatedAt: created, StartedAt: strPtr(started)},
			wantSec: 7140,
			wantL:   "1h 59m",
			wantOK:  true,
		},
		{
			name:    "negative span clamps to zero",
			task:    store.Task{Status: store.TaskDone, CreatedAt: created, StartedAt: strPtr("2026-11-13T10:06:00Z"), FinishedAt: strPtr("2026-11-13T10:05:00Z")},
			wantSec: 0,
			wantL:   "0s",
			wantOK:  true,
		},
		{
			name:   "unparseable start",
			task:   store.Task{Status: store.TaskDone, CreatedAt: "not-a-time", StartedAt: strPtr("bogus")},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sec, label, ok := taskDuration(tc.task, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (sec=%d label=%q)", ok, tc.wantOK, sec, label)
			}
			if !ok {
				return
			}
			if sec != tc.wantSec || label != tc.wantL {
				t.Fatalf("got (%d, %q), want (%d, %q)", sec, label, tc.wantSec, tc.wantL)
			}
		})
	}
}

func TestFormatTaskDuration(t *testing.T) {
	tests := []struct {
		sec  int
		want string
	}{
		{0, "0s"}, {1, "1s"}, {59, "59s"},
		{60, "1m"}, {119, "1m 59s"}, {3599, "59m 59s"},
		{3600, "1h"}, {3661, "1h 1m"}, {7325, "2h 2m"},
	}
	for _, tc := range tests {
		if got := formatTaskDuration(tc.sec); got != tc.want {
			t.Errorf("formatTaskDuration(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}
