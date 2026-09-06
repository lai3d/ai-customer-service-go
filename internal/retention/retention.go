// Package retention deletes customer data: on a schedule because it is old, and on
// request because someone asked to be forgotten.
//
// Until this existed there was no `DELETE` against customer data anywhere in the service.
// Everything a customer had ever typed sat in `chat_memory` and `turn` in plain text for
// ever, and a deletion request was a hand-written SQL statement by whoever had the
// password. That is not a missing feature so much as a missing answer to a question every
// real deployment is eventually asked.
//
// Two things this package will not do, and both are the point rather than an omission:
//
//   - **It never touches `admin_audit`.** An audit row that the subject of the audit can
//     erase is not an audit row. What the trail holds is who did what to which object --
//     an operator's name, a ticket number, a conversation id -- and no customer text, so
//     there is nothing in it to erase.
//   - **It redacts tickets rather than deleting them.** A support ticket is a business
//     record with its own lifecycle, and deleting an open one erases the fact that a
//     refund is owed along with the words that asked for it. The customer's words go; the
//     ticket, its state, its history and who did what survive.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Redacted is what replaces customer text in the rows that survive an erasure. It is a
// marker rather than an empty string on purpose: a blank summary reads as a bug, and an
// operator looking at the ticket should be able to tell "erased on request" from "the
// model wrote nothing".
const Redacted = "[erased]"

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Report is what a sweep or an erasure actually did. Returned rather than logged so the
// caller can audit it, and so a test can tell "deleted nothing because there was nothing"
// from "deleted nothing because the statement was wrong".
type Report struct {
	ChatMemory    int64 `json:"chatMemory"`
	Turns         int64 `json:"turns"`
	Conversations int64 `json:"conversations"`
	Sessions      int64 `json:"sessions"`
	// TicketsRedacted counts tickets whose text was replaced. They are not deleted.
	TicketsRedacted int64 `json:"ticketsRedacted"`
	TicketEvents    int64 `json:"ticketEventsRedacted"`
}

func (r Report) Empty() bool {
	return r.ChatMemory == 0 && r.Turns == 0 && r.Conversations == 0 &&
		r.Sessions == 0 && r.TicketsRedacted == 0 && r.TicketEvents == 0
}

func (r Report) String() string {
	return fmt.Sprintf("memory=%d turns=%d conversations=%d sessions=%d tickets_redacted=%d events_redacted=%d",
		r.ChatMemory, r.Turns, r.Conversations, r.Sessions, r.TicketsRedacted, r.TicketEvents)
}

// Sweep deletes conversation data older than the window.
//
// It does not touch tickets. A ticket ages on its own lifecycle -- an OPEN one is work
// somebody still owes a customer -- and expiring it because the conversation that raised
// it got old would delete the obligation along with the record of it. Retention for
// tickets is a separate decision with a different owner, and pretending otherwise by
// folding it in here would make this function quietly wrong.
func (s *Store) Sweep(ctx context.Context, window time.Duration) (Report, error) {
	if window <= 0 {
		return Report{}, nil
	}
	cutoff := time.Now().Add(-window)

	var report Report
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx)

	// turn first: it is the row that says a conversation existed, and deleting it
	// cascades to turn_passage and turn_tool_call.
	tag, err := tx.Exec(ctx, `DELETE FROM turn WHERE started_at < $1`, cutoff)
	if err != nil {
		return Report{}, fmt.Errorf("sweep turns: %w", err)
	}
	report.Turns = tag.RowsAffected()

	// chat_memory has no conversation-level timestamp of its own beyond each message's,
	// so it is swept by message age. A conversation that is still active keeps its recent
	// messages and loses only the ones past the window, which is the behaviour the
	// windowed memory already has.
	tag, err = tx.Exec(ctx, `DELETE FROM chat_memory WHERE created_at < $1`, cutoff)
	if err != nil {
		return Report{}, fmt.Errorf("sweep memory: %w", err)
	}
	report.ChatMemory = tag.RowsAffected()

	// An owner row for a conversation with nothing left in it is a name for nothing. It
	// is removed only when both are true -- old, and empty -- so an old conversation that
	// still has a ticket pointing at it keeps its attribution.
	tag, err = tx.Exec(ctx, `
		DELETE FROM conversation_owner o
		WHERE o.created_at < $1
		  AND NOT EXISTS (SELECT 1 FROM turn t WHERE t.conversation_id = o.conversation_id)
		  AND NOT EXISTS (SELECT 1 FROM support_ticket s WHERE s.conversation_id = o.conversation_id)`,
		cutoff)
	if err != nil {
		return Report{}, fmt.Errorf("sweep owners: %w", err)
	}
	report.Conversations = tag.RowsAffected()

	// Expired sessions. Kept for a grace period past expiry so that a session which
	// expires mid-request is refused rather than unrecognised.
	tag, err = tx.Exec(ctx, `DELETE FROM chat_session WHERE expires_at < $1`, cutoff)
	if err != nil {
		return Report{}, fmt.Errorf("sweep sessions: %w", err)
	}
	report.Sessions = tag.RowsAffected()

	return report, tx.Commit(ctx)
}

