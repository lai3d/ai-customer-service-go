// Package chat runs one customer turn: memory, retrieval, the model, its tools, and
// what all of it cost.
package chat

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
)

// Memory is the conversation history, in Postgres alongside the vectors.
//
// It stores what the customer actually wrote and what the assistant actually replied --
// never the passages retrieval found. That distinction is the ordering constraint the
// Java implementation had to pin with a test: there, retrieval rewrote the user message
// to carry the passages and memory stored whatever message it was handed, so running
// the two the wrong way round wrote every retrieved passage into the customer's history
// and re-sent it on every later turn. The symptom is not a failure; it is a prompt that
// grows quietly and a bill that grows with it.
//
// Here, memory is written before retrieval runs and passages are attached to the
// outgoing request instead. TestRetrievedPassagesNeverEnterMemory holds the line.
type Memory struct {
	pool   *pgxpool.Pool
	window int
}

func NewMemory(pool *pgxpool.Pool, window int) *Memory {
	return &Memory{pool: pool, window: window}
}

func (m *Memory) Append(ctx context.Context, conversationID string, role llm.Role, content string) error {
	if content == "" {
		return nil
	}
	_, err := m.pool.Exec(ctx,
		`INSERT INTO chat_memory (conversation_id, role, content) VALUES ($1, $2, $3)`,
		conversationID, string(role), content)
	if err != nil {
		return fmt.Errorf("append to conversation memory: %w", err)
	}
	return nil
}

// History returns the last `window` messages, oldest first.
//
// Every message is re-sent and re-billed on every turn, so the window is a cost and
// latency lever rather than a memory setting.
func (m *Memory) History(ctx context.Context, conversationID string) ([]llm.Message, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT role, content FROM (
			SELECT id, role, content FROM chat_memory
			WHERE conversation_id = $1
			ORDER BY id DESC
			LIMIT $2
		) recent
		ORDER BY id ASC`, conversationID, m.window)
	if err != nil {
		return nil, fmt.Errorf("read conversation memory: %w", err)
	}
	defer rows.Close()

	var history []llm.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		history = appendMerging(history, llm.Message{Role: llm.Role(role), Text: content})
	}
	return history, rows.Err()
}

// appendMerging joins consecutive messages that share a role.
//
// They happen: a turn whose model call fails after the user message is stored leaves no
// assistant reply behind, so the next turn's history has two user messages in a row.
// Providers differ on whether that is accepted, and the ones that accept it are not
// obliged to keep doing so. Merging is cheaper than a repair job and loses nothing --
// the two messages were consecutive for the customer too.
func appendMerging(history []llm.Message, message llm.Message) []llm.Message {
	if n := len(history); n > 0 && history[n-1].Role == message.Role {
		history[n-1].Text += "\n\n" + message.Text
		return history
	}
	return append(history, message)
}

func (m *Memory) Count(ctx context.Context, conversationID string) (int, error) {
	var n int
	err := m.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_memory WHERE conversation_id = $1`, conversationID).Scan(&n)
	return n, err
}
