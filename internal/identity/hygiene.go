package identity

import (
	"context"
	"log/slog"
	"time"
)

// Hygiene deletes the rows this package writes and never reads again: closed rate windows
// and expired sessions.
//
// It exists because `SweepWindows` and `Sessions.Sweep` were written, tested, and called by
// nothing. Both were correct; `rate_window` still grew for ever, because a sweeper that
// nobody starts is a comment. Another agent reading this code found that, which is a
// reminder that "the function exists" and "the work happens" are different claims and only
// one of them was true.
//
// Retention (internal/retention) is a different thing and stays separate: that is about
// customer data and a promise to a customer, and it is off by default. This is operational
// hygiene, it involves no customer text, and it always runs.
type Hygiene struct {
	sessions *Sessions
	limits   *Limits
	interval time.Duration
	// grace is how long past expiry a session row survives. Not zero: a session that
	// expires mid-request should be *refused* by the read-side expiry check rather than
	// found missing, and the two read differently in a log.
	grace time.Duration
}

func NewHygiene(sessions *Sessions, limits *Limits, interval time.Duration) *Hygiene {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Hygiene{sessions: sessions, limits: limits, interval: interval, grace: time.Hour}
}

// Run blocks until the context is cancelled. It sweeps once at start-up rather than
// waiting a full interval, because a service that restarts often would otherwise never
// sweep at all -- the same failure as a scheduled job that has never executed.
func (h *Hygiene) Run(ctx context.Context) {
	h.once(ctx)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.once(ctx)
		}
	}
}

// Once sweeps both tables and reports what went. Exported so a test can drive it without
// a ticker, and so the thing being tested is the thing that runs.
func (h *Hygiene) Once(ctx context.Context) (sessions, windows int64, err error) {
	if h.sessions != nil {
		sessions, err = h.sessions.Sweep(ctx, h.grace)
		if err != nil {
			return 0, 0, err
		}
	}
	if h.limits != nil {
		// Windows are kept a little past their close so a request arriving at a boundary
		// still counts against the window it belongs to.
		windows, err = h.limits.SweepWindows(ctx, 2*time.Hour)
		if err != nil {
			return sessions, 0, err
		}
	}
	return sessions, windows, nil
}

func (h *Hygiene) once(ctx context.Context) {
	sessions, windows, err := h.Once(ctx)
	if err != nil {
		// Logged rather than fatal: unbounded growth is a slow problem and refusing to
		// answer customers over it would be a fast one.
		slog.Error("could not sweep expired sessions and rate windows", "error", err)
		return
	}
	if sessions > 0 || windows > 0 {
		slog.Info("swept", "sessions", sessions, "rate_windows", windows)
	}
}
