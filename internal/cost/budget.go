// Package cost bounds what a conversation can spend and meters what it did spend.
package cost

import (
	"container/list"
	"fmt"
	"sync"
)

// Budget caps the tokens one conversation may spend.
//
// A message window bounds any single request; nothing bounds the number of requests. A
// customer who keeps typing, or a script that does, runs indefinitely, and the failure
// is undramatic: no error, no alert, a larger invoice. Reaching the cap is also a good
// reason to hand the customer to a human -- a conversation that long is not going well.
//
// Spend is held in a bounded LRU map, per replica, reset on restart. That is honest
// about what it is: blast-radius limiting, not a ledger. Redis or Postgres would be the
// real thing. The bound matters more than it looks -- an unbounded map keyed by
// conversation id is a memory leak with a long fuse.
type Budget struct {
	mu     sync.Mutex
	spend  map[string]*entry
	order  *list.List
	limit  int64
	maxIDs int
}

type entry struct {
	id      string
	tokens  int64
	element *list.Element
}

// ErrExceeded is returned when a conversation has spent its budget. It is a distinct
// error because the right response is a 429 pointing at a human, not a 500.
type ErrExceeded struct {
	ConversationID string
	Spent          int64
	Limit          int64
}

func (e *ErrExceeded) Error() string {
	return fmt.Sprintf("conversation has spent %d tokens against a budget of %d", e.Spent, e.Limit)
}

// NewBudget returns a budget. A limit of 0 disables the cap but still tracks spend.
func NewBudget(limit int64, maxTracked int) *Budget {
	if maxTracked <= 0 {
		maxTracked = 10_000
	}
	return &Budget{
		spend:  make(map[string]*entry),
		order:  list.New(),
		limit:  limit,
		maxIDs: maxTracked,
	}
}

// Check reports whether a conversation may make another request.
func (b *Budget) Check(conversationID string) error {
	if b.limit <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.spend[conversationID]
	if !ok {
		return nil
	}
	b.order.MoveToFront(e.element)
	if e.tokens >= b.limit {
		return &ErrExceeded{ConversationID: conversationID, Spent: e.tokens, Limit: b.limit}
	}
	return nil
}

// Record adds a turn's tokens to a conversation's running total.
func (b *Budget) Record(conversationID string, tokens int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.spend[conversationID]
	if !ok {
		for len(b.spend) >= b.maxIDs {
			oldest := b.order.Back()
			if oldest == nil {
				break
			}
			b.order.Remove(oldest)
			delete(b.spend, oldest.Value.(string))
		}
		e = &entry{id: conversationID}
		e.element = b.order.PushFront(conversationID)
		b.spend[conversationID] = e
	} else {
		b.order.MoveToFront(e.element)
	}
	e.tokens += tokens
	return e.tokens
}

func (b *Budget) Spent(conversationID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.spend[conversationID]; ok {
		return e.tokens
	}
	return 0
}

func (b *Budget) Limit() int64 { return b.limit }

// Price is dollars per million tokens. Keep it in step with the provider's published
// pricing; nothing here can detect that it has drifted, and a model with no entry has
// its tokens counted but not costed.
type Price struct {
	InputPerMillionUSD  float64
	OutputPerMillionUSD float64
}

// Prices are keyed on the model id the provider *reports*, not the one requested.
// Asking for "gpt-5" yields "gpt-5-2025-08-07", and a price keyed on "gpt-5" silently
// never matches: tokens keep counting while cost stays at zero.
var Prices = map[string]Price{
	"claude-opus-5":    {InputPerMillionUSD: 5.00, OutputPerMillionUSD: 25.00},
	"claude-sonnet-5":  {InputPerMillionUSD: 2.00, OutputPerMillionUSD: 10.00},
	"claude-haiku-4-5": {InputPerMillionUSD: 1.00, OutputPerMillionUSD: 5.00},
}

// USD returns the cost of a call, and whether the model had a price at all.
func USD(model string, inputTokens, outputTokens int64) (float64, bool) {
	price, ok := Prices[model]
	if !ok {
		return 0, false
	}
	return float64(inputTokens)/1e6*price.InputPerMillionUSD +
		float64(outputTokens)/1e6*price.OutputPerMillionUSD, true
}
