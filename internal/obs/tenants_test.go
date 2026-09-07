package obs_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The cap is the whole point: a tenant is nearly bounded, and nearly bounded is the shape
// that is fine for a year and then is not.
func TestTheTenantLabelIsCappedAndSaysSoRatherThanTruncating(t *testing.T) {
	labels := obs.NewTenantLabels(3)

	for _, id := range []string{"a", "b", "c"} {
		if got := labels.Label(id); got != id {
			t.Errorf("tenant %q reported as %q while under the cap", id, got)
		}
	}
	// Already-known tenants keep their own label after the cap is reached.
	if got := labels.Label("a"); got != "a" {
		t.Errorf("a known tenant became %q once the cap was reached", got)
	}
	for _, id := range []string{"d", "e", "f"} {
		if got := labels.Label(id); got != obs.OtherTenants {
			t.Errorf("tenant %q past the cap reported as %q, want %q",
				id, got, obs.OtherTenants)
		}
	}
	if labels.Known() != 3 {
		t.Errorf("%d tenants have their own label, want 3", labels.Known())
	}
}

// A metric arriving with no tenant is a code path that lost it. `unknown` makes that
// visible instead of attributing it to somebody.
func TestATenantlessMetricIsUnknownRatherThanSomebodyElse(t *testing.T) {
	if got := obs.NewTenantLabels(10).Label(""); got != "unknown" {
		t.Errorf("an empty tenant reported as %q", got)
	}
}

// The cap holds under concurrent first-sightings.
//
// **This does not test the re-check under the write lock, and saying so is the point.**
// Removing that re-check -- the one line that stops two goroutines both getting past the
// read and both inserting -- leaves this test green at 200 goroutines and at 5,000, run
// several times. The window between RUnlock and Lock is a couple of instructions wide and
// this machine does not lose that race.
//
// So the re-check is argued rather than evidenced, on the same terms as
// `hnsw.iterative_scan` in internal/store: the reasoning is that a read-then-write without
// a re-check is unbounded by construction, the cost is one map lookup on the path of a
// tenant's first turn only, and the failure it prevents is an intentionally bounded label
// quietly becoming unbounded. What this test does prove is that the cap holds under real
// concurrency, which is worth pinning and is not the same claim.
func TestTheCapHoldsWhenManyNewTenantsArriveAtOnce(t *testing.T) {
	const cap, tenants = 10, 5000
	labels := obs.NewTenantLabels(cap)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			labels.Label(fmt.Sprintf("tenant-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	if labels.Known() > cap {
		t.Errorf("%d tenants have their own label with a cap of %d", labels.Known(), cap)
	}
}

// And the label reaches the metric, bounded, rather than being computed and discarded.
func TestTheCostMeterCarriesABoundedTenantLabel(t *testing.T) {
	m := obs.NewMetrics().WithTenantLabels(2)
	for i := range 5 {
		m.RecordUsage(fmt.Sprintf("tenant-%d", i), "claude-opus-5", 100, 20, 0.01, true)
	}
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var series int
	var other bool
	for _, f := range families {
		if f.GetName() != "chat_cost_usd_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			series++
			for _, l := range metric.GetLabel() {
				if l.GetName() == "tenant" && l.GetValue() == obs.OtherTenants {
					other = true
				}
			}
		}
	}
	// Two named tenants plus `other`, from five.
	if series != 3 {
		t.Errorf("chat_cost_usd_total has %d series from 5 tenants with a cap of 2", series)
	}
	if !other {
		t.Error("no series is labelled `other`; the tenants past the cap vanished instead " +
			"of being aggregated, which is a silent truncation")
	}
	// The aggregate carries the money of all three tenants past the cap, so a bill does
	// not go missing at the cap.
	spend := testutil.ToFloat64(m.CostUSD.WithLabelValues("claude-opus-5", obs.OtherTenants))
	if spend < 0.029 || spend > 0.031 {
		t.Errorf("the `other` series holds $%v, want the three capped tenants' $0.03", spend)
	}
}
