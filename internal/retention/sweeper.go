package retention

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper runs Sweep on a schedule.
//
// It runs once at start-up rather than waiting a full interval, because the interval is
// an hour by default and a service that restarts often would otherwise never sweep at
// all -- the failure mode where a scheduled job exists, is configured, and has never
// executed.
type Sweeper struct {
	store    *Store
	window   time.Duration
	interval time.Duration
}

func NewSweeper(store *Store, window, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Sweeper{store: store, window: window, interval: interval}
}

// Run blocks until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	if s.window <= 0 {
		return
	}
	s.once(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.once(ctx)
		}
	}
}

func (s *Sweeper) once(ctx context.Context) {
	report, err := s.store.Sweep(ctx, s.window)
	if err != nil {
		// Logged rather than fatal: a sweep that cannot run is a service holding data
		// longer than it promised, which is worth an alert and is not worth refusing to
		// answer customers over.
		slog.Error("retention sweep failed", "error", err, "window", s.window)
		return
	}
	if !report.Empty() {
		// Only when something happened. An hourly line saying "deleted nothing" is how a
		// log stops being read.
		slog.Info("retention sweep", "window", s.window, "deleted", report.String())
	}
}
