// Package llm is the boundary between this service and whichever model provider is
// configured. Everything above it -- memory, retrieval, the tool loop, accounting --
// is written against these types.
package llm

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn as the provider sees it.
//
// Native carries the provider's own representation of an assistant turn. Claude wants
// the thinking blocks it produced echoed back unchanged when a tool result continues
// the same turn, and reconstructing them from Text would silently drop them. Nothing
// above this package reads it, and nothing persists it: it lives for one turn.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
	Native      any
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// ToolDefinition is what the model reads when deciding whether to call a tool.
// The description is prompt, not documentation.
type ToolDefinition struct {
	Name        string
	Description string
	Schema      map[string]any
}

type Request struct {
	System    string
	Messages  []Message
	Tools     []ToolDefinition
	MaxTokens int
}

// Usage is what one model call cost.
//
// Deliberately per call, not per turn. A tool-calling turn makes at least two calls --
// one asking for the tool, one answering with its result -- and each is billed. The
// Java implementation got this wrong twice because Spring AI handed it a flat stream of
// usage frames with no call boundary in it, so it had to infer the boundary from the
// numbers: keeping the last frame under-reported, summing distinct frames
// over-reported, and the rule that finally worked grouped frames by input count.
//
// None of that is needed here, because this loop owns the boundary. Stream returns
// exactly one call's usage and the caller adds them up. Whether that difference is real
// or whether some provider still forces the heuristic is checked against live providers
// by TestUsageAccountingAcrossProviders; see docs/reliability.md.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
	}
}

func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Result is the outcome of one model call.
type Result struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	Usage      Usage
	// Model is what the provider says it ran, which is not always what was asked for:
	// requesting "gpt-5" yields "gpt-5-2025-08-07". Metrics and prices key on this,
	// because a price table keyed on the requested id silently never matches.
	Model  string
	Native any
}

// WantsTools reports whether the model asked for a tool and is waiting for results.
func (r Result) WantsTools() bool { return len(r.ToolCalls) > 0 }

// Client is one provider. Stream makes exactly one model call, forwarding text as it
// arrives and returning what the call produced and what it cost.
//
// Sampling parameters are absent from Request on purpose, for every provider. Claude
// Opus 5 returns HTTP 400 for temperature, top_p or top_k; GPT-5 accepts only its own
// default. There is no field here to set one by accident.
type Client interface {
	Provider() string
	Model() string
	Stream(ctx context.Context, req Request, onText func(string) error) (Result, error)
}
