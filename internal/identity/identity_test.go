package identity_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/identity"
	"github.com/lai3d/ai-customer-service-go/internal/tenant"
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

// The defect this package exists for, stated as a test: a conversation id was the whole
// of the authorisation, so anyone holding one could append to that history and have the
// model answer with its context in the prompt.
func TestAConversationBelongsToTheSubjectThatStartedIt(t *testing.T) {
	ctx := context.Background()
	conversations := identity.NewConversations(pool)
	sessions := identity.NewSessions(pool, time.Hour)

	_, mine, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	_, theirs, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}

	const id = "conv-ownership"
	if err := conversations.Claim(ctx, id, mine); err != nil {
		t.Fatal(err)
	}
	// Mine again is fine: a second turn in the same conversation is the normal case.
	if err := conversations.Claim(ctx, id, mine); err != nil {
		t.Errorf("the owner was refused their own conversation: %v", err)
	}
	if err := conversations.Owns(ctx, id, mine); err != nil {
		t.Errorf("the owner does not own it: %v", err)
	}

	if err := conversations.Owns(ctx, id, theirs); !errors.Is(err, identity.ErrNotYours) {
		t.Errorf("another subject was allowed into the conversation: %v", err)
	}
	if err := conversations.Claim(ctx, id, theirs); !errors.Is(err, identity.ErrNotYours) {
		t.Errorf("another subject claimed a conversation that was already owned: %v", err)
	}
}

// An id nobody has used is not "available": telling the two apart is what lets an id be
// probed for existence, and the handler turns both into the same 404 for that reason.
func TestAnUnknownConversationIsNotFoundRatherThanFree(t *testing.T) {
	ctx := context.Background()
	sessions := identity.NewSessions(pool, time.Hour)
	_, subject, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.NewConversations(pool).Owns(ctx, "never-used", subject); !errors.Is(err, identity.ErrNoSuchConv) {
		t.Errorf("an unknown conversation reported %v", err)
	}
}

// Two first turns arriving together must not both succeed. A SELECT-then-INSERT passes
// this test on a laptop and fails it under load, which is the shape the ticket cap had.
func TestTwoSubjectsClaimingTheSameIdAtOnceProduceOneOwner(t *testing.T) {
	ctx := context.Background()
	conversations := identity.NewConversations(pool)
	sessions := identity.NewSessions(pool, time.Hour)

	const attempts = 12
	subjects := make([]identity.Subject, attempts)
	for i := range subjects {
		_, s, _, err := sessions.Issue(ctx, tenant.Default)
		if err != nil {
			t.Fatal(err)
		}
		subjects[i] = s
	}

	const id = "conv-race"
	var wg sync.WaitGroup
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := range subjects {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = conversations.Claim(ctx, id, subjects[i])
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, identity.ErrNotYours):
		default:
			t.Errorf("subject %d got an unexpected error: %v", i, err)
		}
	}
	if won != 1 {
		t.Errorf("%d subjects claimed the same conversation; exactly one may", won)
	}
}

func TestASessionTokenIsNotStoredAndAnExpiredOneDoesNotWork(t *testing.T) {
	ctx := context.Background()

	// The token must not be recoverable from the database. A dump of chat_session is a
	// dump of live credentials otherwise.
	sessions := identity.NewSessions(pool, time.Hour)
	token, subject, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_session WHERE encode(token_hash,'escape') LIKE '%' || $1 || '%'`,
		token).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Error("the session token itself is in the database")
	}

	got, err := sessions.Verify(ctx, token)
	if err != nil || got.ID != subject.ID {
		t.Fatalf("a fresh token did not verify: %v (%q vs %q)", err, got.ID, subject.ID)
	}
	if _, err := sessions.Verify(ctx, token+"x"); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("a token with a character appended verified: %v", err)
	}
	if _, err := sessions.Verify(ctx, ""); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("an empty token verified: %v", err)
	}

	// An expired session is refused on read, not merely swept on some schedule -- a
	// sweeper that is late is otherwise a session that still works.
	//
	// The row is aged with SQL rather than by constructing a Sessions with a negative
	// TTL: the constructor treats any non-positive TTL as "unset" and substitutes a day,
	// so that version of this test passed while proving only that the guard exists.
	stale, _, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE chat_session SET expires_at = now() - interval '1 minute'
		 WHERE token_hash = sha256($1::bytea)`, []byte(stale)); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Verify(ctx, stale); !errors.Is(err, identity.ErrNoSession) {
		t.Errorf("an expired session verified: %v", err)
	}

	n, err := sessions.Sweep(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("the sweep removed nothing though an expired session exists")
	}
}

