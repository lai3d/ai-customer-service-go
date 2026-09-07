package retention_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/retention"
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

// conversation writes one customer's worth of data: a couple of memory messages, a turn
// with its retrieved passages and tool calls, an owner row, and a ticket with history.
func conversation(t *testing.T, id, subject string, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-age)

	for i, m := range []struct{ role, content string }{
		{"user", "my order ORD-10045 has not arrived, my name is " + subject},
		{"assistant", "I have raised a ticket for you"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO chat_memory (conversation_id, role, content, created_at)
			VALUES ($1,$2,$3,$4)`, id, m.role, m.content, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	turnID := id + "-turn"
	if _, err := pool.Exec(ctx, `INSERT INTO turn
		(id, conversation_id, started_at, ended_at, outcome, question, reply, model, model_calls, input_tokens, output_tokens)
		VALUES ($1,$2,$3,$3,'completed','where is ORD-10045','it is coming','m',1,10,5)`,
		turnID, id, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO turn_passage (turn_id, rank, entry_id, language, score, question)
		VALUES ($1,1,'shipping','en',0.9,'where is my order')`, turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO turn_tool_call (turn_id, seq, name, outcome)
		VALUES ($1,1,'lookup_order_status','found')`, turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_owner (conversation_id, subject, created_at)
		VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, id, subject, at); err != nil {
		t.Fatal(err)
	}

	tk, _, err := ticket.NewStore(pool).Create(ctx, ticket.CreateRequest{
		ConversationID: id, Summary: "customer " + subject + " is chasing ORD-10045",
		Category: "shipping", OrderNumber: "ORD-10045"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ticket.NewStore(pool).Update(ctx, tk.Number, ticket.Update{
		Actor: "alex", ExpectedVersion: tk.Version, State: ticket.StateInProgress,
		Note: "called the customer on their mobile"}); err != nil {
		t.Fatal(err)
	}
	return tk.Number
}

func count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestErasureRemovesWhatTheCustomerSaid(t *testing.T) {
	ctx := context.Background()
	id := "erase-" + fmt.Sprint(time.Now().UnixNano())
	number := conversation(t, id, "subject-a", 0)

	// A customer's rating carries free text they wrote, in a table that did not exist
	// when this test did. It reaches the erasure by the cascade from `turn` and by
	// nothing else, which is exactly the kind of link that is true until somebody adds a
	// table without a foreign key.
	if _, err := pool.Exec(ctx, `INSERT INTO turn_feedback (turn_id, source, verdict, note, actor)
		VALUES ($1,'customer','wrong',$2,'subject-a')`,
		id+"-turn", "this is wrong, my name is subject-a and my order is ORD-10045"); err != nil {
		t.Fatal(err)
	}

	report, err := retention.NewStore(pool).EraseConversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if report.Empty() {
		t.Fatal("the erasure reported doing nothing")
	}

	for _, c := range []struct {
		what  string
		query string
	}{
		{"chat memory", `SELECT count(*) FROM chat_memory WHERE conversation_id = $1`},
		{"turns", `SELECT count(*) FROM turn WHERE conversation_id = $1`},
		{"the owner row", `SELECT count(*) FROM conversation_owner WHERE conversation_id = $1`},
	} {
		if n := count(t, c.query, id); n != 0 {
			t.Errorf("%s survived the erasure: %d rows", c.what, n)
		}
	}

	// The turn's children have to go with it. A retrieved-passage row is corpus text, but
	// which passages were retrieved for which question is a statement about what the
	// customer asked.
	if n := count(t, `SELECT count(*) FROM turn_passage WHERE turn_id = $1`, id+"-turn"); n != 0 {
		t.Errorf("%d retrieved-passage rows survived; the cascade is not doing its job", n)
	}
	if n := count(t, `SELECT count(*) FROM turn_tool_call WHERE turn_id = $1`, id+"-turn"); n != 0 {
		t.Errorf("%d tool-call rows survived", n)
	}
	if n := count(t, `SELECT count(*) FROM turn_feedback WHERE turn_id = $1`, id+"-turn"); n != 0 {
		t.Errorf("%d feedback rows survived, with whatever the customer typed in them", n)
	}
	if n := count(t,
		`SELECT count(*) FROM turn_feedback WHERE note LIKE '%subject-a%'`); n != 0 {
		t.Errorf("the customer's name is still in a feedback note %d times", n)
	}

	// Nothing anywhere may still hold the customer's words.
	if n := count(t,
		`SELECT count(*) FROM chat_memory WHERE content LIKE '%subject-a%'`); n != 0 {
		t.Errorf("the customer's name is still in chat_memory %d times", n)
	}
	if n := count(t,
		`SELECT count(*) FROM support_ticket WHERE summary LIKE '%subject-a%' OR order_number IS NOT NULL AND conversation_id = $1`, id); n != 0 {
		t.Errorf("the ticket still carries the customer's details")
	}
	_ = number
}

// What survives is the harder half. A ticket is a business record: erasing an OPEN one
// deletes the fact that somebody is owed a refund along with the words that asked for it.
func TestErasureRedactsTicketsRatherThanDeletingThem(t *testing.T) {
	ctx := context.Background()
	id := "redact-" + fmt.Sprint(time.Now().UnixNano())
	number := conversation(t, id, "subject-b", 0)

	if _, err := retention.NewStore(pool).EraseConversation(ctx, id); err != nil {
		t.Fatal(err)
	}

	var summary, state string
	var order, resolution *string
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT summary, state, order_number, resolution, version FROM support_ticket WHERE ticket_number = $1`,
		number).Scan(&summary, &state, &order, &resolution, &version); err != nil {
		t.Fatalf("the ticket was deleted rather than redacted: %v", err)
	}
	if summary != retention.Redacted {
		t.Errorf("the summary was not redacted: %q", summary)
	}
	if order != nil {
		t.Errorf("the order number survived: %q", *order)
	}
	if state != string(ticket.StateInProgress) {
		t.Errorf("the ticket's state changed to %q; erasure must not move a ticket", state)
	}

	// The history keeps who did what and when, and loses only what was written.
	rows, err := pool.Query(ctx,
		`SELECT actor, action, coalesce(detail,'') FROM ticket_event WHERE ticket_number = $1 ORDER BY at`,
		number)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events int
	for rows.Next() {
		var actor, action, detail string
		if err := rows.Scan(&actor, &action, &detail); err != nil {
			t.Fatal(err)
		}
		events++
		if actor == "" || action == "" {
			t.Error("an event lost its attribution")
		}
		if detail != "" && detail != retention.Redacted {
			t.Errorf("an event kept its text: %q", detail)
		}
	}
	if events < 2 {
		t.Errorf("the ticket has %d events; creation and the claim should both survive", events)
	}
}

