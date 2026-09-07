package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/identity"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// refuse drives the real limiter until the subject is over its limit, once per window.
//
// It goes through Allow rather than inserting rows, because the rows are the thing under
// test: a fixture written from the same understanding as the query would agree with it
// whatever either one meant by "over the limit". Here the row exists only because a
// request was actually refused.
func refuse(t *testing.T, limits *identity.Limits, subject string, windows int, window time.Duration) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < windows; i++ {
		if i > 0 {
			// Past the truncation boundary, so the next pair lands in its own window.
			time.Sleep(window + window/10)
		}
		if _, err := limits.Allow(ctx, "turn", subject, 1, window); err != nil {
			t.Fatalf("the first request in window %d was refused: %v", i, err)
		}
		if _, err := limits.Allow(ctx, "turn", subject, 1, window); err == nil {
			t.Fatalf("the second request in window %d was allowed past a limit of 1", i)
		}
	}
}

// A hundred customers refused once and one client refused a hundred times are the same
// number of 429s. This is the difference, and it is read out of the counting the limiter
// already does rather than out of anything new: no table, no write on the hot path.
func TestARepeatOffenderIsVisibleInTheCountingTheLimiterAlreadyDoes(t *testing.T) {
	ctx := context.Background()
	limits := identity.NewLimits(pool)
	window := 200 * time.Millisecond

	// Short windows, so three of them take under a second. The query does not know what
	// size a window is -- it counts rows -- and in production they are minutes.
	persistent := "subject-refused-in-three-windows-" + t.Name()
	once := "subject-refused-in-one-window-" + t.Name()
	refuse(t, limits, persistent, 3, window)
	refuse(t, limits, once, 1, window)

	offenders, err := limits.RepeatOffenders(ctx, "turn", 1, time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]identity.Offender{}
	for _, o := range offenders {
		found[o.Subject] = o
	}
	got, ok := found[persistent]
	if !ok {
		t.Fatalf("a subject refused in three separate windows is not a repeat offender: %v",
			offenders)
	}
	if got.Windows != 3 {
		t.Errorf("the offender was over its limit in %d windows, and it was refused in 3",
			got.Windows)
	}
	// Two requests per window, the second of which was the refusal.
	if got.Requests != 6 {
		t.Errorf("the offender made %d requests; six were counted", got.Requests)
	}
	// The half that makes the signal worth having. One burst is a per-minute limit doing
	// exactly its job, and reporting it as abuse is how a signal becomes noise.
	if _, ok := found[once]; ok {
		t.Errorf("a subject refused in a single window was reported as a repeat offender: %v",
			found[once])
	}

	// The lookback is applied at all: the same rows, asked about the last ten
	// milliseconds, are nobody's. Without this the hour above would pass just as well
	// against a query that ignored its interval and counted the table for ever.
	time.Sleep(20 * time.Millisecond)
	recent, err := limits.RepeatOffenders(ctx, "turn", 1, 10*time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range recent {
		if o.Subject == persistent {
			t.Errorf("windows from half a second ago counted inside a ten-millisecond "+
				"lookback (%d of them); the interval is not being applied", o.Windows)
		}
	}
}

// The gauge is what an alert can reach, and a gauge nothing updates reads as zero -- which
// is the all-clear. So the sampler is driven rather than the query.
func TestTheGaugeReportsWhatTheSamplerFound(t *testing.T) {
	ctx := context.Background()
	limits := identity.NewLimits(pool)
	limits.TurnsPerMinute = 1
	metrics := obs.NewMetrics()
	watch := identity.NewAbuseWatch(limits, metrics)

	if got := testutil.ToFloat64(metrics.Offenders); got != 0 {
		t.Fatalf("chat_rate_limited_subjects started at %v", got)
	}
	refuse(t, limits, "subject-for-the-gauge-"+t.Name(), 3, 200*time.Millisecond)

	offenders, err := watch.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) == 0 {
		t.Fatal("the sampler found no repeat offender after one was made")
	}
	if got := testutil.ToFloat64(metrics.Offenders); got != float64(len(offenders)) {
		t.Errorf("the gauge reads %v and the sampler found %d subjects", got, len(offenders))
	}
}

// A limit of zero is not "no offenders", it is "nothing was ever refused". Answering it
// with a confident zero is the shape of a detector that cannot fail: the caller checks
// this before starting the watch, and this is the belt to that brace.
func TestWithNoLimitThereIsNothingToBeOverAndNothingIsClaimed(t *testing.T) {
	limits := identity.NewLimits(pool)
	offenders, err := limits.RepeatOffenders(context.Background(), "turn", 0, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Errorf("with no limit configured the query returned %d offenders", len(offenders))
	}
}
