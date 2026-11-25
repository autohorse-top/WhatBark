// Package sched implements the periodic scheduler: it fires recurring
// prompts (schedules), injects self-review tasks for AI employees, posts
// one-time group SLA reminders (S30), and runs the hourly storage cleanup
// (artifact retention + orphan sweep, disk management). Step is
// injectable-clock so tests can advance time deterministically.
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/store"
)

const (
	defaultSelfReviewSeconds = 1800
	// defaultRetentionDays is the artifact retention TTL used when the
	// storage_retention_days setting is absent (0 disables the step).
	defaultRetentionDays = 30
)

// Scheduler ticks periodically and creates tasks for due schedules
// and self-review deadlines.
type Scheduler struct {
	Store *store.Store
	Flow  *flow.Flow
	// Tick is the polling interval (default 1s).
	Tick time.Duration
	// Now is overridable for tests.
	Now func() time.Time
	// lastCleanup throttles stepStorageCleanup to at most one run per hour
	// (zero value → run on the first tick).
	lastCleanup time.Time
}

// New creates a Scheduler with default tick and clock.
func New(st *store.Store, fl *flow.Flow) *Scheduler {
	return &Scheduler{
		Store: st,
		Flow:  fl,
		Tick:  time.Second,
		Now:   time.Now,
	}
}

// Run blocks until ctx is cancelled, ticking on every Tick interval.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Step(s.Now()); err != nil {
				slog.Warn("sched step", "err", err)
			}
		}
	}
}

// Step performs one scheduling pass at the given instant.
func (s *Scheduler) Step(now time.Time) error {
	if err := s.stepSchedules(now); err != nil {
		return err
	}
	if err := s.stepSelfReview(now); err != nil {
		return err
	}
	if err := s.stepGroupSLA(now); err != nil {
		return err
	}
	return s.stepStorageCleanup(now)
}

func (s *Scheduler) stepSchedules(now time.Time) error {
	list, err := s.Store.ListSchedules()
	if err != nil {
		return err
	}
	for _, sc := range list {
		if !sc.Enabled {
			continue
		}
		due := sc.LastFiredAt == nil
		if !due {
			last, perr := time.Parse(time.RFC3339, *sc.LastFiredAt)
			if perr == nil {
				due = now.Sub(last) >= time.Duration(sc.EverySeconds)*time.Second
			}
		}
		if !due {
			continue
		}
		// Send first, then record last_fired_at (S7-Minor12): if the send
		// fails we keep the old timestamp so the next tick retries, rather
		// than marking fired and silently dropping the prompt.
		if _, _, err := s.Flow.Send(0, sc.EmpID, store.KindScheduled, sc.Prompt, nil, nil, nil, 0); err != nil {
			slog.Warn("sched send", "id", sc.ID, "emp", sc.EmpID, "err", err)
			continue
		}
		if err := s.Store.FireSchedule(sc.ID, now.Format(time.RFC3339)); err != nil {
			slog.Warn("sched fire", "id", sc.ID, "err", err)
		}
	}
	return nil
}

func (s *Scheduler) stepSelfReview(now time.Time) error {
	interval := defaultSelfReviewSeconds
	if v, err := s.Store.GetSetting("selfreview_interval_seconds"); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			interval = n
		}
	}
	ivs := time.Duration(interval) * time.Second
	emps, err := s.Store.ListEmployees()
	if err != nil {
		return err
	}
	for _, e := range emps {
		if e.Kind != store.KindAI {
			continue
		}
		last, hasLast := s.lastSelfReview(e.ID)
		if hasLast && now.Sub(last) < ivs {
			continue
		}
		if s.hasPendingSelfReview(e.ID) {
			continue
		}
		if _, _, err := s.Flow.Send(0, e.ID, store.KindSelfReview, "self review: report progress and blockers", nil, nil, nil, 0); err != nil {
			slog.Warn("selfreview", "emp", e.ID, "err", err)
		}
	}
	return nil
}

func (s *Scheduler) lastSelfReview(empID int) (time.Time, bool) {
	var ts string
	err := s.Store.DB().
		QueryRow(`SELECT created_at FROM tasks WHERE employee_id = ? AND kind = 'selfreview' ORDER BY id DESC LIMIT 1`, empID).
		Scan(&ts)
	if err != nil {
		return time.Time{}, false
	}
	return parseTaskTime(ts)
}

