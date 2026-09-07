package chat

import "github.com/lai3d/ai-customer-service-go/internal/rag"

// A turn emits typed events rather than bare tokens.
//
// A chat widget reads Message and Error and ignores the rest. Everything else is there
// because the interesting part of this system is the part a widget hides: which
// passages retrieval found and how well they scored, which tools ran and what they
// decided, and what the turn cost.
type EventType string

const (
	EventRetrieval EventType = "retrieval"
	EventTool      EventType = "tool"
	EventMessage   EventType = "message"
	EventUsage     EventType = "usage"
	EventError     EventType = "error"
)

type Event struct {
	Type EventType `json:"type"`

	// Retrieval is emitted before the model is called, so it arrives while the model
	// is still thinking and survives a failed model call -- which is exactly when
	// someone debugging a bad answer most needs to see it.
	Passages []Passage `json:"passages,omitempty"`

	Tool *ToolEvent `json:"tool,omitempty"`

	Text string `json:"text,omitempty"`

	Usage *UsageEvent `json:"usage,omitempty"`

	// Message is customer-safe text. Internal detail is logged, never sent.
	Message string `json:"message,omitempty"`
}

type Passage struct {
	EntryID  string  `json:"entryId"`
	Language string  `json:"language"`
	Score    float64 `json:"score"`
	Question string  `json:"question"`
}

type ToolEvent struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

type UsageEvent struct {
	// TurnID is the operational record this turn was written to. It is here so a
	// customer can say the answer was wrong: a rating needs something to point at, and
	// the turn is the only thing that already holds the question, the reply, the model
	// and the passages it was answered from.
	//
	// It is safe to hand out because it is a server-issued uuid and the endpoint that
	// takes it checks that the turn is in a conversation the session owns -- the id is
	// not the authorisation.
	TurnID string `json:"turnId,omitempty"`

	Model string `json:"model"`
	// ModelCalls is why this is not called "the model call". A tool-calling turn makes
	// at least two, and each is billed.
	ModelCalls   int     `json:"modelCalls"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd,omitempty"`
	Millis       int64   `json:"millis"`
	TraceID      string  `json:"traceId,omitempty"`
}

func toPassageEvents(passages []rag.Passage) []Passage {
	out := make([]Passage, 0, len(passages))
	for _, p := range passages {
		out = append(out, Passage{
			EntryID:  p.EntryID,
			Language: p.Language,
			Score:    p.Score,
			Question: p.Question,
		})
	}
	return out
}
