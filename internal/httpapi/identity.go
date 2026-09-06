package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/handoff"
	"github.com/lai3d/ai-customer-service-go/internal/identity"
)

// Identity is what the edge needs to know who a request is from and whether the
// conversation is theirs. Nil means AUTH_MODE=off: no sessions, client-supplied ids, no
// ownership -- the pre-identity behaviour, kept for the benchmark and the cross-repository
// comparison, and refused by a production configuration.
type Identity struct {
	Sessions      *identity.Sessions
	Conversations *identity.Conversations
	Limits        *identity.Limits
}

// Transcripts lets a customer read their own conversation, which is how a human's reply
// reaches them. Optional: nil means the endpoint is not registered.
type Transcripts interface {
	Transcript(ctx context.Context, conversationID string) ([]handoff.Message, error)
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

// admit applies the two ceilings a turn has to pass: how often this subject may ask, and
// whether the service has anything left to spend today.
//
// The daily budget is checked here rather than inside the turn because it is not the
// customer's fault and not fixed by rephrasing: it is the service saying no. 503 with a
// Retry-After to the end of the day is the honest shape -- 429 would tell the customer to
// slow down, which changes nothing.
func (s *Server) admit(r *http.Request, subject identity.Subject) *problem {
	if s.identity == nil || s.identity.Limits == nil {
		return nil
	}
	if p := s.allow(r, "turn", subject.ID,
		s.identity.Limits.TurnsPerMinute, time.Minute); p != nil {
		return p
	}
	if err := s.identity.Limits.CheckDailyBudget(r.Context()); err != nil {
		if errors.Is(err, identity.ErrBudgetExhausted) {
			slog.Warn("the daily token budget is exhausted; refusing new turns")
			return &problem{Title: "The service has reached its budget for today",
				Status:     http.StatusServiceUnavailable,
				Detail:     "A human agent can take it from here.",
				RetryAfter: time.Until(endOfUTCDay())}
		}
		// Same reasoning as the limiter: a budget that cannot be read must not become an
		// outage on its own.
		slog.Warn("could not read the daily budget; allowing the turn", "error", err)
	}
	return nil
}

// charge adds a finished turn's tokens to the day's total.
//
// On a detached context and after the response, for the reason the turn record already
// works this way: a customer who closed the tab still spent the money, and a spend that is
// only recorded when the client is still listening is a budget that under-counts exactly
// the traffic worth counting.
func (s *Server) charge(usage *chat.UsageEvent) {
	if s.identity == nil || s.identity.Limits == nil || usage == nil {
		return
	}
	total := usage.InputTokens + usage.OutputTokens
	if total <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := s.identity.Limits.RecordSpend(ctx, total); err != nil {
		// Logged, not returned: the tokens are already spent and failing the response
		// would not unspend them. A budget that silently stops counting is what the
		// alerting item on the readiness list is for.
		slog.Error("could not record the day's spend", "tokens", total, "error", err)
	}
}

func endOfUTCDay() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
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

// allow applies a limit and turns a refusal into the response the client should see.
//
// Retry-After is set on both, and it is not decoration: without it a client backs off by
// guessing, and the guesses that get written are "immediately" and "a minute", neither of
// which is the answer.
func (s *Server) allow(r *http.Request, bucket, key string, limit int, window time.Duration) *problem {
	if s.identity == nil || s.identity.Limits == nil {
		return nil
	}
	retryAfter, err := s.identity.Limits.Allow(r.Context(), bucket, key, limit, window)
	switch {
	case errors.Is(err, identity.ErrTooManyRequests):
		return &problem{Title: "Too many requests", Status: http.StatusTooManyRequests,
			Detail:     "This session is asking faster than this service answers. Try again shortly.",
			RetryAfter: retryAfter}
	case err != nil:
		// A limiter that cannot reach its counter must not become an outage. It fails
		// open and says so: the alternative is that a database blip stops every customer,
		// which is a worse failure than a minute of unbounded requests.
		slog.Warn("the rate limiter could not read its counter; allowing the request",
			"bucket", bucket, "error", err)
		return nil
	}
	return nil
}

// clientIP is the best available identifier for "who is minting sessions". Behind a proxy
// it is the proxy unless X-Forwarded-For is trusted, and trusting that header from an
// untrusted network is how a limit becomes a header a client sets. It is deliberately
// RemoteAddr only, and the deployment note says what to do about it.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
	// This is the one endpoint reachable without a session, so it is the one that mints
	// subjects -- and a per-subject limit is worth nothing if subjects are free.
	if s.identity.Limits != nil {
		if p := s.allow(r, "session", clientIP(r),
			s.identity.Limits.SessionsPerHourPerIP, time.Hour); p != nil {
			writeProblem(w, p)
			return
		}
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

// handleTranscript returns the customer's own conversation.
//
// This is the last step of the handoff and the reason the operator's reply is written into
// chat_memory rather than only onto the ticket: without somewhere for the customer to read
// it, a human's answer is a row in a database that the person who asked will never see.
//
// It is session-scoped like everything else. With AUTH_MODE=off there is no subject to
// scope it to, so the endpoint does not exist -- a transcript endpoint that anyone could
// read by guessing a conversation id would be the confidentiality hole this service just
// closed, reopened for convenience.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if s.identity == nil || s.transcripts == nil {
		writeProblem(w, &problem{Title: "Transcripts are not enabled", Status: http.StatusNotFound,
			Detail: "This service is running without sessions."})
		return
	}
	subject, p := s.resolve(r)
	if p != nil {
		writeProblem(w, p)
		return
	}
	id := r.PathValue("id")
	if _, p := s.conversationFor(r.Context(), id, subject); p != nil {
		writeProblem(w, p)
		return
	}
	messages, err := s.transcripts.Transcript(r.Context(), id)
	if err != nil {
		writeProblem(w, &problem{Title: "Could not read the conversation",
			Status: http.StatusServiceUnavailable, Detail: "Retrying shortly is worthwhile."})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"conversationId": id, "messages": messages,
	}); err != nil {
		slog.Error("could not write the transcript", "error", err)
	}
}
