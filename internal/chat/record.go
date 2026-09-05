package chat

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
)

// Recorder writes the operational history of a turn.
//
// This is not `chat_memory`. That table is the model's context: windowed at 40 messages,
// holding only what the model needs to see next time. An operational record that
// disappears when the window slides is not a record, and it also cannot answer the
// questions an operator asks -- did this turn fail or did the customer close the tab, what
// did retrieval actually return, what did it cost.
//
// It is written at the service boundary rather than from the event stream that feeds the
// browser. The events exist for a page that may not be watching; a turn whose client
// disconnected still has to end up with a terminal outcome.
type Recorder struct{ pool *pgxpool.Pool }

func NewRecorder(pool *pgxpool.Pool) *Recorder { return &Recorder{pool: pool} }

// OutcomeInFlight is the state a turn is in between Begin and Finish. A row left in it
// is a process that died mid-turn, which is a fact worth being able to see rather than
// a row worth hiding.
const OutcomeInFlight = "in_flight"

type TurnRecord struct {
	ID             string
	ConversationID string
	StartedAt      time.Time
	Question       string

	Outcome      string
	Reply        string
	Model        string
	ModelCalls   int
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Priced       bool
	TraceID      string
	Detail       string

	Passages  []rag.Passage
	ToolCalls []ToolEvent
}

// Begin records that a turn started. Its failure is deliberately fatal to the turn: a
// model call this service cannot account for is worse than a turn that did not happen,
// and the alternative is discovering the gap in a month from a bill.
func (r *Recorder) Begin(ctx context.Context, t TurnRecord) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO turn (id, conversation_id, started_at, outcome, question)
		VALUES ($1, $2, $3, $4, $5)`,
		t.ID, t.ConversationID, t.StartedAt, OutcomeInFlight, t.Question)
	if err != nil {
		return fmt.Errorf("record the start of a turn: %w", err)
	}
	return nil
}

// Finish completes the record. It is called from a deferred block on a detached context,
// so it runs whether the turn completed, failed, or had its client disconnect.
//
// Its failure is *not* fatal: by the time it runs the money has been spent and the
// customer has their answer, so failing here would turn a bookkeeping problem into a
// customer-visible one. It is logged loudly by the caller instead.
func (r *Recorder) Finish(ctx context.Context, t TurnRecord) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cost any
	if t.Priced {
		cost = t.CostUSD
	}
	if _, err := tx.Exec(ctx, `
		UPDATE turn SET ended_at = now(), outcome = $2, reply = NULLIF($3,''),
			model = NULLIF($4,''), model_calls = $5, input_tokens = $6,
			output_tokens = $7, cost_usd = $8, trace_id = NULLIF($9,''),
			detail = NULLIF($10,'')
		WHERE id = $1`,
		t.ID, t.Outcome, t.Reply, t.Model, t.ModelCalls, t.InputTokens,
		t.OutputTokens, cost, t.TraceID, t.Detail); err != nil {
		return fmt.Errorf("finish the turn record: %w", err)
	}

	// The passages are kept because the corpus can change, and "why did it answer that"
	// is unanswerable from a corpus that has since moved.
	if len(t.Passages) > 0 {
		rows := make([][]any, len(t.Passages))
		for i, p := range t.Passages {
			rows[i] = []any{t.ID, i, p.EntryID, p.Language, p.Score, p.Question}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"turn_passage"},
			[]string{"turn_id", "rank", "entry_id", "language", "score", "question"},
			pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("record retrieval evidence: %w", err)
		}
	}
	if len(t.ToolCalls) > 0 {
		rows := make([][]any, len(t.ToolCalls))
		for i, c := range t.ToolCalls {
			rows[i] = []any{t.ID, i, c.Name, c.Outcome}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"turn_tool_call"},
			[]string{"turn_id", "seq", "name", "outcome"},
			pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("record tool calls: %w", err)
		}
	}
	return tx.Commit(ctx)
}
