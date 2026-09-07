package tenant

import (
	"sync"
	"time"
)

// lastUsed decides whether a key's last_used_at is worth writing again.
//
// It is a bounded map, on the same rule as the ticket table and the budget: a map keyed by
// something a client supplies and cleared by nothing is a memory leak with a long fuse.
// Here the key is a key_id, which is bounded by how many keys exist — but the number of
// keys is a thing an operator can grow, and "bounded by an operator's behaviour" is not
// bounded.
//
// The eviction is deliberately crude: at capacity the whole map is dropped. What that
// costs is one extra UPDATE per key afterwards, which is the cheapest possible consequence
// for the simplest possible policy — the alternative is an LRU whose ordering is
// maintained on the hot path of every request to decide whether to skip a write.
type lastUsed struct {
	mu    sync.Mutex
	at    map[string]time.Time
	every time.Duration
	max   int
}

func newLastUsed() *lastUsed {
	return &lastUsed{at: map[string]time.Time{}, every: time.Minute, max: 4096}
}

func (l *lastUsed) due(keyID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.at[keyID]; ok && now.Sub(last) < l.every {
		return false
	}
	if len(l.at) >= l.max {
		l.at = map[string]time.Time{}
	}
	l.at[keyID] = now
	return true
}
