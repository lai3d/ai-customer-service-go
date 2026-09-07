// Package feedback is what comes back: a customer saying they were not helped, an operator
// saying an answer was wrong, and the queue those two make.
//
// The queue is the point. A rating widget that nobody reads is a suggestion box, and this
// service already knows what it said, what it retrieved and what it cost — so a marked-wrong
// answer is not a complaint, it is a fully specified piece of work: an eval case that would
// have caught it, or a knowledge entry that should have answered it.
//
// Two sources, recorded separately and deliberately not averaged. A customer knows whether
// they were helped and nothing about whether the answer was correct; an operator knows the
// opposite. Collapsing them into one score would lose exactly the distinction that decides
// what to do about it.
package feedback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Source string

const (
	SourceCustomer Source = "customer"
	SourceOperator Source = "operator"
)

type Verdict string

const (
	VerdictHelpful Verdict = "helpful"
	VerdictWrong   Verdict = "wrong"
	VerdictUnclear Verdict = "unclear"
)

// MaxNoteLength bounds free text that an operator reads and a model never does.
const MaxNoteLength = 2000

var (
	ErrNoSuchTurn = errors.New("no such turn")
	ErrBadVerdict = errors.New("verdict must be helpful, wrong or unclear")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Record stores one verdict, replacing that source's previous one for the same turn.
//
// Upsert rather than insert: somebody changing their mind is not a second opinion, and a
// table with both would count one person twice.
func (s *Store) Record(ctx context.Context, turnID string, source Source, verdict Verdict,
	note, actor string) error {

	switch verdict {
	case VerdictHelpful, VerdictWrong, VerdictUnclear:
	default:
		return ErrBadVerdict
	}
	if len(note) > MaxNoteLength {
		return fmt.Errorf("the note is %d characters; the limit is %d", len(note), MaxNoteLength)
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("feedback needs an author")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO turn_feedback (turn_id, source, verdict, note, actor)
		VALUES ($1,$2,$3,NULLIF($4,''),$5)
		ON CONFLICT (turn_id, source) DO UPDATE SET
			verdict = EXCLUDED.verdict, note = EXCLUDED.note,
			actor = EXCLUDED.actor, at = now(),
			-- A changed verdict is unhandled again: the work it implies is new work.
			handled_at = NULL, handled_by = NULL`,
		turnID, source, verdict, note, actor)
	if err != nil {
		// The only realistic constraint failure is the turn not existing, and a customer
		// rating a turn that was swept is an ordinary race rather than an error worth
		// paging about.
		if strings.Contains(err.Error(), "turn_feedback_turn_id_fkey") {
			return ErrNoSuchTurn
		}
		return err
	}
	return nil
}

// ConversationOf returns the conversation a turn belongs to.
//
// It exists for the customer-facing endpoint, which has to answer a question the operator
// one never asks: is this turn *yours*. A customer sends a turn id, and without this the
// only thing the service could check is that the turn exists — which would let anybody
// with a session rate, and read the existence of, every turn in the database.
//
// ErrNoSuchTurn rather than an empty string, so a caller cannot mistake "no such turn" for
// "a turn in no conversation" and pass an empty id into an ownership check that would then
// be comparing nothing to nothing.
func (s *Store) ConversationOf(ctx context.Context, turnID string) (string, error) {
	var conversationID string
	err := s.pool.QueryRow(ctx,
		`SELECT conversation_id FROM turn WHERE id = $1`, turnID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSuchTurn
	}
	return conversationID, err
}

// Item is one piece of unhandled feedback with everything needed to act on it. The
// question, the answer and the retrieved entries are what turn a complaint into an eval
// case or a knowledge edit without anybody going and looking them up.
type Item struct {
	TurnID         string    `json:"turnId"`
	ConversationID string    `json:"conversationId"`
	Source         Source    `json:"source"`
	Verdict        Verdict   `json:"verdict"`
	Note           string    `json:"note,omitempty"`
	Actor          string    `json:"actor"`
	At             time.Time `json:"at"`
	Question       string    `json:"question"`
	Answer         string    `json:"answer,omitempty"`
	Model          string    `json:"model,omitempty"`
	Outcome        string    `json:"outcome"`
	// Entries retrieved for that turn: what the assistant was answering from, which is
	// where a wrong answer usually starts.
	Entries   []string   `json:"entries,omitempty"`
	Handled   bool       `json:"handled"`
	HandledAt *time.Time `json:"handledAt,omitempty"`
}

// Queue returns feedback that says something went wrong and has not been dealt with.
//
// `helpful` is recorded and not queued: it is worth counting and is not work.
func (s *Store) Queue(ctx context.Context, includeHandled bool, limit int) ([]Item, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT f.turn_id, t.conversation_id, f.source, f.verdict, coalesce(f.note,''),
		       f.actor, f.at, t.question, coalesce(t.reply,''), coalesce(t.model,''),
		       t.outcome, f.handled_at,
		       coalesce(array_agg(p.entry_id ORDER BY p.rank)
		                FILTER (WHERE p.entry_id IS NOT NULL), '{}')
		FROM turn_feedback f
		JOIN turn t ON t.id = f.turn_id
		LEFT JOIN turn_passage p ON p.turn_id = f.turn_id
		WHERE f.verdict <> 'helpful' AND ($1 OR f.handled_at IS NULL)
		GROUP BY f.turn_id, t.conversation_id, f.source, f.verdict, f.note, f.actor, f.at,
		         t.question, t.reply, t.model, t.outcome, f.handled_at
		ORDER BY f.at DESC
		LIMIT $2`, includeHandled, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var i Item
		if err := rows.Scan(&i.TurnID, &i.ConversationID, &i.Source, &i.Verdict, &i.Note,
			&i.Actor, &i.At, &i.Question, &i.Answer, &i.Model, &i.Outcome, &i.HandledAt,
			&i.Entries); err != nil {
			return nil, err
		}
		i.Handled = i.HandledAt != nil
		out = append(out, i)
	}
	return out, rows.Err()
}

// Handle marks one item dealt with — an eval case written, a knowledge entry edited, or a
// decision that nothing was wrong. Which of those it was goes in the audit trail, not here:
// this table records that the queue moved, and the trail records why.
func (s *Store) Handle(ctx context.Context, turnID string, source Source, actor string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE turn_feedback SET handled_at = now(), handled_by = $3
		WHERE turn_id = $1 AND source = $2 AND handled_at IS NULL`, turnID, source, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchTurn
	}
	return nil
}

// Counts is what the overview reads: how much came back, and how much of it is still work.
type Counts struct {
	Helpful   int `json:"helpful"`
	Wrong     int `json:"wrong"`
	Unclear   int `json:"unclear"`
	Unhandled int `json:"unhandled"`
}

func (s *Store) Counts(ctx context.Context, window time.Duration) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE verdict = 'helpful'),
			count(*) FILTER (WHERE verdict = 'wrong'),
			count(*) FILTER (WHERE verdict = 'unclear'),
			count(*) FILTER (WHERE verdict <> 'helpful' AND handled_at IS NULL)
		FROM turn_feedback WHERE at > now() - $1::interval`, window.String()).
		Scan(&c.Helpful, &c.Wrong, &c.Unclear, &c.Unhandled)
	return c, err
}
