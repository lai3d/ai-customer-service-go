package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/retention"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
)

type Server struct {
	store     *Store
	tickets   *ticket.Store
	operators Operators
	cors      CORS
	erasure   *retention.Store
}

func NewServer(store *Store, tickets *ticket.Store, operators Operators, cors CORS,
	erasure *retention.Store) *Server {
	return &Server{store: store, tickets: tickets, operators: operators, cors: cors,
		erasure: erasure}
}

// Routes mounts the operations surface.
//
// The caller must not call this when no operator is configured: the routes then do not
// exist and the paths 404 like any other unknown path. That is the difference between a
// surface that is guarded and a surface that is absent, and only one of them survives a
// mistake in the guard.
func (s *Server) Routes(mux *http.ServeMux) {
	// CORS wraps the outside, authentication the inside. The order is not cosmetic: a
	// preflight carries no Authorization header, so authenticating first rejects every
	// cross-origin request before the browser has even sent the real one.
	read := func(h http.HandlerFunc) http.Handler {
		return s.cors.Wrap(s.operators.Authenticate(h))
	}
	write := func(h http.HandlerFunc) http.Handler {
		return s.cors.Wrap(s.operators.Authenticate(RequireWrite(s.refused, h)))
	}

	mux.Handle("GET /api/admin/v1/overview", read(s.overview))
	mux.Handle("GET /api/admin/v1/conversations", read(s.conversations))
	mux.Handle("GET /api/admin/v1/conversations/{id}", read(s.conversation))
	mux.Handle("GET /api/admin/v1/tickets", read(s.ticketList))
	mux.Handle("GET /api/admin/v1/tickets/{number}", read(s.ticketDetail))
	mux.Handle("PATCH /api/admin/v1/tickets/{number}", write(s.ticketUpdate))
	mux.Handle("DELETE /api/admin/v1/conversations/{id}", write(s.erase))
	mux.Handle("GET /api/admin/v1/audit", read(s.audit))
	mux.Handle("GET /api/admin/v1/whoami", read(s.whoami))

	// A preflight arrives as OPTIONS on the same path, and Go's mux matches on method,
	// so every route above needs its OPTIONS twin or the browser gets a 405 and reports
	// it as a CORS failure -- the least informative error in the browser.
	for _, p := range []string{
		"/api/admin/v1/overview", "/api/admin/v1/conversations",
		"/api/admin/v1/conversations/{id}", "/api/admin/v1/tickets",
		"/api/admin/v1/tickets/{number}", "/api/admin/v1/audit",
		"/api/admin/v1/whoami",
	} {
		mux.Handle("OPTIONS "+p, s.cors.Wrap(http.NotFoundHandler()))
	}

	// No page is served from here any more. The operations UI is its own application on
	// its own origin (admin-ui/), which is why CORS exists above at all.
}

// refused records an authenticated operator being turned away.
func (s *Server) refused(r *http.Request, operator Operator) {
	if err := s.store.Audit(r.Context(), AuditEntry{
		Actor: operator.Name, Action: "refused " + r.Method, Object: r.URL.Path,
		Outcome: "forbidden", Detail: "role " + string(operator.Role),
	}); err != nil {
		slog.Error("could not record a refused action",
			"actor", operator.Name, "path", r.URL.Path, "error", err)
	}
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	writeJSON(w, map[string]any{
		"name":     operator.Name,
		"role":     operator.Role,
		"canWrite": operator.CanWrite(),
	})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	window := 24 * time.Hour
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 24*90 {
			window = time.Duration(n) * time.Hour
		}
	}
	o, err := s.store.Overview(r.Context(), window)
	if err != nil {
		fail(w, r, "overview", err)
		return
	}
	writeJSON(w, o)
}

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, total, err := s.store.Conversations(r.Context(), ConversationFilter{
		Outcome: q.Get("outcome"),
		Search:  q.Get("q"),
		Limit:   atoi(q.Get("limit")),
		Offset:  atoi(q.Get("offset")),
	})
	if err != nil {
		fail(w, r, "conversations", err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "conversations": list})
}

// conversation is the one endpoint that returns what a customer wrote, so it is the one
// that always writes an audit entry. Reading is an action here.
func (s *Server) conversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	turns, err := s.store.Conversation(r.Context(), id)
	if err != nil {
		fail(w, r, "conversation", err)
		return
	}
	s.record(r, "read conversation", "conversation/"+id, "ok", "")
	if turns == nil {
		http.Error(w, "no recorded turns for that conversation", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"conversationId": id, "turns": turns})
}

