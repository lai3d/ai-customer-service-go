package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
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

func newConversation(t *testing.T) string {
	t.Helper()
	return "conv-" + strings.ReplaceAll(t.Name(), "/", "-")
}

// The cap used to be `replicas x 3`, enforced in a map in each process, and the README
// said so as a known limitation. It is a guarantee now, and this is the test that says
// so: two pools are two replicas as far as Postgres is concerned.
func TestTheCapHoldsAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	conversation := newConversation(t)

	second, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	replicas := []*ticket.Store{ticket.NewStore(pool), ticket.NewStore(second)}

	var wg sync.WaitGroup
	outcomes := make([]ticket.Outcome, 20)
	errs := make([]error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, outcome, err := replicas[i%2].Create(ctx, ticket.CreateRequest{
				ConversationID: conversation,
				Summary:        fmt.Sprintf("differently worded problem %d", i),
				Category:       "returns",
			})
			outcomes[i], errs[i] = outcome, err
		}(i)
	}
	wg.Wait()

	created := 0
	for i, o := range outcomes {
		if errs[i] != nil {
			t.Fatalf("create %d failed: %v", i, errs[i])
		}
		if o == ticket.OutcomeCreated {
			created++
		}
	}
	if created != ticket.MaxPerConversation {
		t.Errorf("twenty differently-worded requests across two replicas created %d "+
			"tickets, want %d", created, ticket.MaxPerConversation)
	}
}

// Deduplication has the same story: it used to hold only within one replica.
func TestDeduplicationHoldsAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	conversation := newConversation(t)

	second, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	a, b := ticket.NewStore(pool), ticket.NewStore(second)
	const summary = "Refund has not arrived"

	first, outcome, err := a.Create(ctx, ticket.CreateRequest{
		ConversationID: conversation, Summary: summary, Category: "returns"})
	if err != nil || outcome != ticket.OutcomeCreated {
		t.Fatalf("first create: %v %v", outcome, err)
	}

	// Same request, different replica, different whitespace and case.
	again, outcome, err := b.Create(ctx, ticket.CreateRequest{
		ConversationID: conversation, Summary: "  refund has NOT   arrived ", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != ticket.OutcomeExisted {
		t.Errorf("outcome is %q, want %q", outcome, ticket.OutcomeExisted)
	}
	if again.Number != first.Number {
		t.Errorf("the other replica invented %s instead of returning %s",
			again.Number, first.Number)
	}
}

func TestTheStateMachineRefusesImpossibleMoves(t *testing.T) {
	ctx := context.Background()
	store := ticket.NewStore(pool)
	created, _, err := store.Create(ctx, ticket.CreateRequest{
		ConversationID: newConversation(t), Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}

	// OPEN cannot jump straight to RESOLVED.
	if _, err := store.Update(ctx, created.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: created.Version,
		State: ticket.StateResolved, Resolution: "done",
	}); err == nil {
		t.Error("a ticket moved from OPEN to RESOLVED without being worked")
	}
}

func TestResolvingNeedsAConclusionAndReopeningNeedsAReason(t *testing.T) {
	ctx := context.Background()
	store := ticket.NewStore(pool)
	tk, _, err := store.Create(ctx, ticket.CreateRequest{
		ConversationID: newConversation(t), Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}

	tk, err = store.Update(ctx, tk.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: tk.Version, State: ticket.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}

	// A RESOLVED ticket with no record of what was done is indistinguishable from an
	// abandoned one.
	if _, err := store.Update(ctx, tk.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: tk.Version, State: ticket.StateResolved,
	}); err == nil {
		t.Error("a ticket was resolved with no conclusion")
	}

	tk, err = store.Update(ctx, tk.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: tk.Version,
		State: ticket.StateResolved, Resolution: "refund issued"})
	if err != nil {
		t.Fatal(err)
	}

	// A ticket that comes back is the interesting case, and why is the only part worth
	// keeping.
	if _, err := store.Update(ctx, tk.Number, ticket.Update{
		Actor: "sam", ExpectedVersion: tk.Version, State: ticket.StateInProgress,
	}); err == nil {
		t.Error("a resolved ticket was reopened with no reason")
	}

	tk, err = store.Update(ctx, tk.Number, ticket.Update{
		Actor: "sam", ExpectedVersion: tk.Version,
		State: ticket.StateInProgress, Reason: "customer says it never arrived"})
	if err != nil {
		t.Fatal(err)
	}

	_, events, err := store.Get(ctx, tk.Number)
	if err != nil {
		t.Fatal(err)
	}
	var actors []string
	for _, e := range events {
		actors = append(actors, e.Actor)
	}
	if len(events) < 4 {
		t.Errorf("history has %d entries (%v); creation, two transitions and the reopen "+
			"should all be attributable", len(events), actors)
	}
	if !strings.Contains(events[len(events)-1].Detail, "never arrived") {
		t.Errorf("the reopen reason is not in the history: %+v", events[len(events)-1])
	}
}

// Two operators with the same ticket open. Without the version check the second write
// wins silently and the first operator never learns their change is gone -- the shape of
// bug that only appears once two people use the thing.
func TestTwoOperatorsCannotOverwriteEachOther(t *testing.T) {
	ctx := context.Background()
	store := ticket.NewStore(pool)
	tk, _, err := store.Create(ctx, ticket.CreateRequest{
		ConversationID: newConversation(t), Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}
	stale := tk.Version

	alex := "alex"
	if _, err := store.Update(ctx, tk.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: stale, Assignee: &alex}); err != nil {
		t.Fatal(err)
	}

	sam := "sam"
	_, err = store.Update(ctx, tk.Number, ticket.Update{
		Actor: "sam", ExpectedVersion: stale, Assignee: &sam})
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("the second operator got %v, want a conflict", err)
	}

	after, _, err := store.Get(ctx, tk.Number)
	if err != nil {
		t.Fatal(err)
	}
	if after.Assignee != "alex" {
		t.Errorf("assignee is %q; the losing write was applied anyway", after.Assignee)
	}
}

func TestUnknownTicketsAreNotFoundRatherThanEmpty(t *testing.T) {
	_, _, err := ticket.NewStore(pool).Get(context.Background(), "TKT-does-not-exist")
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