// The rows nobody reads again actually go.
//
// This test exists because `SweepWindows` and `Sessions.Sweep` were both written, both
// tested, and called by nothing: `rate_window` grew for ever while two correct sweepers
// sat there. "The function exists" and "the work happens" are different claims, and only
// the first had a test.
func TestExpiredSessionsAndClosedWindowsAreActuallySwept(t *testing.T) {
	ctx := context.Background()
	sessions := identity.NewSessions(pool, time.Hour)
	limits := identity.NewLimits(pool)

	token, _, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_session SET expires_at = now() - interval '2 days'
		WHERE token_hash = sha256($1::bytea)`, []byte(token)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rate_window (tenant_id, bucket, subject, window_start, count)
		VALUES ('default', 'turn', 'swept-subject', now() - interval '3 days', 1)`); err != nil {
		t.Fatal(err)
	}
	// And rows that are still in use must survive the same sweep.
	live, _, _, err := sessions.Issue(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rate_window (tenant_id, bucket, subject, window_start, count)
		VALUES ('default', 'turn', 'live-subject', now(), 1)`); err != nil {
		t.Fatal(err)
	}

	sweptSessions, sweptWindows, err := identity.NewHygiene(sessions, limits, time.Hour).Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweptSessions == 0 {
		t.Error("the expired session survived")
	}
	if sweptWindows == 0 {
		t.Error("the closed rate window survived, which is the table that grew for ever")
	}

	if _, err := sessions.Verify(ctx, live); err != nil {
		t.Errorf("a live session was swept: %v", err)
	}
	var liveWindows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rate_window WHERE subject = 'live-subject'`).Scan(&liveWindows); err != nil {
		t.Fatal(err)
	}
	if liveWindows == 0 {
		t.Error("the current rate window was swept, so a request at a boundary loses its count")
	}
}

// The tenant predicate itself, with the subject held constant.
//
// This exists because the obvious test does not test this. Two tenants at the edge get two
// different subjects, so a cross-tenant request is already refused by the subject
// comparison -- and removing the tenant from the WHERE clause leaves that test green. The
// perturbation was run and it passed, which is the only reason this test is here.
//
// Holding the subject equal is not a hypothetical. Subject ids are server-issued today;
// the predicate is what keeps this correct if they ever become something a customer
// supplies, and it is what protects every lookup that has a conversation id and no
// subject -- the transcript, the operations surface, retention.
func TestAConversationIsNotReachableFromAnotherTenantWithTheSameSubject(t *testing.T) {
	ctx := context.Background()
	store := tenant.NewStore(pool)
	a, err := store.Create(ctx, fmt.Sprintf("own-a-%d", time.Now().UnixNano()), "A", "platform")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(ctx, fmt.Sprintf("own-b-%d", time.Now().UnixNano()), "B", "platform")
	if err != nil {
		t.Fatal(err)
	}

	conversations := identity.NewConversations(pool)
	id := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	// The same subject string in both tenants, which is the whole point of the test.
	const subject = "the-same-subject-id"
	inA := identity.Subject{ID: subject, TenantID: a.ID}
	inB := identity.Subject{ID: subject, TenantID: b.ID}

	if err := conversations.Claim(ctx, id, inA); err != nil {
		t.Fatal(err)
	}
	if err := conversations.Owns(ctx, id, inA); err != nil {
		t.Errorf("the owning tenant cannot reach its own conversation: %v", err)
	}
	if err := conversations.Owns(ctx, id, inB); !errors.Is(err, identity.ErrNoSuchConv) {
		t.Errorf("another tenant with the same subject id reached the conversation: %v", err)
	}
	// And claiming it from the other tenant does not take it over, which is the same
	// question asked of the write path rather than the read path.
	if err := conversations.Claim(ctx, id, inB); !errors.Is(err, identity.ErrNotYours) {
		t.Errorf("another tenant claimed a conversation that exists: %v", err)
	}
	if err := conversations.Owns(ctx, id, inA); err != nil {
		t.Errorf("the owner lost the conversation to the other tenant's claim: %v", err)
	}
}

// A session with no tenant is refused rather than defaulted. A row with no tenant belongs
// to whichever tenant the next query forgets to filter on.
func TestASessionCannotBeIssuedWithoutATenant(t *testing.T) {
	sessions := identity.NewSessions(pool, time.Hour)
	if _, _, _, err := sessions.Issue(context.Background(), ""); err == nil {
		t.Error("a session was issued with no tenant")
	}
}

// Losing a claim must say "not yours", not "the database returned nothing".
//
// The twelve-way race above shows the failure happens; this shows it *must*, without
// goroutine timing deciding the outcome. An uncommitted insert is invisible to another
// session, so a claim that conflicts with it used to find neither its own row (the insert
// did nothing) nor the winner's (not yet visible) and returned `no rows in result set` --
// which the edge turns into a 503 on the first turn of a conversation.
//
// Construction borrowed from TestTheExtensionRaceIsTransactionVisibilityNotTiming in
// internal/store, which made the same class of race deterministic the same way.
func TestAClaimLosingToAnUncommittedWinnerIsToldItLost(t *testing.T) {
	ctx := context.Background()
	conversations := identity.NewConversations(pool)
	id := fmt.Sprintf("uncommitted-%d", time.Now().UnixNano())

	winner := identity.Subject{ID: "winner-subject", TenantID: tenant.Default}
	loser := identity.Subject{ID: "loser-subject", TenantID: tenant.Default}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_owner (conversation_id, subject, tenant_id, created_at)
		VALUES ($1,$2,$3, now())`, id, winner.ID, winner.TenantID); err != nil {
		t.Fatal(err)
	}

	claimed := make(chan error, 1)
	go func() { claimed <- conversations.Claim(ctx, id, loser) }()

	// The claim must be blocked on the row lock rather than already finished. The sleep
	// orders the commit after the claim has started; if the claim were not blocking, it
	// would already have answered and the assertion below would be about the wrong thing.
	select {
	case err := <-claimed:
		t.Fatalf("the claim answered %v before the winner committed; it is not waiting, "+
			"which is the bug this test is about", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-claimed:
		if !errors.Is(err, identity.ErrNotYours) {
			t.Errorf("the losing claim returned %v, want ErrNotYours", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the losing claim never returned")
	}
}
