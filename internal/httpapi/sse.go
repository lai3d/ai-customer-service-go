package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
)

// handleStream is the streaming endpoint.
//
// The turn runs in its own goroutine and events arrive on a channel, so the heartbeat
// can interleave with them. That the upstream is consumed exactly once is a property of
// the channel rather than something to assert: the Java implementation merged a
// heartbeat into a reactive stream, where subscribing twice would have run the entire
// turn twice -- two model calls, two bills, two sets of messages written to memory --
// while the response still looked correct.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
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
	// Both of these must happen before a single byte of the stream is written. Once the
	// 200 and the event-stream headers are out, an authorisation failure can only be
	// reported as an error *event*, which a client is far more likely to render as a
	// chat message than as a refusal.
	subject, p := s.resolve(r)
	if p != nil {
		writeProblem(w, p)
		return
	}
	id, p := s.conversationFor(r.Context(), req.ConversationID, subject)
	if p != nil {
		writeProblem(w, p)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set(ConversationIDHeader, id)
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	_ = controller.Flush()

	events := make(chan sseEvent, 64)
	go func() {
		defer close(events)
		err := s.chat.Turn(r.Context(), id, message, func(e chat.Event) {
			select {
			case events <- sseEvent{name: string(e.Type), payload: e}:
			case <-r.Context().Done():
			}
		})
		if err != nil {
			// A failure after the response is committed cannot change the status
			// code, so it arrives as a terminal error event. A client never has to
			// guess whether an apology came from the model or from the transport.
			select {
			case events <- sseEvent{name: "error", payload: problemFor(err)}:
			case <-r.Context().Done():
			}
		}
	}()

	// SSE connections are legitimately idle between the request and the first token --
	// retrieval plus a slow model is several seconds -- and proxies close idle
	// connections. A comment-only frame is invisible to any correct client.
	// Clamped rather than trusted. time.NewTicker panics on a non-positive interval, and
	// a panic in a handler takes the connection down mid-response and logs a stack trace
	// where a wrong heartbeat would have logged nothing -- a much worse failure than the
	// misconfiguration it reports. Config.Load refuses the value too, so this is the
	// second line of defence for a Server built by something other than main.
	interval := s.cfg.KeepAliveInterval
	if interval <= 0 {
		interval = defaultKeepAlive
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client is gone. Nothing can be sent, and the turn's own deferred
			// persistence has already taken care of the partial reply.
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			_ = controller.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeEvent(w, event); err != nil {
				slog.Debug("could not write an SSE event", "error", err)
				return
			}
			_ = controller.Flush()
		}
	}
}

const defaultKeepAlive = 15 * time.Second

type sseEvent struct {
	name    string
	payload any
}

func writeEvent(w http.ResponseWriter, e sseEvent) error {
	encoded, err := json.Marshal(e.payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, encoded)
	return err
}
