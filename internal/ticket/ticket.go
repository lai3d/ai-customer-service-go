// Package ticket owns support tickets: the AI creates them, people work them.
//
// They used to live in a map in each process. That made two of this repository's
// documented limits real defects rather than honest simplifications -- the
// three-per-conversation cap was actually `replicas x 3`, and deduplication only held
// within whichever replica happened to serve the request. An operations surface makes
// both visible to a human instead of only to a reader of the docs: two operators would
// see two different sets of tickets and neither would be wrong.
package ticket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxPerConversation bounds what a persuaded model can put in a human queue. The system
// prompt asks the model to treat customer text as data; this is what actually holds.
const MaxPerConversation = 3

type State string

const (
	StateOpen       State = "OPEN"
	StateInProgress State = "IN_PROGRESS"
	StateResolved   State = "RESOLVED"
	StateClosed     State = "CLOSED"
)

type Ticket struct {
	Number         string    `json:"ticketNumber"`
	ConversationID string    `json:"conversationId"`
	Category       string    `json:"category"`
	Summary        string    `json:"summary"`
	OrderNumber    string    `json:"orderNumber,omitempty"`
	State          State     `json:"state"`
	Assignee       string    `json:"assignee,omitempty"`
	Resolution     string    `json:"resolution,omitempty"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Outcome says what a creation attempt did. It is a value rather than an error for the
// same reason a missing order is: the model reads this and writes an answer from it.
type Outcome string

const (
	OutcomeCreated Outcome = "created"
	OutcomeExisted Outcome = "duplicate_suppressed"
	OutcomeCapped  Outcome = "capped"
)

// Creator is what the model-facing tool needs. An interface so the tool's own tests do
// not need a database, and so the behaviour that does need one is tested where it lives.
type Creator interface {
	Create(ctx context.Context, req CreateRequest) (Ticket, Outcome, error)
}

type CreateRequest struct {
	ConversationID string
	Summary        string
	Category       string
	OrderNumber    string
}

// ErrConflict is returned when an admin update lost an optimistic-concurrency check.
var ErrConflict = errors.New("ticket was modified by someone else")

// ErrNotFound is returned for an unknown ticket number.
var ErrNotFound = errors.New("no such ticket")

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Create deduplicates and caps in one transaction.
//
// Both guards are inside a transaction holding an advisory lock on the conversation,
// because checking the count and then inserting is not the same as doing both
// atomically -- and now that replicas share a database, two concurrent calls with
// different wording could each see two tickets and each add a third. The unique index
// on (conversation_id, dedupe_key) is the backstop that makes deduplication true even
// if this lock is ever removed.
func (s *Store) Create(ctx context.Context, req CreateRequest) (Ticket, Outcome, error) {
	key := DedupeKey(req.Summary)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, "", err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, req.ConversationID); err != nil {
		return Ticket{}, "", fmt.Errorf("lock conversation: %w", err)
	}

	existing, err := scanOne(tx.QueryRow(ctx, selectColumns+`
		FROM support_ticket WHERE conversation_id = $1 AND dedupe_key = $2`,
		req.ConversationID, key))
	switch {
	case err == nil:
		return existing, OutcomeExisted, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return Ticket{}, "", err
	}

	var open int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM support_ticket WHERE conversation_id = $1`,
		req.ConversationID).Scan(&open); err != nil {
		return Ticket{}, "", err
	}
	if open >= MaxPerConversation {
		return Ticket{}, OutcomeCapped, tx.Commit(ctx)
	}

	created, err := scanOne(tx.QueryRow(ctx, `
		INSERT INTO support_ticket
			(ticket_number, conversation_id, dedupe_key, category, summary, order_number)
		VALUES ('TKT-' || nextval('support_ticket_number_seq'), $1, $2, $3, $4, NULLIF($5, ''))
		RETURNING `+columns, req.ConversationID, key, NormaliseCategory(req.Category),
		req.Summary, req.OrderNumber))
	if err != nil {
		return Ticket{}, "", err
	}
	if err := appendEvent(ctx, tx, created.Number, "assistant", "created", req.Summary); err != nil {
		return Ticket{}, "", err
	}
	return created, OutcomeCreated, tx.Commit(ctx)
}

// DedupeKey is the normalised summary. Exported because it is the contract the unique
// index enforces, and a caller that normalises differently would defeat it.
func DedupeKey(summary string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(summary))), " ")
}

func NormaliseCategory(category string) string {
	switch c := DedupeKey(category); c {
	case "returns", "shipping", "payment", "account":
		return c
	default:
		return "other"
	}
}

const columns = `ticket_number, conversation_id, category, summary,
	coalesce(order_number, ''), state, coalesce(assignee, ''), coalesce(resolution, ''),
	version, created_at, updated_at`

const selectColumns = `SELECT ` + columns

type row interface{ Scan(dest ...any) error }

func scanOne(r row) (Ticket, error) {
	var t Ticket
	err := r.Scan(&t.Number, &t.ConversationID, &t.Category, &t.Summary, &t.OrderNumber,
		&t.State, &t.Assignee, &t.Resolution, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func appendEvent(ctx context.Context, tx pgx.Tx, number, actor, action, detail string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO ticket_event (ticket_number, actor, action, detail) VALUES ($1,$2,$3,NULLIF($4,''))`,
		number, actor, action, detail)
	return err
}
