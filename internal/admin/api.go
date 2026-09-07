package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/feedback"
	"github.com/lai3d/ai-customer-service-go/internal/handoff"
	"github.com/lai3d/ai-customer-service-go/internal/knowledge"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/retention"
	"github.com/lai3d/ai-customer-service-go/internal/tenant"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
)

type Server struct {
	store     *Store
	tickets   *ticket.Store
	operators Operators
	cors      CORS
	erasure   *retention.Store
	handoff   *handoff.Store
	knowledge *knowledge.Store
	feedback  *feedback.Store
	// tenants is nil when no platform operator is configured, and the tenant routes are
	// then never registered -- the same rule as the whole surface: an absent route cannot
	// be misconfigured.
	tenants Tenants
}

func NewServer(store *Store, tickets *ticket.Store, operators Operators, cors CORS,
	erasure *retention.Store, replies *handoff.Store, entries *knowledge.Store,
	verdicts *feedback.Store, tenants Tenants) *Server {
	return &Server{store: store, tickets: tickets, operators: operators, cors: cors,
		erasure: erasure, handoff: replies, knowledge: entries, feedback: verdicts,
		tenants: tenants}
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
	// Three wrappers, and the middle layer is the tenant guard.
	//
	// `read` and `write` are for handlers that touch one tenant's data, so both refuse the
	// tenant-less platform role. `platform` is the opposite: only that role, and it reaches
	// no customer content at all. An account is on exactly one side of this line.
	read := func(h http.HandlerFunc) http.Handler {
		return s.cors.Wrap(s.operators.Authenticate(RequireTenant(s.refused, h)))
	}
	write := func(h http.HandlerFunc) http.Handler {
		return s.cors.Wrap(s.operators.Authenticate(
			RequireTenant(s.refused, RequireWrite(s.refused, h))))
	}
	platform := func(h http.HandlerFunc) http.Handler {
		return s.cors.Wrap(s.operators.Authenticate(RequirePlatform(s.refused, h)))
	}

	mux.Handle("GET /api/admin/v1/overview", read(s.overview))
	mux.Handle("GET /api/admin/v1/conversations", read(s.conversations))
	mux.Handle("GET /api/admin/v1/conversations/{id}", read(s.conversation))
	mux.Handle("GET /api/admin/v1/tickets", read(s.ticketList))
	mux.Handle("GET /api/admin/v1/tickets/{number}", read(s.ticketDetail))
	mux.Handle("PATCH /api/admin/v1/tickets/{number}", write(s.ticketUpdate))
	mux.Handle("POST /api/admin/v1/tickets/{number}/reply", write(s.reply))

	// Knowledge. Reading the drafts is a read; changing them and publishing are writes,
	// and publishing is the one that reaches customers.
	mux.Handle("GET /api/admin/v1/knowledge", read(s.knowledgeList))
	mux.Handle("PUT /api/admin/v1/knowledge/{entryId}/{language}", write(s.knowledgeSave))
	mux.Handle("DELETE /api/admin/v1/knowledge/{entryId}/{language}", write(s.knowledgeDelete))
	mux.Handle("GET /api/admin/v1/knowledge/versions", read(s.knowledgeVersions))
	mux.Handle("POST /api/admin/v1/knowledge/publish", write(s.knowledgePublish))
	mux.Handle("POST /api/admin/v1/knowledge/versions/{version}/activate", write(s.knowledgeActivate))
	mux.Handle("DELETE /api/admin/v1/conversations/{id}", write(s.erase))
	// Feedback. Reading the queue is a read; adding a verdict and clearing one are writes,
	// because both change what the next person sees as their work.
	mux.Handle("GET /api/admin/v1/feedback", read(s.feedbackQueue))
	mux.Handle("POST /api/admin/v1/turns/{id}/feedback", write(s.feedbackRecord))
	mux.Handle("POST /api/admin/v1/turns/{id}/feedback/handled", write(s.feedbackHandle))
	mux.Handle("GET /api/admin/v1/audit", read(s.audit))
	// whoami is the one authenticated route with no tenant guard: a platform account has
	// to be able to find out what it is, and the answer contains nothing but its own name,
	// role and tenant.
	mux.Handle("GET /api/admin/v1/whoami", s.cors.Wrap(s.operators.Authenticate(http.HandlerFunc(s.whoami))))

	// Tenants. Only the platform role, and none of it reads customer content: a tenant's
	// id, name and keys say who a customer of this service is, never what their customers
	// said. A key is returned in full exactly once, at issue.
	if s.tenants != nil {
		mux.Handle("GET /api/admin/v1/tenants", platform(s.tenantList))
		mux.Handle("POST /api/admin/v1/tenants", platform(s.tenantCreate))
		mux.Handle("POST /api/admin/v1/tenants/{id}/disabled", platform(s.tenantSetDisabled))
		mux.Handle("GET /api/admin/v1/tenants/{id}/keys", platform(s.tenantKeys))
		mux.Handle("POST /api/admin/v1/tenants/{id}/keys", platform(s.tenantIssueKey))
		mux.Handle("DELETE /api/admin/v1/tenants/{id}/keys/{keyId}", platform(s.tenantRevokeKey))
	}

	// A preflight arrives as OPTIONS on the same path, and Go's mux matches on method,
	// so every route above needs its OPTIONS twin or the browser gets a 405 and reports
	// it as a CORS failure -- the least informative error in the browser.
	for _, p := range []string{
		"/api/admin/v1/overview", "/api/admin/v1/conversations",
		"/api/admin/v1/conversations/{id}", "/api/admin/v1/tickets",
		"/api/admin/v1/tickets/{number}", "/api/admin/v1/tickets/{number}/reply",
		"/api/admin/v1/knowledge", "/api/admin/v1/knowledge/{entryId}/{language}",
		"/api/admin/v1/knowledge/versions",
		"/api/admin/v1/knowledge/publish",
		"/api/admin/v1/knowledge/versions/{version}/activate",
		"/api/admin/v1/feedback", "/api/admin/v1/turns/{id}/feedback",
		"/api/admin/v1/turns/{id}/feedback/handled",
		"/api/admin/v1/audit",
		"/api/admin/v1/whoami",
		"/api/admin/v1/tenants", "/api/admin/v1/tenants/{id}/disabled",
		"/api/admin/v1/tenants/{id}/keys", "/api/admin/v1/tenants/{id}/keys/{keyId}",
	} {
		mux.Handle("OPTIONS "+p, s.cors.Wrap(http.NotFoundHandler()))
	}

	// No page is served from here any more. The operations UI is its own application on
	// its own origin (admin-ui/), which is why CORS exists above at all.
}

