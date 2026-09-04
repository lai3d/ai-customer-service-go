// Package httpapi is the edge: validation, SSE, and turning failures into responses a
// client can act on.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
)

// ConversationIDHeader carries the id of the conversation a response belongs to, so a
// client that omitted one on the first turn knows what to send on the second.
const ConversationIDHeader = "X-Conversation-Id"

type Request struct {
	ConversationID string `json:"conversationId,omitempty"`
	Message        string `json:"message"`
}

type Reply struct {
	ConversationID string           `json:"conversationId"`
	Reply          string           `json:"reply"`
	Passages       []chat.Passage   `json:"passages,omitempty"`
	Tools          []chat.ToolEvent `json:"tools,omitempty"`
	Usage          *chat.UsageEvent `json:"usage,omitempty"`
}

// problem is RFC 9457. A client should be able to tell "try again" from "this will
// never work" without parsing prose.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Turner is the one thing this package needs from the chat service. It is an interface
// here rather than a concrete type so the edge -- validation, status codes, SSE framing
// -- can be tested without a database, a model, or an embedding model.
type Turner interface {
	Turn(ctx context.Context, conversationID, message string, emit func(chat.Event)) error
}

type Server struct {
	chat Turner
	cfg  config.Chat
}

func NewServer(service Turner, cfg config.Chat) *Server {
	return &Server{chat: service, cfg: cfg}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)
	mux.HandleFunc("POST /api/v1/chat/stream", s.handleStream)
}

// validate rejects what should never reach a model call. Both limits cost nothing to
// enforce here and are a 500 from the database if they are not.
func (s *Server) validate(req Request) (string, *problem) {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return "", &problem{Title: "Message required", Status: http.StatusBadRequest,
			Detail: "The message must not be blank."}
	}
	if utf8.RuneCountInString(message) > s.cfg.MaxMessageLength {
		return "", &problem{Title: "Message too long", Status: http.StatusBadRequest,
			Detail: "The message is longer than this service accepts."}
	}
	if len(req.ConversationID) > s.cfg.MaxConversationIDLength {
		// A client-supplied id lands in a bounded column. Unvalidated, this surfaced
		// as a 500 from a constraint violation in the Java implementation.
		return "", &problem{Title: "Conversation id too long", Status: http.StatusBadRequest,
			Detail: "The conversation id is longer than this service accepts."}
	}
	return message, nil
}

func decode(r *http.Request) (Request, *problem) {
	var req Request
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := decoder.Decode(&req); err != nil {
		return Request{}, &problem{Title: "Malformed request", Status: http.StatusBadRequest,
			Detail: "The request body is not valid JSON."}
	}
	return req, nil
}

func conversationID(supplied string) string {
	if supplied != "" {
		return supplied
	}
	return uuid.NewString()
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, p := decode(r)
	if p != nil {
		writeProblem(w, p)
		return
	}
	message, p := s.validate(req)
	if p != nil {
		writeProblem(w, p)
		return
	}
	id := conversationID(req.ConversationID)
	w.Header().Set(ConversationIDHeader, id)

	reply := Reply{ConversationID: id}
	var text strings.Builder
	err := s.chat.Turn(r.Context(), id, message, func(e chat.Event) {
		switch e.Type {
		case chat.EventMessage:
			text.WriteString(e.Text)
		case chat.EventRetrieval:
			reply.Passages = e.Passages
		case chat.EventTool:
			reply.Tools = append(reply.Tools, *e.Tool)
		case chat.EventUsage:
			// Recorded here as well as in the meters. The Java implementation's
			// blocking endpoint threw the response metadata away and with it the token
			// usage, so this path was invisible to both the budget and the cost meters
			// while spending real money.
			reply.Usage = e.Usage
		}
	})
	if err != nil {
		writeProblem(w, problemFor(err))
		return
	}
	reply.Reply = text.String()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		slog.Error("could not write reply", "error", err)
	}
}

// problemFor maps a failure to a response a client can act on: retry, do not retry, or
// this conversation is over.
func problemFor(err error) *problem {
	var exceeded *cost.ErrExceeded
	if errors.As(err, &exceeded) {
		return &problem{
			Title:  "Conversation budget reached",
			Status: http.StatusTooManyRequests,
			Detail: "This conversation has reached its token budget. A human agent can take it from here.",
		}
	}
	var llmErr *llm.Error
	if errors.As(err, &llmErr) {
		if llmErr.Retryable {
			return &problem{Title: "The assistant is temporarily unavailable",
				Status: http.StatusServiceUnavailable,
				Detail: "The model provider is rate limiting or overloaded. Retrying shortly is worthwhile."}
		}
		return &problem{Title: "The assistant could not answer",
			Status: http.StatusBadGateway,
			Detail: "The model provider rejected the request. Retrying will not help."}
	}
	slog.Error("unhandled failure in a chat turn", "error", err)
	return &problem{Title: "Internal error", Status: http.StatusInternalServerError}
}

func writeProblem(w http.ResponseWriter, p *problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.Error("could not write problem response", "error", err)
	}
}
