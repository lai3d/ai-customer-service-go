package testsupport

import (
	"context"
	"sync"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/ticket"
)

// FakeTickets is a ticket.Creator for tests that are about something else.
//
// It exists so the model-facing tool, the chat turn and the benchmark do not need a
// database to exercise paths that have nothing to do with storage. It deliberately does
// **not** reimplement the cap or the deduplication: those are guarantees of the schema
// now -- a unique index and a transaction -- and a fake that reimplemented them would be
// a second implementation to keep in step, and would let a test pass against behaviour
// no deployment has. They are tested in internal/ticket against a real Postgres.
type FakeTickets struct {
	mu      sync.Mutex
	n       int
	Created []ticket.CreateRequest
	Outcome ticket.Outcome // what to return; defaults to created
	Err     error
}

func (f *FakeTickets) Create(_ context.Context, req ticket.CreateRequest) (ticket.Ticket, ticket.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return ticket.Ticket{}, "", f.Err
	}
	f.Created = append(f.Created, req)
	outcome := f.Outcome
	if outcome == "" {
		outcome = ticket.OutcomeCreated
	}
	f.n++
	return ticket.Ticket{
		Number:         "TKT-TEST-" + string(rune('0'+f.n%10)),
		ConversationID: req.ConversationID,
		Category:       ticket.NormaliseCategory(req.Category),
		Summary:        req.Summary,
		OrderNumber:    req.OrderNumber,
		State:          ticket.StateOpen,
		Version:        1,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}, outcome, nil
}

func (f *FakeTickets) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Created)
}
