package feedback_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/feedback"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, stop, err := testsupport.StartPostgres(context.Background(), 384)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

func turn(t *testing.T, id, question, reply string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO turn (id, conversation_id, started_at, ended_at, outcome, question, reply,
		                  model, model_calls, input_tokens, output_tokens)
		VALUES ($1, $1||'-conv', now(), now(), 'completed', $2, $3, 'm', 1, 10, 5)`,
		id, question, reply); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO turn_passage (turn_id, rank, entry_id, language, score, question)
		VALUES ($1, 1, 'returns-window', 'en', 0.9, 'how long to return')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The two sources are recorded separately and never averaged: a customer knows whether they
// were helped and nothing about correctness, an operator knows the opposite, and one number
// would lose the distinction that decides what to do.
func TestACustomerAndAnOperatorAreBothHeardAboutTheSameTurn(t *testing.T) {
	ctx := context.Background()
	s := feedback.NewStore(pool)
	id := turn(t, "both-"+stamp(), "how long do I have to return?", "you have 14 days")

	if err := s.Record(ctx, id, feedback.SourceCustomer, feedback.VerdictUnclear,
		"I still don't know the deadline", "session-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, id, feedback.SourceOperator, feedback.VerdictWrong,
		"the window is 30 days, not 14", "alex"); err != nil {
		t.Fatal(err)
	}

	items, err := s.Queue(ctx, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	var sources []feedback.Source
	for _, i := range items {
		if i.TurnID == id {
			sources = append(sources, i.Source)
		}
	}
	if len(sources) != 2 {
		t.Fatalf("the queue has %d entries for one turn, want both sources: %v", len(sources), sources)
	}

	// A verdict change replaces that source's own and leaves the other alone.
	if err := s.Record(ctx, id, feedback.SourceCustomer, feedback.VerdictHelpful,
		"", "session-abc"); err != nil {
		t.Fatal(err)
	}
	items, err = s.Queue(ctx, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	sources = nil
	for _, i := range items {
		if i.TurnID == id {
			sources = append(sources, i.Source)
		}
	}
	if len(sources) != 1 || sources[0] != feedback.SourceOperator {
		t.Errorf("after the customer changed their mind the queue holds %v", sources)
	}
}

// The queue exists so somebody can act without going and looking things up. An item that
// says only "this was wrong" is a complaint; one that carries the question, the answer and
// what was retrieved is a piece of work.
func TestAQueuedItemCarriesWhatIsNeededToActOnIt(t *testing.T) {
	ctx := context.Background()
	s := feedback.NewStore(pool)
	id := turn(t, "rich-"+stamp(), "when does my refund arrive?", "immediately")

	if err := s.Record(ctx, id, feedback.SourceOperator, feedback.VerdictWrong,
		"refunds take three business days", "alex"); err != nil {
		t.Fatal(err)
	}
	items, err := s.Queue(ctx, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	var item *feedback.Item
	for i := range items {
		if items[i].TurnID == id {
			item = &items[i]
		}
	}
	if item == nil {
		t.Fatal("the item is not in the queue")
	}
	if item.Question == "" || item.Answer == "" {
		t.Error("the item does not carry the question and the answer")
	}
	if len(item.Entries) == 0 {
		t.Error("the item does not say what was retrieved, which is where a wrong answer usually starts")
	}
	if item.Note == "" {
		t.Error("the note was lost")
	}
}

// Collecting feedback is only worth doing if something clears it. A queue nobody empties is
// a suggestion box.
func TestHandlingRemovesAnItemAndAChangedMindPutsItBack(t *testing.T) {
	ctx := context.Background()
	s := feedback.NewStore(pool)
	id := turn(t, "handle-"+stamp(), "do you ship abroad?", "no")

	if err := s.Record(ctx, id, feedback.SourceOperator, feedback.VerdictWrong,
		"we ship to 34 countries", "alex"); err != nil {
		t.Fatal(err)
	}
	if err := s.Handle(ctx, id, feedback.SourceOperator, "alex"); err != nil {
		t.Fatal(err)
	}
	if inQueue(t, s, id) {
		t.Error("a handled item is still in the queue")
	}
	// Handling it twice is not an error worth failing a request over, but it must not
	// silently claim to have done something.
	if err := s.Handle(ctx, id, feedback.SourceOperator, "alex"); !errors.Is(err, feedback.ErrNoSuchTurn) {
		t.Errorf("handling an already-handled item returned %v", err)
	}
	// It is still visible when asked for, because "what did we decide about this" is a
	// question somebody asks later.
	items, err := s.Queue(ctx, true, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, i := range items {
		if i.TurnID == id && i.Handled && i.HandledAt != nil {
			found = true
		}
	}
	if !found {
		t.Error("a handled item cannot be found even when asked for")
	}

	// And somebody changing their verdict makes it work again.
	if err := s.Record(ctx, id, feedback.SourceOperator, feedback.VerdictWrong,
		"still wrong after the edit", "dana"); err != nil {
		t.Fatal(err)
	}
	if !inQueue(t, s, id) {
		t.Error("a re-reported item did not come back into the queue")
	}
}

func TestHelpfulIsCountedAndNotQueued(t *testing.T) {
	ctx := context.Background()
	s := feedback.NewStore(pool)
	id := turn(t, "good-"+stamp(), "what payment methods?", "Visa, Mastercard, PayPal")

	if err := s.Record(ctx, id, feedback.SourceCustomer, feedback.VerdictHelpful, "", "session-x"); err != nil {
		t.Fatal(err)
	}
	if inQueue(t, s, id) {
		t.Error("a helpful verdict was queued as work")
	}
	counts, err := s.Counts(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Helpful == 0 {
		t.Error("a helpful verdict was not counted")
	}
}

func TestFeedbackIsValidatedAndBounded(t *testing.T) {
	ctx := context.Background()
	s := feedback.NewStore(pool)
	id := turn(t, "valid-"+stamp(), "q", "a")

	if err := s.Record(ctx, id, feedback.SourceCustomer, "brilliant", "", "x"); !errors.Is(err, feedback.ErrBadVerdict) {
		t.Errorf("an invented verdict returned %v", err)
	}
	if err := s.Record(ctx, id, feedback.SourceCustomer, feedback.VerdictWrong,
		strings.Repeat("x", feedback.MaxNoteLength+1), "x"); err == nil {
		t.Error("an unbounded note was accepted")
	}
	if err := s.Record(ctx, id, feedback.SourceCustomer, feedback.VerdictWrong, "", "  "); err == nil {
		t.Error("feedback with no author was accepted")
	}
	// A turn that no longer exists -- swept by retention, say -- is a race rather than a
	// fault, and it must not be recorded against nothing.
	if err := s.Record(ctx, "no-such-turn", feedback.SourceCustomer, feedback.VerdictWrong,
		"", "x"); !errors.Is(err, feedback.ErrNoSuchTurn) {
		t.Errorf("feedback on a missing turn returned %v", err)
	}
}

func inQueue(t *testing.T, s *feedback.Store, id string) bool {
	t.Helper()
	items, err := s.Queue(context.Background(), false, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range items {
		if i.TurnID == id {
			return true
		}
	}
	return false
}

func stamp() string { return fmt.Sprint(time.Now().UnixNano()) }
