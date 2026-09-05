package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits are the two ceilings that stop a stranger spending the model budget: how often
// one subject may ask, and how much the whole service may spend in a day.
//
// Both live in Postgres, and that is the design rather than an implementation detail. A
// counter in a map is `replicas x limit` -- the same arithmetic that made the ticket cap
// wrong for as long as it lived in process memory, and the same arithmetic an attacker
// benefits from. The per-conversation token budget that already exists is not a
// substitute: conversation ids are free, so anyone who wants more budget starts another
// conversation.
type Limits struct {
	pool *pgxpool.Pool

	// TurnsPerMinute is per subject. Zero disables the check.
	TurnsPerMinute int
	// SessionsPerHourPerIP bounds session creation, which is the one endpoint reachable
	// without a session and therefore the one that mints new subjects. Zero disables it.
	SessionsPerHourPerIP int
	// DailyTokenBudget is the whole service, all subjects, per UTC day. Zero disables it.
	DailyTokenBudget int64
}

func NewLimits(pool *pgxpool.Pool) *Limits { return &Limits{pool: pool} }

var (
	// ErrTooManyRequests is the subject asking too often. Retryable, and the caller says
	// when.
	ErrTooManyRequests = errors.New("too many requests")
	// ErrBudgetExhausted is the service's own daily ceiling. Not the customer's fault and
	// not fixed by waiting a minute, so it is reported differently.
	ErrBudgetExhausted = errors.New("the service has reached its daily budget")
)

// Allow records one request against a fixed window and reports whether it fits.
//
// A fixed window, not a sliding one, and the edge behaviour is real: a subject can spend
// the whole allowance at 10:59:59 and the whole allowance again at 11:00:00. That is
// twice the rate for one second, and it is an acceptable trade for a counter that is one
// statement and cannot drift between replicas. A sliding window needs either a sorted set
// per subject or a second table, and neither is worth it until something says this bound
// is the one being hit.
func (l *Limits) Allow(ctx context.Context, bucket, key string, limit int, window time.Duration) (retryAfter time.Duration, err error) {
	if limit <= 0 {
		return 0, nil
	}
	start := time.Now().UTC().Truncate(window)
	var count int
	// The insert is the increment. Doing it as a read and then a write is a race that
	// resolves in favour of whoever is hammering the endpoint.
	err = l.pool.QueryRow(ctx, `
		INSERT INTO rate_window (bucket, subject, window_start, count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (bucket, subject, window_start)
		DO UPDATE SET count = rate_window.count + 1
		RETURNING count`, bucket, key, start).Scan(&count)
	if err != nil {
		return 0, err
	}
	if count > limit {
		return time.Until(start.Add(window)), ErrTooManyRequests
	}
	return 0, nil
}

// SweepWindows removes windows that closed more than grace ago. Nothing reads them and
// they are the only unbounded growth this table has.
func (l *Limits) SweepWindows(ctx context.Context, grace time.Duration) (int64, error) {
	tag, err := l.pool.Exec(ctx,
		`DELETE FROM rate_window WHERE window_start < now() - $1::interval`, grace.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CheckDailyBudget reports whether the service may start another turn today.
//
// It is checked before the turn and recorded after, so the ceiling is crossed by at most
// the turns already in flight rather than enforced to the token. Charging first would
// need a reservation and a refund, and the refund is the part that goes wrong: a process
// that dies between reserving and refunding leaves the budget permanently smaller.
func (l *Limits) CheckDailyBudget(ctx context.Context) error {
	if l.DailyTokenBudget <= 0 {
		return nil
	}
	// A missing row is zero spend, so the aggregate is used rather than a lookup: it
	// returns one row either way and there is no "no rows" case to get wrong.
	used, err := l.UsedToday(ctx)
	if err != nil {
		return err
	}
	if used >= l.DailyTokenBudget {
		return fmt.Errorf("%w: %d of %d tokens", ErrBudgetExhausted, used, l.DailyTokenBudget)
	}
	return nil
}

// RecordSpend adds a turn's tokens to today's total. Called after the turn on a detached
// context: a customer who closed the tab still spent the money.
func (l *Limits) RecordSpend(ctx context.Context, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	_, err := l.pool.Exec(ctx, `
		INSERT INTO daily_spend (day, tokens)
		VALUES ((now() AT TIME ZONE 'UTC')::date, $1)
		ON CONFLICT (day) DO UPDATE SET tokens = daily_spend.tokens + $1`, tokens)
	return err
}

// UsedToday is what the operations overview reads.
func (l *Limits) UsedToday(ctx context.Context) (int64, error) {
	var used int64
	err := l.pool.QueryRow(ctx,
		`SELECT coalesce(sum(tokens), 0) FROM daily_spend WHERE day = (now() AT TIME ZONE 'UTC')::date`).
		Scan(&used)
	return used, err
}
