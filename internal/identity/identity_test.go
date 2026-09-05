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

	_, mine, _, err := sessions.Issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, theirs, _, err := sessions.Issue(ctx)
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
	_, subject, _, err := sessions.Issue(ctx)
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
		_, s, _, err := sessions.Issue(ctx)
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
	token, subject, _, err := sessions.Issue(ctx)
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
	stale, _, _, err := sessions.Issue(ctx)
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
