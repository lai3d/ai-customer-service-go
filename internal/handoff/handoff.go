// Package handoff is the loop back to a human, and back from them.
//
// A ticket used to be a row and nothing else happened. It was created, deduplicated,
// capped and audited, and then it sat there: nothing told a person it existed, and when a
// person did reply there was no path back to the customer who had asked. That is the line
// between an assistant that escalates and a chat box that files tickets into a drawer.
//
// Two directions, and they fail differently:
//
//   - **Outbound**, to whoever answers tickets: a webhook, because it is the one shape
//     every destination accepts. Delivery is recorded, including failure, because a
//     notification that silently did not arrive is worse than none — nobody is waiting for
//     one they know did not come.
//   - **Inbound**, back to the customer: the operator's reply is written into the
//     conversation as an assistant message. That is not decoration. The model's next turn
//     composes from that history, so without it the assistant would cheerfully tell a
//     customer that a human will be in touch about something a human already answered.
package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
)

var (
	ErrNoSuchTicket = errors.New("no such ticket")
	ErrEmptyReply   = errors.New("a reply with no text is not a reply")
)

// Memory is the part of chat.Memory this package needs. An interface so a reply can be
// tested without a chat service, and so this package does not depend on that one.
type Memory interface {
	Append(ctx context.Context, conversationID string, role llm.Role, content string) error
}

type Store struct {
	pool     *pgxpool.Pool
	memory   Memory
	notifier *Notifier
}

func NewStore(pool *pgxpool.Pool, memory Memory, notifier *Notifier) *Store {
	return &Store{pool: pool, memory: memory, notifier: notifier}
}

// Reply sends an operator's words to the customer.
//
// Everything happens in one transaction except the notification: the ticket event, the
// message in the customer's conversation, and the ticket's updated timestamp either all
// happen or none do. A reply recorded on the ticket but missing from the conversation
// would be a human answer the customer never sees and the model then contradicts.
func (s *Store) Reply(ctx context.Context, number, actor, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrEmptyReply
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("a reply needs an author")
	}

	var conversationID string
	err := s.pool.QueryRow(ctx,
		`SELECT conversation_id FROM support_ticket WHERE ticket_number = $1`, number).
		Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchTicket
	}
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO ticket_event (ticket_number, at, actor, action, detail)
		VALUES ($1, now(), $2, 'replied', $3)`, number, actor, text); err != nil {
		return fmt.Errorf("record the reply: %w", err)
	}
	// Attributed in the text itself. The conversation has one assistant role and the
	// customer cannot see a database column, so "Alex from support" has to be in the words
	// or the customer is told a machine answered them.
	if _, err := tx.Exec(ctx, `
		INSERT INTO chat_memory (conversation_id, role, content, created_at)
		VALUES ($1, 'assistant', $2, now())`,
		conversationID, actor+" (support): "+text); err != nil {
		return fmt.Errorf("deliver the reply: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE support_ticket SET updated_at = now() WHERE ticket_number = $1`, number); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.notifier.Send(ctx, Event{
		Type: "ticket.replied", Ticket: number, Conversation: conversationID, Actor: actor})
	return nil
}

// Announce tells the destination that a new ticket exists. It satisfies tools.Announcer.
func (s *Store) Announce(ctx context.Context, number, conversationID, category string) {
	s.notifier.Send(ctx, Event{
		Type: "ticket.created", Ticket: number, Conversation: conversationID, Category: category})
}

// Transcript is what the customer sees when they come back: their own messages and
// everything answered, whether by the model or by a person.
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