// EraseConversation removes what a customer said in one conversation.
//
// Everything happens in one transaction: a half-erased customer is worse than an
// un-erased one, because nothing afterwards will tell you which half.
func (s *Store) EraseConversation(ctx context.Context, conversationID string) (Report, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx)

	report, err := eraseIn(ctx, tx, []string{conversationID})
	if err != nil {
		return Report{}, err
	}
	return report, tx.Commit(ctx)
}

// EraseSubject erases every conversation a subject owns, and the subject's sessions.
//
// Returns the conversation ids as well as the counts, because the audit entry this
// produces has to name what was erased -- "erased a subject" records that something
// happened and not what, which is the failure mode the audit trail exists to avoid.
func (s *Store) EraseSubject(ctx context.Context, subject string) (Report, []string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Report{}, nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT conversation_id FROM conversation_owner WHERE subject = $1`, subject)
	if err != nil {
		return Report{}, nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Report{}, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, nil, err
	}

	report, err := eraseIn(ctx, tx, ids)
	if err != nil {
		return Report{}, nil, err
	}

	// The sessions go too. Leaving them would let the subject keep talking under an
	// identity they asked to have removed.
	tag, err := tx.Exec(ctx, `DELETE FROM chat_session WHERE subject = $1`, subject)
	if err != nil {
		return Report{}, nil, fmt.Errorf("erase sessions: %w", err)
	}
	report.Sessions += tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return Report{}, nil, err
	}
	return report, ids, nil
}

func eraseIn(ctx context.Context, tx pgx.Tx, conversationIDs []string) (Report, error) {
	var report Report
	if len(conversationIDs) == 0 {
		return report, nil
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM chat_memory WHERE conversation_id = ANY($1)`, conversationIDs)
	if err != nil {
		return Report{}, fmt.Errorf("erase memory: %w", err)
	}
	report.ChatMemory = tag.RowsAffected()

	// Cascades to turn_passage and turn_tool_call. The passages are corpus text rather
	// than the customer's, but which passages were retrieved for which question is itself
	// a statement about what the customer asked.
	tag, err = tx.Exec(ctx,
		`DELETE FROM turn WHERE conversation_id = ANY($1)`, conversationIDs)
	if err != nil {
		return Report{}, fmt.Errorf("erase turns: %w", err)
	}
	report.Turns = tag.RowsAffected()

	// Tickets are redacted, not deleted: see the package comment. order_number goes with
	// the text, because an order number identifies a person as well as a parcel.
	tag, err = tx.Exec(ctx, `
		UPDATE support_ticket
		SET summary = $2, order_number = NULL, resolution = CASE WHEN resolution IS NULL THEN NULL ELSE $2 END,
		    updated_at = now()
		WHERE conversation_id = ANY($1) AND summary <> $2`, conversationIDs, Redacted)
	if err != nil {
		return Report{}, fmt.Errorf("redact tickets: %w", err)
	}
	report.TicketsRedacted = tag.RowsAffected()

	// The event's actor, action and timestamp survive; only its detail is redacted, and
	// only where there is one. Who did what and when is the ticket's history, and erasing
	// that would be erasing an operator's record rather than a customer's words.
	tag, err = tx.Exec(ctx, `
		UPDATE ticket_event e
		SET detail = $2
		FROM support_ticket s
		WHERE e.ticket_number = s.ticket_number
		  AND s.conversation_id = ANY($1)
		  AND e.detail IS NOT NULL AND e.detail <> '' AND e.detail <> $2`,
		conversationIDs, Redacted)
	if err != nil {
		return Report{}, fmt.Errorf("redact ticket events: %w", err)
	}
	report.TicketEvents = tag.RowsAffected()

	// The owner row goes last. It is what maps a subject to a conversation, so keeping it
	// after erasing the conversation would keep the one fact the erasure was about.
	tag, err = tx.Exec(ctx,
		`DELETE FROM conversation_owner WHERE conversation_id = ANY($1)`, conversationIDs)
	if err != nil {
		return Report{}, fmt.Errorf("erase owners: %w", err)
	}
	report.Conversations = tag.RowsAffected()

	return report, nil
}