// An audit row the subject of the audit can erase is not an audit row.
func TestErasureLeavesTheAuditTrailAlone(t *testing.T) {
	ctx := context.Background()
	id := "audit-" + fmt.Sprint(time.Now().UnixNano())
	conversation(t, id, "subject-c", 0)

	if _, err := pool.Exec(ctx,
		`INSERT INTO admin_audit (actor, action, object, outcome, detail)
		 VALUES ('alex','read conversation',$1,'ok','')`, "conversation/"+id); err != nil {
		t.Fatal(err)
	}
	before := count(t, `SELECT count(*) FROM admin_audit`)

	if _, err := retention.NewStore(pool).EraseConversation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := count(t, `SELECT count(*) FROM admin_audit`); after != before {
		t.Errorf("the audit trail went from %d rows to %d; an erasure must not touch it",
			before, after)
	}
	if n := count(t, `SELECT count(*) FROM admin_audit WHERE object = $1`,
		"conversation/"+id); n == 0 {
		t.Error("the record of who read this conversation was erased with it")
	}
}

func TestErasingASubjectCoversEveryConversationTheyOwn(t *testing.T) {
	ctx := context.Background()
	stamp := fmt.Sprint(time.Now().UnixNano())
	subject := "subject-d-" + stamp
	first, second := "sub-a-"+stamp, "sub-b-"+stamp
	conversation(t, first, subject, 0)
	conversation(t, second, subject, 0)
	// Somebody else's conversation, which must be untouched.
	other := "sub-other-" + stamp
	conversation(t, other, "subject-e-"+stamp, 0)

	if _, err := pool.Exec(ctx, `INSERT INTO chat_session (token_hash, subject, created_at, last_seen_at, expires_at)
		VALUES (sha256($1::bytea), $2, now(), now(), now() + interval '1 hour')`,
		[]byte(stamp), subject); err != nil {
		t.Fatal(err)
	}

	report, ids, err := retention.NewStore(pool).EraseSubject(ctx, subject)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("erased %d conversations, want both of the subject's: %v", len(ids), ids)
	}
	if report.Sessions == 0 {
		t.Error("the subject's session survived; they can keep talking as the identity they asked to remove")
	}
	for _, id := range []string{first, second} {
		if n := count(t, `SELECT count(*) FROM turn WHERE conversation_id = $1`, id); n != 0 {
			t.Errorf("%s still has turns", id)
		}
	}
	if n := count(t, `SELECT count(*) FROM turn WHERE conversation_id = $1`, other); n == 0 {
		t.Error("somebody else's conversation was erased too")
	}
}

func TestTheSweepTakesOldDataAndLeavesTicketsAndAudit(t *testing.T) {
	ctx := context.Background()
	stamp := fmt.Sprint(time.Now().UnixNano())
	old, recent := "old-"+stamp, "recent-"+stamp
	oldTicket := conversation(t, old, "subject-f-"+stamp, 60*24*time.Hour)
	conversation(t, recent, "subject-g-"+stamp, time.Hour)

	report, err := retention.NewStore(pool).Sweep(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.Turns == 0 {
		t.Fatal("the sweep removed no turns though one is sixty days old")
	}

	if n := count(t, `SELECT count(*) FROM turn WHERE conversation_id = $1`, old); n != 0 {
		t.Errorf("the sixty-day-old turn survived the thirty-day window")
	}
	if n := count(t, `SELECT count(*) FROM turn WHERE conversation_id = $1`, recent); n == 0 {
		t.Error("an hour-old turn was swept by a thirty-day window")
	}
	if n := count(t, `SELECT count(*) FROM chat_memory WHERE conversation_id = $1`, recent); n == 0 {
		t.Error("an hour-old conversation lost its memory")
	}

	// A ticket ages on its own lifecycle. Expiring it with the conversation would delete
	// an obligation because the request that raised it got old.
	if n := count(t, `SELECT count(*) FROM support_ticket WHERE ticket_number = $1`, oldTicket); n != 1 {
		t.Error("the sweep deleted a ticket; retention for tickets is a separate decision")
	}

	// Disabled means disabled, not "a window of zero".
	before := count(t, `SELECT count(*) FROM turn`)
	if r, err := retention.NewStore(pool).Sweep(ctx, 0); err != nil || !r.Empty() {
		t.Errorf("a zero window swept something: %v %v", r, err)
	}
	if after := count(t, `SELECT count(*) FROM turn`); after != before {
		t.Errorf("a zero window deleted %d turns", before-after)
	}
}