// stepGroupSLA (S30): for every group with SLA enabled
// (SlaTimeoutSeconds > 0), post a one-time system reminder into the group
// thread for each open task (queued/running) that has been open longer than
// the threshold. Each task is reminded at most once in its lifetime
// (sla_reminded_at is set after a successful send; the send-then-mark order
// mirrors stepSchedules so a failed send retries on the next tick).
func (s *Scheduler) stepGroupSLA(now time.Time) error {
	groups, err := s.Store.ListGroups()
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.SlaTimeoutSeconds <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(g.SlaTimeoutSeconds) * time.Second)
		due, err := s.Store.ListOpenTasksSLADue(g.ID, cutoff)
		if err != nil {
			slog.Warn("group sla: list failed", "group", g.ID, "err", err)
			continue
		}
		for _, t := range due {
			name := fmt.Sprintf("Employee #%d", t.EmpID)
			if emp, eerr := s.Store.GetEmployee(t.EmpID); eerr == nil {
				name = emp.Name
			}
			created, ok := parseTaskTime(t.CreatedAt)
			if !ok {
				continue
			}
			body := fmt.Sprintf("⏰ Task #%d (%s) has been open for %s, exceeding this group's SLA threshold (%s)",
				t.ID, name, humanDuration(now.Sub(created)),
				humanDuration(time.Duration(g.SlaTimeoutSeconds)*time.Second))
			if _, err := s.Store.CreateGroupMessage(g.ID, store.MsgSystem, body, nil, nil, nil); err != nil {
				slog.Warn("group sla: send failed", "group", g.ID, "task", t.ID, "err", err)
				continue
			}
			if err := s.Store.MarkTaskSLAReminded(t.ID, now.UTC().Format(time.RFC3339)); err != nil {
				slog.Warn("group sla: mark failed", "group", g.ID, "task", t.ID, "err", err)
			}
		}
	}
	return nil
}

// stepStorageCleanup (disk management) enforces the artifact retention TTL
// and sweeps orphan artifact files, at most once per hour (lastCleanup
// throttle; zero value → run on the first tick).
// storage_retention_days=0 disables the TTL deletion only; the orphan sweep
// still runs. Housekeeping errors are logged, never fatal.
func (s *Scheduler) stepStorageCleanup(now time.Time) error {
	if !s.lastCleanup.IsZero() && now.Sub(s.lastCleanup) < time.Hour {
		return nil
	}
	s.lastCleanup = now

	days := defaultRetentionDays
	if v, err := s.Store.GetSetting("storage_retention_days"); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			days = n
		}
	}
	expired := 0
	if days > 0 {
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
		var err error
		expired, err = s.Store.DeleteExpiredArtifacts(cutoff)
		if err != nil {
			slog.Warn("storage cleanup: delete expired artifacts", "err", err)
		}
	}
	orphan := 0
	if o, err := s.Store.CleanupOrphanArtifacts(now.Add(-time.Hour)); err != nil {
		slog.Warn("storage cleanup: orphan sweep", "err", err)
	} else {
		orphan = o
	}
	if expired > 0 || orphan > 0 {
		slog.Info("storage cleanup", "expired_artifacts", expired, "orphan_files", orphan)
	}
	return nil
}

// humanDuration renders a duration in the smallest meaningful unit so the
// SLA reminder body stays coherent for sub-minute thresholds (e.g. a 1s
// SLA reads "1 second" instead of "0 minutes" against "0 minutes").
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d sec", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}

// parseTaskTime parses a task created_at timestamp. The schema default is
// datetime('now') ("2006-01-02 15:04:05", UTC); RFC3339 is accepted as well.
func parseTaskTime(ts string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, perr := time.Parse(layout, ts); perr == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (s *Scheduler) hasPendingSelfReview(empID int) bool {
	var n int
	err := s.Store.DB().
		QueryRow(`SELECT COUNT(*) FROM tasks WHERE employee_id = ? AND kind = 'selfreview' AND status IN ('queued','running')`, empID).
		Scan(&n)
	return err == nil && n > 0
}
