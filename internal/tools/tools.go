// Package tools holds the actions the model can take.
//
// Ticket creation is real: it writes to Postgres, and the dedupe and the cap are
// guarantees of that schema. Order lookup is a seam -- OrderSource, with an in-memory
// fixture as the default and an HTTP adapter for a real order service -- because there is
// no order system to point it at. Which one is wired is a start-up decision the server
// announces, not something a reader of this package has to work out.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Result is what the model gets back, plus a label for metrics and the stream.
//
// Outcome is not sent to the model. It exists because a tool call is otherwise
// invisible to everything outside the model call -- no metric, no span, nothing for a
// client to display until the assistant happens to mention what it did.
type Result struct {
	Content string
	Outcome string
}

// Tool is one callable action.
//
// The conversation id is a parameter rather than something fished out of an ambient
// context map. In Spring AI it travelled through a ToolContext, which created a
// contract with teeth: a code path that reached the model without populating it broke
// ticket creation, and broke it only once a conversation had escalated far enough for
// the model to try. Here a caller that forgets it does not compile.
type Tool interface {
	Definition() Definition
	Invoke(ctx context.Context, conversationID string, arguments json.RawMessage) (Result, error)
}

// Definition is what the model reads. Descriptions are prompt, not documentation:
// they are the entire basis on which the model decides whether to call a tool instead
// of answering from retrieved text, so they say what a tool is not for as well as what
// it is for.
type Definition struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		// Only reachable if a result struct grows an unmarshalable field.
		return fmt.Sprintf(`{"error":%q}`, "result could not be encoded")
	}
	return string(encoded)
}
