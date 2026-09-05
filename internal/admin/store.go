package admin

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Overview struct {
	Since          time.Time      `json:"since"`
	TurnsByOutcome map[string]int `json:"turnsByOutcome"`
	Conversations  int            `json:"conversations"`
	InputTokens    int64          `json:"inputTokens"`
	OutputTokens   int64          `json:"outputTokens"`
	// CostUSD is an estimate and is labelled one everywhere it appears. A turn on a
	// model with no price entry contributes tokens and no cost, which is why
	// UnpricedTurns sits beside it -- a total that silently omits some turns is worse
	// than one that says how many.
	CostUSD       float64        `json:"costUsd"`
	UnpricedTurns int            `json:"unpricedTurns"`
	Tickets       map[string]int `json:"tickets"`
}

func (s *Store) Overview(ctx context.Context, window time.Duration) (Overview, error) {
	since := time.Now().Add(-window)
	o := Overview{Since: since, TurnsByOutcome: map[string]int{}, Tickets: map[string]int{}}

	rows, err := s.pool.Query(ctx,
		`SELECT outcome, count(*) FROM turn WHERE started_at >= $1 GROUP BY outcome`, since)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var outcome string
		var n int
		if err := rows.Scan(&outcome, &n); err != nil {
			rows.Close()
			return o, err
		}
		o.TurnsByOutcome[outcome] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return o, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT conversation_id),
		       coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		       coalesce(sum(cost_usd),0),
		       count(*) FILTER (WHERE cost_usd IS NULL AND model_calls > 0)
		FROM turn WHERE started_at >= $1`, since).
		Scan(&o.Conversations, &o.InputTokens, &o.OutputTokens, &o.CostUSD, &o.UnpricedTurns); err != nil {
		return o, err
	}

	tickets, err := s.pool.Query(ctx, `SELECT state, count(*) FROM support_ticket GROUP BY state`)
	if err != nil {
		return o, err
	}
	defer tickets.Close()
	for tickets.Next() {
		var state string
		var n int
		if err := tickets.Scan(&state, &n); err != nil {
			return o, err
		}
		o.Tickets[state] = n
	}
	return o, tickets.Err()
}

type ConversationSummary struct {
	ConversationID string    `json:"conversationId"`
	Turns          int       `json:"turns"`
	StartedAt      time.Time `json:"startedAt"`
	LastAt         time.Time `json:"lastAt"`
	// Outcomes is every distinct outcome the conversation produced, so a conversation
	// with one failure in twenty turns does not look identical to one that never failed.
	Outcomes     []string `json:"outcomes"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	Tickets      int      `json:"tickets"`
}

type ConversationFilter struct {
	Outcome       string
	Search        string
	Limit, Offset int
}

func (s *Store) Conversations(ctx context.Context, f ConversationFilter) ([]ConversationSummary, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	const where = `
		WHERE ($1 = '' OR t.conversation_id IN
		         (SELECT conversation_id FROM turn WHERE outcome = $1))
		  AND ($2 = '' OR t.conversation_id ILIKE '%' || $2 || '%')`

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT t.conversation_id) FROM turn t`+where,
		f.Outcome, f.Search).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT t.conversation_id, count(*), min(t.started_at), max(t.started_at),
		       array_agg(DISTINCT t.outcome),
		       coalesce(sum(t.input_tokens),0), coalesce(sum(t.output_tokens),0),
		       (SELECT count(*) FROM support_ticket s WHERE s.conversation_id = t.conversation_id)
		FROM turn t`+where+`
		GROUP BY t.conversation_id
		ORDER BY max(t.started_at) DESC
		LIMIT $3 OFFSET $4`, f.Outcome, f.Search, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var c ConversationSummary
		if err := rows.Scan(&c.ConversationID, &c.Turns, &c.StartedAt, &c.LastAt,
			&c.Outcomes, &c.InputTokens, &c.OutputTokens, &c.Tickets); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

type Passage struct {
	EntryID  string  `json:"entryId"`
	Language string  `json:"language"`
	Score    float64 `json:"score"`
	Question string  `json:"question"`
}

type ToolCall struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

type Turn struct {
	ID           string     `json:"id"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	Outcome      string     `json:"outcome"`
	Question     string     `json:"question"`
	Reply        string     `json:"reply,omitempty"`
	Model        string     `json:"model,omitempty"`
	ModelCalls   int        `json:"modelCalls"`
	InputTokens  int64      `json:"inputTokens"`
	OutputTokens int64      `json:"outputTokens"`
	CostUSD      *float64   `json:"costUsd,omitempty"`
	TraceID      string     `json:"traceId,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	Passages     []Passage  `json:"passages,omitempty"`
	ToolCalls    []ToolCall `json:"toolCalls,omitempty"`
}

// Conversation returns every recorded turn, with the evidence for each.
func (s *Store) Conversation(ctx context.Context, id string) ([]Turn, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, started_at, ended_at, outcome, question, coalesce(reply,''),
		       coalesce(model,''), model_calls, input_tokens, output_tokens, cost_usd,
		       coalesce(trace_id,''), coalesce(detail,'')
		FROM turn WHERE conversation_id = $1 ORDER BY started_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []Turn
	byID := map[string]int{}
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ID, &t.StartedAt, &t.EndedAt, &t.Outcome, &t.Question,
			&t.Reply, &t.Model, &t.ModelCalls, &t.InputTokens, &t.OutputTokens,
			&t.CostUSD, &t.TraceID, &t.Detail); err != nil {
			return nil, err
		}
		byID[t.ID] = len(turns)
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(turns))
	for _, t := range turns {
		ids = append(ids, t.ID)
	}

	passages, err := s.pool.Query(ctx, `
		SELECT turn_id, entry_id, language, score, question FROM turn_passage
		WHERE turn_id = ANY($1) ORDER BY turn_id, rank`, ids)
	if err != nil {
		return nil, err
	}
	for passages.Next() {
		var turnID string
		var p Passage
		if err := passages.Scan(&turnID, &p.EntryID, &p.Language, &p.Score, &p.Question); err != nil {
			passages.Close()
			return nil, err
		}
		turns[byID[turnID]].Passages = append(turns[byID[turnID]].Passages, p)
	}
	passages.Close()
	if err := passages.Err(); err != nil {
		return nil, err
	}

	calls, err := s.pool.Query(ctx, `
		SELECT turn_id, name, outcome FROM turn_tool_call
		WHERE turn_id = ANY($1) ORDER BY turn_id, seq`, ids)
	if err != nil {
		return nil, err
	}
	defer calls.Close()
	for calls.Next() {
		var turnID string
		var c ToolCall
		if err := calls.Scan(&turnID, &c.Name, &c.Outcome); err != nil {
			return nil, err
		}
		turns[byID[turnID]].ToolCalls = append(turns[byID[turnID]].ToolCalls, c)
	}
	return turns, calls.Err()
}

type AuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Object  string    `json:"object"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

// Audit records an operator action. It is called for reads of customer content as well
// as for writes: who looked is part of what an audit trail is for, and this is the only
// surface in the service that shows a customer's words to anyone.
//
// There is deliberately no update or delete path to this table anywhere in the codebase.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_audit (actor, action, object, outcome, detail)
		 VALUES ($1,$2,$3,$4,NULLIF($5,''))`,
		e.Actor, e.Action, e.Object, e.Outcome, e.Detail)
	return err
}

func (s *Store) AuditTrail(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT at, actor, action, object, outcome, coalesce(detail,'')
		FROM admin_audit ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Object, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
