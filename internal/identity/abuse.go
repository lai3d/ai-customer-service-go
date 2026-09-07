package identity

import (
	"context"
	"log/slog"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/obs"
)

// The abuse signal, built entirely out of counting that already happens.
//
// The rate limiter writes one row per (bucket, subject, window) and reads it back to
// decide whether to refuse. That table already knows which subject was over its limit and
// in how many separate minutes -- the question "is this one client, again" is a GROUP BY
// over rows that exist, not a new thing to record. Anything that needed its own table, its
// own write on the hot path, or a second store would not be worth its cost at this size,
// and this is the version that is.
//
// What it deliberately does not do is score, ban, or lock anyone out. A repeat offender
// here is a client that keeps being refused, which is a scraper, a loop, a stuck retry, or
// a limit set below what the product legitimately does -- four things that need a person to
// tell apart, not an automatic response.
const (
	// abuseLookback is how far back a repeat offender is looked for. An hour is long
	// enough that a client retrying every few minutes is visible and short enough that
	// yesterday's incident is not still on the dashboard.
	abuseLookback = time.Hour
	// abuseMinWindows is how many separate windows over the limit make a repeat offender.
	// One is a burst -- exactly what a per-minute limit is for, working as designed.
	// Three separate minutes is a client that did not back off after being told to.
	abuseMinWindows = 3
	// abuseInterval is how often the count is refreshed. It is one indexed aggregate over
	// an hour of a small table, per replica.
	abuseInterval = time.Minute
	// abuseLogged bounds how many subject ids one log line carries. The count is the
	// signal; the ids are for whoever goes looking.
	abuseLogged = 5
)

// Offender is one subject that went over its limit in several separate windows.
//
// The subject id is here, and it reaches a log line rather than a metric label: subjects
// are unbounded, and an unbounded label value is how a metrics backend goes down. It is an
// opaque server-issued id either way -- it identifies a session, not a person, and this
// service has no idea who that is.
type Offender struct {
	Subject  string
	Windows  int
	Requests int64
}

// RepeatOffenders reads the rate limiter's own rows for subjects that were over `limit` in
// at least minWindows separate windows inside the lookback.
//
// `count > limit` is the same comparison Allow makes, deliberately: a row is over the
// limit here exactly when the request that wrote it was refused. Reconstructing the
// threshold differently -- >= limit, say -- would count subjects that were never refused
// at all and quietly disagree with the 429s the customer actually got.
func (l *Limits) RepeatOffenders(ctx context.Context, bucket string, limit int,
	lookback time.Duration, minWindows int) ([]Offender, error) {

	if limit <= 0 {
		// No limit means nothing was ever refused, so there are no offenders. Returning
		// an empty list rather than querying keeps the caller from reporting a confident
		// zero that was never measured -- the caller checks the limit before starting.
		return nil, nil
	}
	rows, err := l.pool.Query(ctx, `
		SELECT subject, count(*)::int, coalesce(sum(count), 0)::bigint
		FROM rate_window
		WHERE bucket = $1
		  AND window_start > now() - $2::interval
		  AND count > $3
		GROUP BY subject
		HAVING count(*) >= $4
		ORDER BY count(*) DESC, sum(count) DESC
		LIMIT 100`, bucket, lookback.String(), limit, minWindows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Offender
	for rows.Next() {
		var o Offender
		if err := rows.Scan(&o.Subject, &o.Windows, &o.Requests); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AbuseWatch keeps chat_rate_limited_subjects up to date.
//
// A per-minute limit on its own cannot answer the question an operator has when the 429s
// start: a hundred customers refused once and one client refused a hundred times produce
// the same number of refusals, and only one of them is abuse. This is that difference,
// sampled.
type AbuseWatch struct {
	limits  *Limits
	metrics *obs.Metrics
	bucket  string
}

// NewAbuseWatch watches the `turn` bucket, which is the per-subject one. The session
// bucket is keyed by IP rather than by subject and bounds a different thing -- how fast
// new subjects can be minted -- so it is not the same question and is not counted here.
func NewAbuseWatch(limits *Limits, metrics *obs.Metrics) *AbuseWatch {
	return &AbuseWatch{limits: limits, metrics: metrics, bucket: "turn"}
}

// Sample refreshes the gauge once and returns what it found.
//
// Separate from Run so the measurement can be driven from a test against a real database
// rather than a ticker being waited on.
func (w *AbuseWatch) Sample(ctx context.Context) ([]Offender, error) {
	offenders, err := w.limits.RepeatOffenders(ctx, w.bucket,
		w.limits.TurnsPerMinute, abuseLookback, abuseMinWindows)
	if err != nil {
		// The gauge is left at its last reading rather than set to zero. Zero is a
		// measurement -- "nobody is being throttled" -- and publishing it because a query
		// failed is the one wrong answer available here: it is the reading that says stop
		// looking.
		return nil, err
	}
	w.metrics.Offenders.Set(float64(len(offenders)))
	if len(offenders) > 0 {
		// The ids are here and nowhere else. Warn rather than Info because the count is
		// on a dashboard and this line is what makes it actionable.
		ids := make([]string, 0, abuseLogged)
		for _, o := range offenders {
			if len(ids) == abuseLogged {
				break
			}
			ids = append(ids, o.Subject)
		}
		slog.Warn("subjects are repeatedly hitting the per-minute turn limit",
			"subjects", len(offenders), "worst", ids,
			"windows_over_limit", offenders[0].Windows, "lookback", abuseLookback)
	}
	return offenders, nil
}

// Run samples until the context is done.
func (w *AbuseWatch) Run(ctx context.Context) {
	ticker := time.NewTicker(abuseInterval)
	defer ticker.Stop()
	for {
		if _, err := w.Sample(ctx); err != nil && ctx.Err() == nil {
			slog.Error("could not count repeatedly rate-limited subjects; "+
				"chat_rate_limited_subjects is now stale rather than wrong", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
