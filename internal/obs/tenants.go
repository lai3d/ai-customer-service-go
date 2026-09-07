package obs

import "sync"

// TenantLabels bounds the `tenant` label.
//
// Every other rule about labels in this service is "do not put an unbounded value in one":
// no conversation id, no subject, no key id, no tool name the model invented. A tenant is
// *nearly* bounded -- it is operator-chosen and there are as many as there are customers --
// and "nearly bounded" is the shape that is fine for a year and then is not. A metrics
// backend does not fail when the cardinality gets too high; it gets slow, then expensive,
// then it drops things, and the first symptom is a dashboard that used to work.
//
// So the label is capped. Past the cap every further tenant reports as `other`, which is
// deliberately visible: a rising `other` is a signal that this cap is now the thing
// deciding what you can see, rather than a silent truncation.
//
// The first N are kept rather than the busiest N. That is an argument from what this is
// for: a deployment with a handful of tenants gets all of them, and one with hundreds gets
// an arbitrary subset plus an aggregate -- which is the honest answer, because picking the
// "important" ones would need a definition of important that this package does not have.
type TenantLabels struct {
	mu   sync.RWMutex
	seen map[string]struct{}
	max  int
}

// OtherTenants is the label value every tenant past the cap reports as.
const OtherTenants = "other"

// DefaultMaxTenantLabels is the cap when nothing configures one. Twenty is a guess and is
// labelled as one: it is small enough that the series count stays in the hundreds across
// every metric that carries the label, and large enough that a deployment which is
// multi-tenant on paper and single-tenant in practice never meets it.
const DefaultMaxTenantLabels = 20

func NewTenantLabels(max int) *TenantLabels {
	if max <= 0 {
		max = DefaultMaxTenantLabels
	}
	return &TenantLabels{seen: make(map[string]struct{}, max), max: max}
}

// Label returns the value to use for a tenant.
func (t *TenantLabels) Label(tenantID string) string {
	if tenantID == "" {
		// Not a tenant, and not silently attributed to one either. A metric that arrives
		// with no tenant is a code path that lost it, and `unknown` is how that becomes
		// visible instead of becoming somebody else's number.
		return "unknown"
	}

	t.mu.RLock()
	_, known := t.seen[tenantID]
	full := len(t.seen) >= t.max
	t.mu.RUnlock()
	if known {
		return tenantID
	}
	if full {
		return OtherTenants
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// Re-checked under the write lock: two turns for a new tenant can both get past the
	// read above, and without this the map can exceed max by however many were racing.
	if _, known := t.seen[tenantID]; known {
		return tenantID
	}
	if len(t.seen) >= t.max {
		return OtherTenants
	}
	t.seen[tenantID] = struct{}{}
	return tenantID
}

// Known is how many distinct tenants have a label of their own. For tests and for the
// start-up log line that says the cap exists.
func (t *TenantLabels) Known() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.seen)
}