func (s *Store) Transcript(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT role, content, created_at FROM chat_memory
		WHERE conversation_id = $1 ORDER BY id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Content, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Event is what a destination is told. Deliberately thin: a ticket number, a conversation
// id, and what happened.
//
// It carries **no customer text**. Everything else in this service works to keep that text
// out of the places it leaks to, and a webhook is by definition a place outside this
// service's control -- often a chat room with a search box and a wide audience. Whoever
// receives this can open the operations UI, where reading it is an audited action.
type Event struct {
	Type         string    `json:"type"`
	Ticket       string    `json:"ticket"`
	Conversation string    `json:"conversation"`
	Category     string    `json:"category,omitempty"`
	Actor        string    `json:"actor,omitempty"`
	At           time.Time `json:"at"`
}

// Notifier posts events to a webhook. A nil Notifier is a working no-op, which is what
// makes the URL optional without every caller checking.
type Notifier struct {
	url     string
	client  *http.Client
	pool    *pgxpool.Pool
	metrics *obs.Metrics
}

func NewNotifier(pool *pgxpool.Pool, url string, timeout time.Duration) *Notifier {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Notifier{url: strings.TrimSpace(url), client: &http.Client{Timeout: timeout}, pool: pool}
}

// Meter attaches the delivery counter, so that a notification nobody received is a
// number an alert can reach rather than only a row somebody has to go and read.
//
// It is a separate call rather than a constructor parameter because two test packages
// build a Notifier with no registry at all, and a counter is not worth a required
// argument in every one of them. That makes it possible to forget, which is why
// TestTheHandoffNotifierIsMeteredWhereItIsConstructed reads the one production
// construction and fails if the wiring goes missing: an optional meter is otherwise a
// hole with nothing looking at it.
func (n *Notifier) Meter(metrics *obs.Metrics) *Notifier {
	if n != nil {
		n.metrics = metrics
	}
	return n
}

func (n *Notifier) Enabled() bool { return n != nil && n.url != "" }

// Send delivers asynchronously and records the outcome.
//
// Asynchronous because a slow webhook must not become a slow ticket: the model has already
// promised the customer a human, and failing the turn because a chat room was unreachable
// would break the thing that worked to protect the thing that did not.
//
// Recorded because the failure mode of a notification is silence, and silence is
// indistinguishable from "nothing happened". `handoff_delivery` is what makes "we were
// never told" answerable.
func (n *Notifier) Send(ctx context.Context, e Event) {
	if !n.Enabled() {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	// Detached: the customer's request is finishing and this outlives it.
	go n.deliver(context.WithoutCancel(ctx), e)
}

func (n *Notifier) deliver(ctx context.Context, e Event) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(e)
	if err != nil {
		n.record(ctx, e, 0, err.Error())
		return
	}

	var lastErr string
	var status int
	// Two attempts, because the common failure is a moment rather than a state. Not more:
	// a retry loop against a destination that is down is a way of turning one outage into
	// two, and the record is what makes a missed notification recoverable by hand.
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
		if err != nil {
			lastErr = err.Error()
			break
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.client.Do(req)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		status = resp.StatusCode
		resp.Body.Close()
		if status >= 200 && status < 300 {
			n.record(ctx, e, status, "")
			return
		}
		lastErr = fmt.Sprintf("HTTP %d", status)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	slog.Error("could not notify the handoff destination",
		"ticket", e.Ticket, "type", e.Type, "error", lastErr)
	n.record(ctx, e, status, lastErr)
}

func (n *Notifier) record(ctx context.Context, e Event, status int, failure string) {
	// Counted here rather than at the two call sites above, because this is the one
	// place every outcome passes through -- the same reason the row is written here.
	outcome := "delivered"
	if failure != "" {
		outcome = "failed"
	}
	if n.metrics != nil {
		n.metrics.Handoffs.WithLabelValues(e.Type, outcome).Inc()
	}
	if _, err := n.pool.Exec(ctx, `
		INSERT INTO handoff_delivery (at, type, ticket_number, status, failure)
		VALUES (now(), $1, $2, $3, NULLIF($4,''))`,
		e.Type, e.Ticket, status, failure); err != nil {
		slog.Error("could not record a handoff delivery", "ticket", e.Ticket, "error", err)
	}
}

// Undelivered is what the operations overview reads: notifications that never arrived.
func (n *Notifier) Undelivered(ctx context.Context, since time.Duration) (int, error) {
	if n == nil || n.pool == nil {
		return 0, nil
	}
	var count int
	err := n.pool.QueryRow(ctx, `
		SELECT count(*) FROM handoff_delivery
		WHERE failure IS NOT NULL AND at > now() - $1::interval`, since.String()).Scan(&count)
	return count, err
}