// refused records an authenticated operator being turned away.
func (s *Server) refused(r *http.Request, operator Operator) {
	// Through recordFor, which is how it stops being the one audit write with its own copy
	// of the row-building: it was, it lost its tenant when the column arrived, and it
	// became a foreign-key error and a log line -- the exact failure an audit trail exists
	// to prevent. A refused *platform* operator has no tenant at all, which recordFor
	// files under the default one rather than dropping.
	s.recordFor(r, operator.TenantID, "refused "+r.Method, r.URL.Path, "forbidden",
		"role "+string(operator.Role))
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
	operator, _ := FromContext(r.Context())
	o, err := s.store.Overview(r.Context(), operator.TenantID, window)
	if err != nil {
		fail(w, r, "overview", err)
		return
	}
	writeJSON(w, o)
}

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	operator, _ := FromContext(r.Context())
	list, total, err := s.store.Conversations(r.Context(), ConversationFilter{
		TenantID: operator.TenantID,
		Outcome:  q.Get("outcome"),
		Search:   q.Get("q"),
		Limit:    atoi(q.Get("limit")),
		Offset:   atoi(q.Get("offset")),
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
	operator, _ := FromContext(r.Context())
	turns, err := s.store.Conversation(r.Context(), operator.TenantID, id)
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
	operator, _ := FromContext(r.Context())
	list, total, err := s.tickets.List(r.Context(), ticket.Filter{
		TenantID:       operator.TenantID,
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
	operator, _ := FromContext(r.Context())
	t, events, err := s.tickets.Get(r.Context(), operator.TenantID, r.PathValue("number"))
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

	updated, err := s.tickets.Update(r.Context(), operator.TenantID, number, ticket.Update{
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

// feedbackQueue is what somebody works from: answers that were reported wrong, with the
// question, the reply and what was retrieved, so acting on one does not start with a
// lookup.
func (s *Server) feedbackQueue(w http.ResponseWriter, r *http.Request) {
	includeHandled := r.URL.Query().Get("handled") == "true"
	operator, _ := FromContext(r.Context())
	items, err := s.feedback.Queue(r.Context(), operator.TenantID, includeHandled,
		atoi(r.URL.Query().Get("limit")))
	if err != nil {
		fail(w, r, "feedback", err)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) feedbackRecord(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	var body struct {
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	turnID := r.PathValue("id")
	switch err := s.feedback.Record(r.Context(), operator.TenantID, turnID, feedback.SourceOperator,
		feedback.Verdict(body.Verdict), body.Note, operator.Name); {
	case errors.Is(err, feedback.ErrNoSuchTurn):
		http.Error(w, "no such turn", http.StatusNotFound)
		return
	case errors.Is(err, feedback.ErrBadVerdict):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		fail(w, r, "feedback", err)
		return
	}
	// Audited because it is a judgement about the service recorded against a named person,
	// which is exactly what the trail is for. The note is not in the detail: it is on the
	// feedback row, which an erasure can reach.
	s.record(r, "judge answer", "turn/"+turnID, "ok", body.Verdict)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) feedbackHandle(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	turnID := r.PathValue("id")
	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	source := feedback.Source(body.Source)
	if source != feedback.SourceCustomer && source != feedback.SourceOperator {
		http.Error(w, "source must be customer or operator", http.StatusUnprocessableEntity)
		return
	}
	switch err := s.feedback.Handle(r.Context(), operator.TenantID, turnID, source, operator.Name); {
	case errors.Is(err, feedback.ErrNoSuchTurn):
		http.Error(w, "no such open feedback", http.StatusNotFound)
		return
	case err != nil:
		fail(w, r, "feedback", err)
		return
	}
	s.record(r, "clear feedback", "turn/"+turnID, "ok", string(source))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) knowledgeList(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	entries, err := s.knowledge.List(r.Context(), operator.TenantID)
	if err != nil {
		fail(w, r, "knowledge", err)
		return
	}
	active, revision, err := s.knowledge.State(r.Context(), operator.TenantID)
	if err != nil {
		fail(w, r, "knowledge", err)
		return
	}
	// Whether the drafts and the live corpus have diverged.
	//
	// They can, legitimately: a rollback changes the active version and leaves the drafts
	// alone, so an operator looking at the list sees text that is not what customers are
	// being told. Found by opening the page after a rollback -- the list said 45 days
	// while the service answered 30, and nothing on screen said which was live.
	//
	// Computed here rather than in the page because the server has both timestamps and the
	// page would have to infer it from two responses that can disagree.
	unpublished, err := s.knowledge.HasUnpublishedChanges(r.Context(), operator.TenantID)
	if err != nil {
		fail(w, r, "knowledge", err)
		return
	}
	// The revision travels with the list because it is what a publication has to hand
	// back. A page that fetched it separately could read a revision that changed between
	// the two requests and lose the race it exists to detect.
	writeJSON(w, map[string]any{
		"entries": entries, "activeVersion": active, "revision": revision,
		"unpublishedChanges": unpublished})
}

func (s *Server) knowledgeSave(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	var entry knowledge.Entry
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&entry); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	// The path is the identity, not the body: a body that disagreed with the URL would
	// let one entry's edit land on another.
	entry.EntryID, entry.Language = r.PathValue("entryId"), r.PathValue("language")

	before, err := s.knowledge.Save(r.Context(), operator.TenantID, entry, operator.Name)
	if err != nil {
		s.record(r, "edit knowledge", knowledgeObject(entry.EntryID, entry.Language),
			"rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	// What changed, not that something did. An audit line saying "alex edited
	// returns-window" cannot answer the question anybody asks it afterwards.
	s.record(r, "edit knowledge", knowledgeObject(entry.EntryID, entry.Language), "ok",
		changeSummary(before, entry))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) knowledgeDelete(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	entryID, language := r.PathValue("entryId"), r.PathValue("language")
	switch err := s.knowledge.Delete(r.Context(), operator.TenantID, entryID, language, operator.Name); {
	case errors.Is(err, knowledge.ErrNotFound):
		s.record(r, "delete knowledge", knowledgeObject(entryID, language), "not_found", "")
		http.Error(w, "no such entry", http.StatusNotFound)
		return
	case err != nil:
		fail(w, r, "knowledge", err)
		return
	}
	// Not live yet, and the audit line says so: the entry stops being retrievable when
	// somebody publishes, not when somebody deletes.
	s.record(r, "delete knowledge", knowledgeObject(entryID, language), "ok",
		"marked deleted; takes effect on the next publication")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) knowledgeVersions(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	versions, err := s.knowledge.Versions(r.Context(), operator.TenantID)
	if err != nil {
		fail(w, r, "knowledge", err)
		return
	}
	writeJSON(w, map[string]any{"versions": versions})
}

// knowledgePublish is the only operator action that changes what customers are answered
// from, which is why it is audited with the version it produced and why a stale revision
// is a 409 rather than an overwrite.
func (s *Server) knowledgePublish(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	var body struct {
		Note     string `json:"note"`
		Revision int    `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	if body.Revision <= 0 {
		http.Error(w, "revision is required: read it from GET /knowledge",
			http.StatusBadRequest)
		return
	}

	version, err := s.knowledge.Publish(r.Context(), operator.TenantID, operator.Name,
		body.Note, body.Revision)
	switch {
	case errors.Is(err, rag.ErrStaleActivation):
		s.record(r, "publish knowledge", "corpus", "conflict", "someone else published first")
		http.Error(w, "someone else published while this page was open; reload and try again",
			http.StatusConflict)
		return
	case errors.Is(err, knowledge.ErrEmptyDraft):
		s.record(r, "publish knowledge", "corpus", "rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		s.record(r, "publish knowledge", "corpus", "failed", err.Error())
		fail(w, r, "publish", err)
		return
	}
	s.record(r, "publish knowledge", "corpus/"+version, "ok", body.Note)
	writeJSON(w, map[string]any{"version": version})
}

func (s *Server) knowledgeActivate(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	version := r.PathValue("version")
	var body struct {
		Revision int `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	switch err := s.knowledge.Activate(r.Context(), operator.TenantID, version,
		operator.Name, body.Revision); {
	case errors.Is(err, rag.ErrStaleActivation):
		s.record(r, "activate corpus", "corpus/"+version, "conflict", "")
		http.Error(w, "someone else changed the active version; reload and try again",
			http.StatusConflict)
		return
	case err != nil:
		// Includes the case that matters: a retained version whose documents were swept.
		// Activating it would answer nothing, so it is refused rather than obeyed.
		s.record(r, "activate corpus", "corpus/"+version, "rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.record(r, "activate corpus", "corpus/"+version, "ok", "rolled back or re-activated")
	w.WriteHeader(http.StatusNoContent)
}

func knowledgeObject(entryID, language string) string {
	return "knowledge/" + entryID + ":" + language
}

// changeSummary says what an edit did without putting the whole answer in the audit trail.
// Lengths and a created/updated distinction are enough to answer "what happened here" and
// small enough that the trail stays readable.
func changeSummary(before *knowledge.Entry, after knowledge.Entry) string {
	if before == nil {
		return fmt.Sprintf("created; question %d chars, answer %d chars",
			len(after.Question), len(after.Answer))
	}
	var changed []string
	if before.Question != after.Question {
		changed = append(changed, fmt.Sprintf("question %d -> %d chars",
			len(before.Question), len(after.Question)))
	}
	if before.Answer != after.Answer {
		changed = append(changed, fmt.Sprintf("answer %d -> %d chars",
			len(before.Answer), len(after.Answer)))
	}
	if before.Category != after.Category {
		changed = append(changed, "category "+before.Category+" -> "+after.Category)
	}
	if before.Deleted && !after.Deleted {
		changed = append(changed, "restored")
	}
	if len(changed) == 0 {
		return "saved with no change"
	}
	return strings.Join(changed, ", ")
}

// reply is the half of a handoff that nobody builds.
//
// A ticket that a human answers and a customer never hears about is the same ticket that
// was never answered, from the only point of view that matters. The reply goes into the
// customer's conversation, which is also what stops the model contradicting it on the next
// turn -- the history it composes from now contains the human's answer.
func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	number := r.PathValue("number")

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}

	switch err := s.handoff.Reply(r.Context(), number, operator.Name, body.Text); {
	case errors.Is(err, handoff.ErrNoSuchTicket):
		s.record(r, "reply to ticket", "ticket/"+number, "not_found", "")
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	case errors.Is(err, handoff.ErrEmptyReply):
		s.record(r, "reply to ticket", "ticket/"+number, "rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		fail(w, r, "reply", err)
		return
	}
	// The text is not in the audit detail. The trail records that a reply was sent and by
	// whom; what was said is in the ticket history, which an erasure can redact.
	s.record(r, "reply to ticket", "ticket/"+number, "ok", fmt.Sprintf("%d characters", len(body.Text)))
	w.WriteHeader(http.StatusNoContent)
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
	operator, _ := FromContext(r.Context())
	entries, total, err := s.store.AuditTrail(r.Context(), operator.TenantID,
		atoi(q.Get("limit")), atoi(q.Get("offset")))
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
	s.recordFor(r, operator.TenantID, action, object, outcome, detail)
}

// recordFor writes an audit row against a tenant the operator does not belong to.
//
// It exists for exactly one caller: the platform role, which belongs to no tenant and
// administers all of them. The row is filed under the tenant being administered rather
// than under an empty string, because "who created this tenant" is a question asked while
// looking at that tenant.
func (s *Server) recordFor(r *http.Request, tenantID, action, object, outcome, detail string) {
	operator, _ := FromContext(r.Context())
	if tenantID == "" {
		// A row with no tenant cannot be written -- the column is NOT NULL with a foreign
		// key -- and losing an audit row is the failure the table exists to prevent. The
		// default tenant is the honest place for an action that named no valid tenant:
		// somebody tried something, and that is worth a row.
		tenantID = tenant.Default
	}
	if err := s.store.Audit(r.Context(), AuditEntry{
		TenantID: tenantID, Actor: operator.Name, Action: action, Object: object,
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