func (s *Server) ticketList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, total, err := s.tickets.List(r.Context(), ticket.Filter{
		State:          ticket.State(q.Get("state")),
		Assignee:       q.Get("assignee"),
		ConversationID: q.Get("conversationId"),
		Limit:          atoi(q.Get("limit")),
		Offset:         atoi(q.Get("offset")),
	})
	if err != nil {
		fail(w, r, "tickets", err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "tickets": list})
}

func (s *Server) ticketDetail(w http.ResponseWriter, r *http.Request) {
	t, events, err := s.tickets.Get(r.Context(), r.PathValue("number"))
	if errors.Is(err, ticket.ErrNotFound) {
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, r, "ticket", err)
		return
	}
	writeJSON(w, map[string]any{"ticket": t, "history": events})
}

type ticketPatch struct {
	ExpectedVersion int     `json:"expectedVersion"`
	State           string  `json:"state,omitempty"`
	Assignee        *string `json:"assignee,omitempty"`
	Resolution      string  `json:"resolution,omitempty"`
	Reason          string  `json:"reason,omitempty"`
	Note            string  `json:"note,omitempty"`
}

func (s *Server) ticketUpdate(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	number := r.PathValue("number")

	var patch ticketPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	if patch.ExpectedVersion <= 0 {
		// Not optional, and not defaulted. Without it two operators with the same ticket
		// open overwrite each other and the loser is told nothing.
		http.Error(w, "expectedVersion is required", http.StatusBadRequest)
		return
	}

	updated, err := s.tickets.Update(r.Context(), number, ticket.Update{
		Actor:           operator.Name,
		ExpectedVersion: patch.ExpectedVersion,
		State:           ticket.State(patch.State),
		Assignee:        patch.Assignee,
		Resolution:      patch.Resolution,
		Reason:          patch.Reason,
		Note:            patch.Note,
	})
	switch {
	case errors.Is(err, ticket.ErrNotFound):
		s.record(r, "update ticket", "ticket/"+number, "not_found", "")
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	case errors.Is(err, ticket.ErrConflict):
		s.record(r, "update ticket", "ticket/"+number, "conflict", err.Error())
		// 409 rather than 500: the operator can refresh and retry, and telling them so
		// is the difference between a usable page and a mysterious one.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.record(r, "update ticket", "ticket/"+number, "rejected", err.Error())
		// A rejected transition is the operator's mistake, not the server's.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.record(r, "update ticket", "ticket/"+number, "ok", patch.State+" "+patch.Note)
	writeJSON(w, updated)
}

// erase deletes what a customer said, on request.
//
// It is the most destructive thing this API can do and there is no undo, so three things
// are true of it deliberately: it is operator-only, it writes an audit entry naming what
// it removed *before* returning, and the entry is written even when the erasure found
// nothing -- "somebody asked us to erase a conversation that did not exist" is exactly the
// kind of thing an investigation later wants to know happened.
//
// There is no button for it in the operations UI. A one-click irreversible erase needs a
// confirmation design that has not been thought about yet, and shipping the button first
// is how that thinking gets skipped.
func (s *Server) erase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	report, err := s.erasure.EraseConversation(r.Context(), id)
	if err != nil {
		s.record(r, "erase conversation", "conversation/"+id, "failed", err.Error())
		fail(w, r, "erase", err)
		return
	}
	s.record(r, "erase conversation", "conversation/"+id, "ok", report.String())
	writeJSON(w, report)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, total, err := s.store.AuditTrail(r.Context(), atoi(q.Get("limit")), atoi(q.Get("offset")))
	if err != nil {
		fail(w, r, "audit", err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "entries": entries})
}

// record writes the audit entry, detached from the request context so that a client
// which disconnects mid-response does not lose the record of what it did.
func (s *Server) record(r *http.Request, action, object, outcome, detail string) {
	operator, _ := FromContext(r.Context())
	if err := s.store.Audit(r.Context(), AuditEntry{
		Actor: operator.Name, Action: action, Object: object,
		Outcome: outcome, Detail: detail,
	}); err != nil {
		// Loud, because an unrecorded action is the failure this table exists to
		// prevent. It does not fail the request: the action already happened.
		slog.Error("could not write an audit entry",
			"actor", operator.Name, "action", action, "object", object, "error", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("could not write an admin response", "error", err)
	}
}

func fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.Error("admin query failed", "query", what, "error", err)
	// The database's own words never reach the page: an operator is not the audience
	// for a constraint name, and a query error can carry data.
	http.Error(w, "the query failed; see the service log", http.StatusInternalServerError)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
