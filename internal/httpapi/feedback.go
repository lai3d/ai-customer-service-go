package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/feedback"
)

// Verdicts is the customer half of the feedback loop: one rating, on one of their own
// turns. Optional -- nil means the endpoint is not registered.
//
// An interface rather than *feedback.Store for the same reason Turner is one: the edge
// here is ownership and refusal, and it is worth being able to test that against a stub
// that cannot possibly be answering from a row it also wrote.
type Verdicts interface {
	ConversationOf(ctx context.Context, turnID string) (string, error)
	Record(ctx context.Context, turnID string, source feedback.Source,
		verdict feedback.Verdict, note, actor string) error
}

// handleFeedback records a customer's verdict on a turn in their own conversation.
//
// The operator endpoint next door (`POST /api/admin/v1/turns/{id}/feedback`) does the
// same write with a different source and no ownership check at all, because an operator
// is allowed to judge any answer. This one exists separately rather than sharing a
// handler with a role flag, because everything interesting about it is the check the
// other one does not do.
//
// Sources are not averaged, and the two say different things: a customer knows whether
// they were helped and nothing about whether the answer was correct. What that buys is
// coverage -- operators read a sample, customers read everything.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if s.identity == nil || s.verdicts == nil {
		// With AUTH_MODE=off there is no subject, so there is nothing to scope a rating
		// to: anybody could rate any turn by guessing an id, and the ratings would all be
		// from the same nobody. The endpoint does not exist rather than existing
		// unscoped, on the same rule as the transcript.
		writeProblem(w, &problem{Title: "Feedback is not enabled", Status: http.StatusNotFound,
			Detail: "This service is running without sessions."})
		return
	}
	subject, p := s.resolve(r)
	if p != nil {
		writeProblem(w, p)
		return
	}
	// Its own bucket at the turn limit. A rating costs a row rather than a model call, so
	// it is not the turn limiter's business -- but it is a write reachable with nothing
	// but a session, and an endpoint like that with no ceiling is a table anybody can
	// grow. Sharing the number avoids a configuration knob whose right value nobody
	// knows; sharing the *bucket* would let ratings spend the customer's turns, which is
	// the one thing a rating must never cost.
	if s.identity.Limits != nil {
		if p := s.allow(r, "feedback", subject.ID,
			s.identity.Limits.TurnsPerMinute, time.Minute); p != nil {
			writeProblem(w, p)
			return
		}
	}

	var body struct {
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeProblem(w, &problem{Title: "Malformed request", Status: http.StatusBadRequest,
			Detail: "The request body is not valid JSON."})
		return
	}

	turnID := r.PathValue("id")

	// A turn that is not yours and a turn that does not exist get the same 404, for the
	// reason conversationFor gives: an id that can be probed for existence is most of
	// what this is protecting. Turn ids are server-issued uuids and conversation ids are
	// too, so the only way to learn one is to have been the customer who asked.
	conversationID, err := s.verdicts.ConversationOf(r.Context(), turnID)
	switch {
	case errors.Is(err, feedback.ErrNoSuchTurn):
		s.metrics.Refusals.WithLabelValues("not_yours").Inc()
		writeProblem(w, notYourTurn())
		return
	case err != nil:
		writeProblem(w, &problem{Title: "Could not check the turn",
			Status: http.StatusServiceUnavailable, Detail: "Retrying shortly is worthwhile."})
		return
	}
	// conversationFor counts the refusal and answers 404 itself; a turn in a conversation
	// this subject does not own is exactly the case it was written for.
	if _, p := s.conversationFor(r.Context(), conversationID, subject); p != nil {
		writeProblem(w, p)
		return
	}

	// The subject id, not a name: this is the only identity a customer has, and it is
	// already what owns the conversation. It is a column and never a metric label.
	switch err := s.verdicts.Record(r.Context(), turnID, feedback.SourceCustomer,
		feedback.Verdict(body.Verdict), body.Note, subject.ID); {
	case errors.Is(err, feedback.ErrBadVerdict):
		writeProblem(w, &problem{Title: "Unknown verdict", Status: http.StatusUnprocessableEntity,
			Detail: "The verdict must be helpful, wrong or unclear."})
		return
	case errors.Is(err, feedback.ErrNoSuchTurn):
		// The turn was swept between the ownership check and the write. A race, not a
		// fault, and the same answer as a turn that was never there.
		writeProblem(w, notYourTurn())
		return
	case err != nil:
		writeProblem(w, &problem{Title: "Could not record the feedback",
			Status: http.StatusServiceUnavailable, Detail: "Retrying shortly is worthwhile."})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func notYourTurn() *problem {
	return &problem{Title: "No such turn", Status: http.StatusNotFound,
		Detail: "There is no turn with that id in this session."}
}
