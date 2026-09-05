package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/lai3d/ai-customer-service-go/internal/identity"
)

// Identity is what the edge needs to know who a request is from and whether the
// conversation is theirs. Nil means AUTH_MODE=off: no sessions, client-supplied ids, no
// ownership -- the pre-identity behaviour, kept for the benchmark and the cross-repository
// comparison, and refused by a production configuration.
type Identity struct {
	Sessions      *identity.Sessions
	Conversations *identity.Conversations
}

// resolve returns the subject for a request, or the problem to send.
//
// The two failures are deliberately the same shape as each other and different from a
// server error: a missing session and an expired one are both "sign in again", and the
// page acts on that without needing to tell them apart.
func (s *Server) resolve(r *http.Request) (identity.Subject, *problem) {
	if s.identity == nil {
		return identity.Subject{}, nil
	}
	subject, err := s.identity.Sessions.Verify(r.Context(), identity.BearerToken(r))
	switch {
	case errors.Is(err, identity.ErrNoSession):
		return identity.Subject{}, &problem{
			Title:  "No session",
			Status: http.StatusUnauthorized,
			Detail: "Start a session at POST /api/v1/session and send its token as a bearer token.",
		}
	case err != nil:
		return identity.Subject{}, &problem{
			Title: "Session lookup failed", Status: http.StatusServiceUnavailable,
			Detail: "The service could not check the session. Retrying shortly is worthwhile.",
		}
	}
	return subject, nil
}

// conversationFor decides which conversation this turn belongs to and refuses one that is
// not the subject's.
//
// A conversation owned by somebody else and a conversation that does not exist get the
// *same* 404. A 403 would confirm the id exists, and an id that can be probed for
// existence is most of what this is protecting -- the ids were guessable enough to be
// worth enumerating when they were whatever the client sent.
func (s *Server) conversationFor(ctx context.Context, supplied string, subject identity.Subject) (string, *problem) {
	if s.identity == nil {
		return conversationID(supplied), nil
	}

	notFound := &problem{
		Title: "No such conversation", Status: http.StatusNotFound,
		Detail: "There is no conversation with that id in this session.",
	}

	if supplied == "" {
		// Server-issued, so it is not a value a client can choose and therefore not a
		// value a client can collide with on purpose.
		id := uuid.NewString()
		if err := s.identity.Conversations.Claim(ctx, id, subject); err != nil {
			return "", &problem{Title: "Could not start a conversation",
				Status: http.StatusServiceUnavailable,
				Detail: "The service could not record the conversation. Retrying shortly is worthwhile."}
		}
		return id, nil
	}

	switch err := s.identity.Conversations.Owns(ctx, supplied, subject); {
	case err == nil:
		return supplied, nil
	case errors.Is(err, identity.ErrNotYours), errors.Is(err, identity.ErrNoSuchConv):
		return "", notFound
	default:
		return "", &problem{Title: "Could not check the conversation",
			Status: http.StatusServiceUnavailable,
			Detail: "The service could not check the conversation. Retrying shortly is worthwhile."}
	}
}

type sessionReply struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// handleSession issues a session. It is the one endpoint that is reachable without one.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.identity == nil {
		writeProblem(w, &problem{Title: "Sessions are not enabled", Status: http.StatusNotFound,
			Detail: "This service is running with AUTH_MODE=off."})
		return
	}
	token, _, expires, err := s.identity.Sessions.Issue(r.Context())
	if err != nil {
		writeProblem(w, &problem{Title: "Could not start a session",
			Status: http.StatusServiceUnavailable,
			Detail: "Retrying shortly is worthwhile."})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Never cached, anywhere, by anything: it is a credential in a response body.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(sessionReply{Token: token, ExpiresAt: expires}); err != nil {
		slog.Error("could not write the session response", "error", err)
	}
}
