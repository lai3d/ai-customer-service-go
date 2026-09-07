package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/admin"
	"github.com/lai3d/ai-customer-service-go/internal/feedback"
	"github.com/lai3d/ai-customer-service-go/internal/handoff"
	"github.com/lai3d/ai-customer-service-go/internal/knowledge"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/retention"
	"github.com/lai3d/ai-customer-service-go/internal/tenant"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
)

const (
	operatorToken = "operator-token-that-is-long-enough"
	viewerToken   = "viewer-token-that-is-long-enough"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, stop, err := testsupport.StartPostgres(context.Background(), 384)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

func serve(t *testing.T) (*httptest.Server, *ticket.Store) {
	t.Helper()
	ops, err := admin.ParseOperators(
		"alex:" + operatorToken + ":operator,dana:" + viewerToken + ":viewer")
	if err != nil {
		t.Fatal(err)
	}
	tickets := ticket.NewStore(pool)
	mux := http.NewServeMux()
	admin.NewServer(admin.NewStore(pool), tickets, ops, corsFor(t),
		retention.NewStore(pool), handoffFor(pool), knowledgeFor(pool), feedback.NewStore(pool), tenant.NewStore(pool)).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, tickets
}

func do(t *testing.T, server *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, server.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The surface does not exist unless somebody configured who may use it. This is the
// difference between a guarded page and an absent one, and only one of them survives a
// mistake in the guard.
func TestWithNoOperatorsConfiguredThereIsNoAdminSurface(t *testing.T) {
	ops, err := admin.ParseOperators("")
	if err != nil {
		t.Fatal(err)
	}
	if ops.Enabled() {
		t.Fatal("an empty configuration produced an enabled admin surface")
	}

	// The caller must not mount the routes; a bare mux then 404s like any unknown path.
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	for _, path := range []string{"/api/admin/v1/overview", "/api/admin/v1/tickets"} {
		resp := do(t, server, "GET", path, operatorToken, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d with no operators configured, want 404", path, resp.StatusCode)
		}
	}
}

func TestTokensAreRejectedBeforeTheyCanBeWeak(t *testing.T) {
	cases := []struct{ name, spec, wants string }{
		{"no token", "alex", "name:token"},
		{"empty token", "alex:", "name:token"},
		// This credential reads every customer conversation in the database.
		{"short token", "alex:hunter2:operator", "16"},
		{"unknown role", "alex:" + operatorToken + ":superuser", "unknown role"},
		{"a fifth field", "alex:" + operatorToken + ":operator:acme:extra", "name:token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.ParseOperators(tc.spec)
			if err == nil {
				t.Fatalf("%q was accepted", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error is %q, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// An omitted role is a viewer. Least privilege is the safe direction for a typo.
func TestAnOmittedRoleIsReadOnly(t *testing.T) {
	ops, err := admin.ParseOperators("dana:" + viewerToken)
	if err != nil {
		t.Fatal(err)
	}
	names := ops.Names()
	if len(names) != 1 || !strings.Contains(names[0], "viewer") {
		t.Errorf("operators are %v, want dana as a viewer", names)
	}
}

func TestEveryEndpointRejectsAnUnknownToken(t *testing.T) {
	server, _ := serve(t)
	for _, path := range []string{
		"/api/admin/v1/overview", "/api/admin/v1/conversations",
		"/api/admin/v1/tickets", "/api/admin/v1/audit", "/api/admin/v1/whoami",
	} {
		if got := do(t, server, "GET", path, "", "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s with no token returned %d, want 401", path, got)
		}
		if got := do(t, server, "GET", path, "not-the-token-but-long-enough", "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token returned %d, want 401", path, got)
		}
	}
}

// Hiding a button is a user-interface decision. This is the access control.
func TestAViewerCannotChangeATicket(t *testing.T) {
	server, tickets := serve(t)
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{TenantID: tenant.Default,
		ConversationID: "viewer-cannot-write", Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"expectedVersion":%d,"note":"trying"}`, created.Version)
	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, viewerToken, body).StatusCode; got != http.StatusForbidden {
		t.Errorf("a viewer's PATCH returned %d, want 403", got)
	}
	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, operatorToken, body).StatusCode; got != http.StatusOK {
		t.Errorf("an operator's PATCH returned %d, want 200", got)
	}
}

// The version check has to be reachable from the API, not only from the store: an
// endpoint that defaults it away would silently reintroduce lost updates.
func TestAStaleUpdateIsAConflictAndNotAServerError(t *testing.T) {
	server, tickets := serve(t)
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{TenantID: tenant.Default,
		ConversationID: "stale-update", Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}
	stale := fmt.Sprintf(`{"expectedVersion":%d,"note":"first"}`, created.Version)

	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, operatorToken, stale).StatusCode; got != http.StatusOK {
		t.Fatalf("the first update returned %d", got)
	}
	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, operatorToken, stale).StatusCode; got != http.StatusConflict {
		t.Errorf("the stale update returned %d, want 409 so the operator can refresh", got)
	}

	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, operatorToken,
		`{"note":"no version at all"}`).StatusCode; got != http.StatusBadRequest {
		t.Errorf("an update with no expectedVersion returned %d, want 400", got)
	}
}

// Reading a customer's words is an action. Who looked is most of what an audit trail is
// for, and this is the only surface in the service that shows them to anyone.
func TestOpeningAConversationIsAudited(t *testing.T) {
	server, _ := serve(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO turn (id, tenant_id, conversation_id, started_at, outcome, question)
		VALUES ('turn-audited', 'default', 'audited-conversation', now(), 'completed', 'where is my order?')`); err != nil {
		t.Fatal(err)
	}

	if got := do(t, server, "GET", "/api/admin/v1/conversations/audited-conversation", viewerToken, "").StatusCode; got != http.StatusOK {
		t.Fatalf("reading the conversation returned %d", got)
	}

	var actor, action, object string
	if err := pool.QueryRow(ctx, `
		SELECT actor, action, object FROM admin_audit
		WHERE object = 'conversation/audited-conversation' ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &action, &object); err != nil {
		t.Fatalf("no audit entry for reading a conversation: %v", err)
	}
	if actor != "dana" {
		t.Errorf("audit actor is %q; an entry that cannot name who looked is most of the "+
			"way to no entry at all", actor)
	}
	if !strings.Contains(action, "read") {
		t.Errorf("audit action is %q", action)
	}
}

// The audit trail has no write path other than appending. There is deliberately no
// endpoint that edits or deletes it, and this fails if one appears.
func TestNothingCanEditTheAuditTrail(t *testing.T) {
	server, _ := serve(t)
	for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		resp := do(t, server, method, "/api/admin/v1/audit", operatorToken, `{}`)
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s /audit returned %d; the trail must have no mutation path",
				method, resp.StatusCode)
		}
	}
}

// A database error must not reach an operator's browser: a constraint name is not for
// them, and a query error can carry the data that caused it.
func TestQueryFailuresDoNotLeakTheDatabasesWords(t *testing.T) {
	server, _ := serve(t)
	// A conversation id longer than its column, which fails in the database rather than
	// in validation.
	resp := do(t, server, "GET",
		"/api/admin/v1/conversations?q="+strings.Repeat("x", 300), operatorToken, "")
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SQLSTATE")) || bytes.Contains(raw, []byte("pg_")) {
		t.Errorf("the response carries database internals: %q", raw)
	}
}

func TestWhoamiTellsThePageWhatItMayDo(t *testing.T) {
	server, _ := serve(t)
	for token, wantWrite := range map[string]bool{operatorToken: true, viewerToken: false} {
		resp := do(t, server, "GET", "/api/admin/v1/whoami", token, "")
		var me struct {
			Name     string `json:"name"`
			CanWrite bool   `json:"canWrite"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
			t.Fatal(err)
		}
		if me.CanWrite != wantWrite {
			t.Errorf("%s: canWrite is %v, want %v", me.Name, me.CanWrite, wantWrite)
		}
	}
}

// The same rule as the demo page, and it matters more here: this page renders what
// customers wrote, so it is the one place where model- and customer-authored text is
// A refused attempt is exactly what an audit trail is for. This was missing until a live
// walk through the operator workflow showed a 403 leaving no trace at all -- the deny
// path returned before anything recorded it.
func TestARefusedActionIsAudited(t *testing.T) {
	server, tickets := serve(t)
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{TenantID: tenant.Default,
		ConversationID: "refusal-audited", Summary: "a problem", Category: "returns"})
	if err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"expectedVersion":%d,"assignee":"dana"}`, created.Version)
	if got := do(t, server, "PATCH", "/api/admin/v1/tickets/"+created.Number, viewerToken, body).StatusCode; got != http.StatusForbidden {
		t.Fatalf("the viewer's PATCH returned %d, want 403", got)
	}

	var actor, action, outcome string
	if err := pool.QueryRow(context.Background(), `
		SELECT actor, action, outcome FROM admin_audit
		WHERE outcome = 'forbidden' ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &action, &outcome); err != nil {
		t.Fatalf("a refused action left no audit entry: %v", err)
	}
	if actor != "dana" || !strings.Contains(action, "PATCH") {
		t.Errorf("audit entry is %s/%s/%s, want dana being refused a PATCH", actor, action, outcome)
	}
}

const uiOrigin = "https://ops.example.com"

func corsFor(t *testing.T) admin.CORS {
	t.Helper()
	return admin.ParseCORS(uiOrigin + ", https://ops2.example.com")
}

// The UI is a separate application on a separate origin, so every one of its requests is
// cross-origin and the browser decides whether the response may be read. These assertions
// are that decision.
func TestTheBrowserIsToldWhichOriginMayReadTheseResponses(t *testing.T) {
	server, _ := serve(t)

	t.Run("a preflight from the UI is answered without a token", func(t *testing.T) {
		// A browser sends no Authorization header on a preflight. If this needed one,
		// every cross-origin request would fail with an opaque network error.
		resp := preflight(t, server, uiOrigin, "PATCH")
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("preflight returned %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != uiOrigin {
			t.Errorf("Allow-Origin is %q, want the UI's origin echoed back", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
			t.Errorf("Allow-Headers is %q; the UI cannot send its token", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PATCH") {
			t.Errorf("Allow-Methods is %q; the UI cannot update a ticket", got)
		}
	})

	t.Run("another origin is not allowed to read anything", func(t *testing.T) {
		resp := do(t, server, "GET", "/api/admin/v1/whoami", operatorToken, "")
		_ = resp
		req, _ := http.NewRequest("GET", server.URL+"/api/admin/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+operatorToken)
		req.Header.Set("Origin", "https://evil.test")
		got, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer got.Body.Close()
		// The request still succeeds -- CORS is enforced in the browser, not here -- but
		// without this header the browser will not hand the body to the page.
		if h := got.Header.Get("Access-Control-Allow-Origin"); h != "" {
			t.Errorf("an unknown origin was told it may read responses: %q", h)
		}
		if !strings.Contains(got.Header.Get("Vary"), "Origin") {
			t.Error("no Vary: Origin, so a shared cache can serve one origin's response to another")
		}
	})

	t.Run("a preflight from another origin is refused", func(t *testing.T) {
		resp := preflight(t, server, "https://evil.test", "PATCH")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("preflight from an unknown origin returned %d, want 403", resp.StatusCode)
		}
		if h := resp.Header.Get("Access-Control-Allow-Origin"); h != "" {
			t.Errorf("refused preflight still carried Allow-Origin: %q", h)
		}
	})

	// A wildcard is the value that makes CORS errors disappear in development and turns
	// the support inbox into something any page can read. There must be no configuration
	// that produces one.
	t.Run("no configuration produces a wildcard", func(t *testing.T) {
		for _, spec := range []string{"*", "https://a.test,*", " * "} {
			c := admin.ParseCORS(spec)
			mux := http.NewServeMux()
			admin.NewServer(admin.NewStore(pool), ticket.NewStore(pool), mustOps(t), c,
				retention.NewStore(pool), handoffFor(pool), knowledgeFor(pool), feedback.NewStore(pool), tenant.NewStore(pool)).Routes(mux)
			s := httptest.NewServer(mux)
			req, _ := http.NewRequest("GET", s.URL+"/api/admin/v1/whoami", nil)
			req.Header.Set("Authorization", "Bearer "+operatorToken)
			req.Header.Set("Origin", "https://anywhere.test")
			resp, err := s.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			s.Close()
			if h := resp.Header.Get("Access-Control-Allow-Origin"); h != "" {
				t.Errorf("ADMIN_CORS_ORIGINS=%q let an arbitrary origin read responses (%q)",
					spec, h)
			}
		}
	})

	// A prefix match on https://ops.example.com also accepts
	// https://ops.example.com.evil.test, which is a domain an attacker can register.
	t.Run("a lookalike origin is not a match", func(t *testing.T) {
		for _, origin := range []string{
			uiOrigin + ".evil.test", "https://evil.test?" + uiOrigin, "http://ops.example.com",
		} {
			resp := preflight(t, server, origin, "GET")
			if h := resp.Header.Get("Access-Control-Allow-Origin"); h != "" {
				t.Errorf("origin %q was allowed", origin)
			}
		}
	})
}

// With no origins configured there is no CORS at all: correct for a reverse proxy that
// serves the UI and the API from one origin, and the reason the empty value is not a
// permissive default.
func TestWithNoOriginsConfiguredThereIsNoCORS(t *testing.T) {
	mux := http.NewServeMux()
	admin.NewServer(admin.NewStore(pool), ticket.NewStore(pool), mustOps(t),
		admin.ParseCORS(""), retention.NewStore(pool), handoffFor(pool),
		knowledgeFor(pool), feedback.NewStore(pool), tenant.NewStore(pool)).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	req, _ := http.NewRequest("GET", server.URL+"/api/admin/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	req.Header.Set("Origin", uiOrigin)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if h := resp.Header.Get("Access-Control-Allow-Origin"); h != "" {
		t.Errorf("CORS is off but the response carried Allow-Origin: %q", h)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("same-origin use broke when CORS was disabled: %d", resp.StatusCode)
	}
}

func mustOps(t *testing.T) admin.Operators {
	t.Helper()
	ops, err := admin.ParseOperators("alex:" + operatorToken + ":operator")
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func preflight(t *testing.T, server *httptest.Server, origin, method string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("OPTIONS", server.URL+"/api/admin/v1/tickets/TKT-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A page of the audit trail must say how much trail there is, not how much of it it is
// carrying. The admin UI states that number on screen, and a footer that counts the rows
// it happens to hold tells a reader there is nothing more -- which was true of every list
// in the UI until the page size mattered.
func TestTheAuditTrailReportsItsTotalSeparatelyFromThePage(t *testing.T) {
	server, _ := serve(t)
	for i := 0; i < 7; i++ {
		// Each read writes one row: reading is an action here.
		resp := do(t, server, "GET", "/api/admin/v1/conversations/none-"+strconv.Itoa(i),
			operatorToken, "")
		resp.Body.Close()
	}

	var page struct {
		Total   int `json:"total"`
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
	}
	resp := do(t, server, "GET", "/api/admin/v1/audit?limit=3&offset=0", operatorToken, "")
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("asked for 3 entries and got %d", len(page.Entries))
	}
	if page.Total < 7 {
		t.Errorf("the trail reports %d rows in total, but at least 7 were written; a "+
			"reader is told the page size and concludes that is all there is", page.Total)
	}

	// And the second page must be different rows, not the same ones again.
	var second struct {
		Entries []struct {
			At     string `json:"at"`
			Object string `json:"object"`
		} `json:"entries"`
	}
	resp2 := do(t, server, "GET", "/api/admin/v1/audit?limit=3&offset=3", operatorToken, "")
	defer resp2.Body.Close()
	if err := json.NewDecoder(resp2.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) == 0 {
		t.Fatal("the second page is empty")
	}
	var first struct {
		Entries []struct {
			At     string `json:"at"`
			Object string `json:"object"`
		} `json:"entries"`
	}
	resp3 := do(t, server, "GET", "/api/admin/v1/audit?limit=3&offset=0", operatorToken, "")
	defer resp3.Body.Close()
	if err := json.NewDecoder(resp3.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].Object == second.Entries[0].Object &&
		first.Entries[0].At == second.Entries[0].At {
		t.Error("offset changed nothing; the second page repeats the first")
	}
}

// Erasure is the most destructive thing this API can do, and there is no undo. Three
// things have to be true of it, and only one of them is about deleting anything.
func TestErasingAConversationIsOperatorOnlyAndAudited(t *testing.T) {
	server, _ := serve(t)
	ctx := context.Background()
	const id = "erase-through-the-api"

	if _, err := pool.Exec(ctx, `INSERT INTO chat_memory (tenant_id, conversation_id, role, content)
		VALUES ('default',$1,'user','my card number is 4111 1111 1111 1111')`, id); err != nil {
		t.Fatal(err)
	}

	// A viewer must not be able to erase, and the refusal is itself audited.
	resp := do(t, server, "DELETE", "/api/admin/v1/conversations/"+id, viewerToken, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer got %d erasing a conversation, want 403", resp.StatusCode)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_memory WHERE conversation_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("a viewer's refused request erased the conversation anyway")
	}

	resp = do(t, server, "DELETE", "/api/admin/v1/conversations/"+id, operatorToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an operator got %d erasing a conversation", resp.StatusCode)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_memory WHERE conversation_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d messages survived an operator's erasure", n)
	}

	// The audit entry has to say what was removed. "Somebody erased something" records
	// that it happened and not what, which is the failure the trail exists to prevent.
	var detail string
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(detail,'') FROM admin_audit
		WHERE action = 'erase conversation' AND object = $1 AND outcome = 'ok'
		ORDER BY id DESC LIMIT 1`, "conversation/"+id).Scan(&detail); err != nil {
		t.Fatalf("the erasure left no audit entry: %v", err)
	}
	if !strings.Contains(detail, "memory=") {
		t.Errorf("the audit entry does not say what was removed: %q", detail)
	}
}

// handoffFor builds a reply path with no webhook: the notification is a working no-op and
// these tests are about what reaches the customer, not about what reaches a chat room.
func handoffFor(pool *pgxpool.Pool) *handoff.Store {
	return handoff.NewStore(pool, handoff.NewNotifier(pool, "", 0))
}

// knowledgeFor builds the editing store with an embedder that returns a fixed non-zero
// vector. These tests are about the endpoints -- who may call them, what they audit -- and
// loading a 470 MB model to check an authorisation rule would make them minutes long.
//
// Non-zero because a zero vector has NaN cosine distance, so a search silently returns
// nothing: CLAUDE.md records that one.
type fixedEmbedder struct{}

func (fixedEmbedder) EmbedPassages(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, 384)
		v[i%384] = 1
		out[i] = v
	}
	return out, nil
}

func knowledgeFor(pool *pgxpool.Pool) *knowledge.Store {
	return knowledge.NewStore(pool, rag.NewStore(pool), fixedEmbedder{})
}

// whoami runs a request through the real Authenticate middleware and reports the operator
// it landed as. Through the middleware rather than a lookup helper, because what matters is
// the identity a handler actually receives.
func whoami(t *testing.T, ops admin.Operators, token string) (admin.Operator, bool) {
	t.Helper()
	var got admin.Operator
	var ok bool
	handler := ops.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = admin.FromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/api/admin/v1/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return got, ok
}

// One tenant per staff account, and no account sees two. An operations surface that can be
// pointed at a neighbour's conversations by editing a query parameter is the
// confidentiality hole this whole surface exists to avoid having.
func TestAnOperatorBelongsToExactlyOneTenant(t *testing.T) {
	ops, err := admin.ParseOperators(
		"alex:" + operatorToken + ":operator,kim:" + viewerToken + ":viewer:acme")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, name := range ops.Names() {
		got[name] = ""
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d operators", len(got))
	}

	// An omitted tenant is the default one, which is what every configuration written
	// before tenancy means and keeps meaning.
	alex, ok := whoami(t, ops, operatorToken)
	if !ok {
		t.Fatal("alex does not authenticate")
	}
	if alex.TenantID != tenant.Default {
		t.Errorf("an operator with no tenant field belongs to %q, want %q",
			alex.TenantID, tenant.Default)
	}
	kim, ok := whoami(t, ops, viewerToken)
	if !ok {
		t.Fatal("kim does not authenticate")
	}
	if kim.TenantID != "acme" {
		t.Errorf("kim belongs to %q, want acme", kim.TenantID)
	}
}

// The isolation test for the surface that shows customer text on purpose.
//
// Two operators, one per tenant, against the same database. Everything one can ask for --
// the conversation list, one conversation's turns, the tickets, one ticket, the overview
// and the audit trail -- must be about their own tenant and nothing else.
//
// The other tenant's conversation id and ticket number are handed to the wrong operator
// directly. That is the case that happens: an id from a screenshot, a support email, a
// shared spreadsheet.
func TestAnOperatorSeesOnlyTheirOwnTenant(t *testing.T) {
	ctx := context.Background()
	const acmeToken = "acme-operator-token-0123456789"
	const globexToken = "globex-operator-token-0123456"

	tenants := tenant.NewStore(pool)
	stamp := time.Now().UnixNano()
	acme := fmt.Sprintf("acme-%d", stamp)
	globex := fmt.Sprintf("globex-%d", stamp)
	for _, id := range []string{acme, globex} {
		if _, err := tenants.Create(ctx, id, id, "platform"); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := admin.ParseOperators(
		"acmeop:" + acmeToken + ":operator:" + acme + "," +
			"globexop:" + globexToken + ":operator:" + globex)
	if err != nil {
		t.Fatal(err)
	}
	tickets := ticket.NewStore(pool)
	mux := http.NewServeMux()
	admin.NewServer(admin.NewStore(pool), tickets, ops, corsFor(t),
		retention.NewStore(pool), handoffFor(pool), knowledgeFor(pool),
		feedback.NewStore(pool), tenant.NewStore(pool)).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// One conversation and one ticket each.
	made := map[string]struct{ conversation, ticketNumber string }{}
	for _, id := range []string{acme, globex} {
		conversation := fmt.Sprintf("conv-%s", id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO turn (id, tenant_id, conversation_id, started_at, outcome, question, reply)
			VALUES ($1, $2, $3, now(), 'completed', $4, 'we can help')`,
			"turn-"+id, id, conversation,
			"my secret question for "+id); err != nil {
			t.Fatal(err)
		}
		tk, _, err := tickets.Create(ctx, ticket.CreateRequest{
			TenantID: id, ConversationID: conversation,
			Summary: "summary for " + id, Category: "returns"})
		if err != nil {
			t.Fatal(err)
		}
		made[id] = struct{ conversation, ticketNumber string }{conversation, tk.Number}
	}

	// The status is asserted, not just the body.
	//
	// Without it every `doesNotContain` below passes on a 500, because an error body
	// contains nobody's conversation id. That is not hypothetical: the tenancy work left
	// both list queries returning `could not determine data type of parameter $3`, this
	// test stayed green, and a browser found it.
	read := func(token, path string) string {
		t.Helper()
		resp := do(t, server, "GET", path, token, "")
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, body)
		}
		return string(body)
	}

	for _, c := range []struct{ token, mine, theirs string }{
		{acmeToken, acme, globex},
		{globexToken, globex, acme},
	} {
		mine, theirs := made[c.mine], made[c.theirs]

		// The lists.
		if body := read(c.token, "/api/admin/v1/conversations"); strings.Contains(body, theirs.conversation) {
			t.Errorf("%s's conversation list contains %s", c.mine, theirs.conversation)
		}
		if body := read(c.token, "/api/admin/v1/tickets"); strings.Contains(body, theirs.ticketNumber) {
			t.Errorf("%s's ticket list contains %s", c.mine, theirs.ticketNumber)
		}

		// The direct reads, with the other tenant's id handed over.
		if got := do(t, server, "GET",
			"/api/admin/v1/conversations/"+theirs.conversation, c.token, "").StatusCode; got != http.StatusNotFound {
			t.Errorf("%s reading %s's conversation got %d, want 404", c.mine, c.theirs, got)
		}
		if got := do(t, server, "GET",
			"/api/admin/v1/tickets/"+theirs.ticketNumber, c.token, "").StatusCode; got != http.StatusNotFound {
			t.Errorf("%s reading %s's ticket got %d, want 404", c.mine, c.theirs, got)
		}

		// And their own still works, so none of the above passes by refusing everything.
		if body := read(c.token, "/api/admin/v1/conversations/"+mine.conversation); !strings.Contains(body, "my secret question for "+c.mine) {
			t.Errorf("%s cannot read its own conversation: %s", c.mine, body)
		}
		if got := do(t, server, "GET",
			"/api/admin/v1/tickets/"+mine.ticketNumber, c.token, "").StatusCode; got != http.StatusOK {
			t.Errorf("%s cannot read its own ticket: %d", c.mine, got)
		}
	}

	// A write to another tenant's ticket is refused before the row is locked.
	body := fmt.Sprintf(`{"expectedVersion":1,"assignee":"acmeop"}`)
	if got := do(t, server, "PATCH",
		"/api/admin/v1/tickets/"+made[globex].ticketNumber, acmeToken, body).StatusCode; got != http.StatusNotFound {
		t.Errorf("acme patching globex's ticket got %d, want 404", got)
	}

	// The audit trail is scoped too: reading a conversation writes a row, and each
	// operator sees only their own.
	if body := read(acmeToken, "/api/admin/v1/audit"); strings.Contains(body, "globexop") {
		t.Error("acme's audit trail contains globex's operator")
	}
	if body := read(globexToken, "/api/admin/v1/audit"); strings.Contains(body, "acmeop") {
		t.Error("globex's audit trail contains acme's operator")
	}
}

// The platform role administers tenants and reads no customer content, and both halves are
// asserted. Either one alone would be a role that is called separated and is not.
func TestThePlatformRoleAdministersTenantsAndSeesNoCustomerContent(t *testing.T) {
	ctx := context.Background()
	const platformToken = "platform-operator-token-01234"
	const tenantOpToken = "tenant-operator-token-0123456"

	tenants := tenant.NewStore(pool)
	owner := fmt.Sprintf("owner-%d", time.Now().UnixNano())
	if _, err := tenants.Create(ctx, owner, owner, "platform"); err != nil {
		t.Fatal(err)
	}
	ops, err := admin.ParseOperators(
		"root:" + platformToken + ":platform," +
			"alex:" + tenantOpToken + ":operator:" + owner)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	admin.NewServer(admin.NewStore(pool), ticket.NewStore(pool), ops, corsFor(t),
		retention.NewStore(pool), handoffFor(pool), knowledgeFor(pool),
		feedback.NewStore(pool), tenants).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// Every route that touches a tenant's data is refused for the platform account,
	// reads included. This is the half that is easy to leave out.
	for _, path := range []string{
		"/api/admin/v1/overview",
		"/api/admin/v1/conversations",
		"/api/admin/v1/conversations/anything",
		"/api/admin/v1/tickets",
		"/api/admin/v1/knowledge",
		"/api/admin/v1/feedback",
		"/api/admin/v1/audit",
	} {
		if got := do(t, server, "GET", path, platformToken, "").StatusCode; got != http.StatusForbidden {
			t.Errorf("the platform account got %d for %s, want 403", got, path)
		}
	}
	// And it can find out what it is, which is the one authenticated route with no tenant.
	if got := do(t, server, "GET", "/api/admin/v1/whoami", platformToken, "").StatusCode; got != http.StatusOK {
		t.Errorf("the platform account cannot read whoami: %d", got)
	}

	// The other direction: a tenant operator cannot administer tenants.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/admin/v1/tenants", ""},
		{"POST", "/api/admin/v1/tenants", `{"id":"sneaky-tenant","name":"Sneaky"}`},
		{"GET", "/api/admin/v1/tenants/" + owner + "/keys", ""},
		{"POST", "/api/admin/v1/tenants/" + owner + "/keys", `{"label":"mine"}`},
	} {
		if got := do(t, server, c.method, c.path, tenantOpToken, c.body).StatusCode; got != http.StatusForbidden {
			t.Errorf("a tenant operator got %d for %s %s, want 403", got, c.method, c.path)
		}
	}

	// Creating a tenant and issuing it a key, which is the whole reason the role exists.
	made := fmt.Sprintf("new-%d", time.Now().UnixNano())
	resp := do(t, server, "POST", "/api/admin/v1/tenants", platformToken,
		fmt.Sprintf(`{"id":%q,"name":"New Customer"}`, made))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("creating a tenant returned %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = do(t, server, "POST", "/api/admin/v1/tenants/"+made+"/keys", platformToken,
		`{"label":"their integration"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issuing a key returned %d", resp.StatusCode)
	}
	if cache := resp.Header.Get("Cache-Control"); cache != "no-store" {
		t.Errorf("a credential came back with Cache-Control %q", cache)
	}
	var issued struct {
		KeyID  string `json:"keyId"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if issued.Secret == "" {
		t.Fatal("the issued key came back without its secret; it exists nowhere else")
	}
	if got, err := tenants.Resolve(ctx, issued.Secret); err != nil || got != made {
		t.Errorf("the issued key resolves to %q (%v), want %q", got, err, made)
	}

	// Listing the keys afterwards must not hand the secret back a second time.
	resp = do(t, server, "GET", "/api/admin/v1/tenants/"+made+"/keys", platformToken, "")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), issued.Secret) {
		t.Error("listing a tenant's keys returned the secret")
	}
	if !strings.Contains(string(body), issued.KeyID) {
		t.Errorf("the issued key is not in the list: %s", body)
	}

	// Revoking through another tenant's URL is a 404, on the same rule as reading a ticket
	// by number: a key_id is not the authorisation.
	if got := do(t, server, "DELETE",
		"/api/admin/v1/tenants/"+owner+"/keys/"+issued.KeyID, platformToken, "").StatusCode; got != http.StatusNotFound {
		t.Errorf("revoking a key through another tenant's URL returned %d, want 404", got)
	}
	if got, err := tenants.Resolve(ctx, issued.Secret); err != nil || got != made {
		t.Error("the key was revoked through the wrong tenant's URL after all")
	}

	// And through its own, which then stops it answering.
	if got := do(t, server, "DELETE",
		"/api/admin/v1/tenants/"+made+"/keys/"+issued.KeyID, platformToken, "").StatusCode; got != http.StatusNoContent {
		t.Errorf("revoking the key returned %d", got)
	}
	if _, err := tenants.Resolve(ctx, issued.Secret); !errors.Is(err, tenant.ErrNoSuchKey) {
		t.Errorf("a revoked key still resolves: %v", err)
	}

	// Every one of those is audited against the tenant administered, not against the
	// platform account's (which has none).
	var actions int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit WHERE tenant_id = $1 AND actor = 'root'`,
		made).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions < 3 {
		t.Errorf("%d audit rows for the tenant that was created, keyed and revoked", actions)
	}
}

// A platform account that also names a tenant is the combination the role exists to avoid.
func TestAPlatformOperatorCannotAlsoBelongToATenant(t *testing.T) {
	_, err := admin.ParseOperators("root:" + operatorToken + ":platform:acme")
	if err == nil {
		t.Fatal("a platform operator with a tenant was accepted")
	}
	if !strings.Contains(err.Error(), "platform") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// Every method the routes are registered with has to be in the preflight's
// Access-Control-Allow-Methods, or the browser refuses the real request.
//
// The header said "GET, PATCH, OPTIONS" for as long as it existed. Replying to a ticket,
// saving or deleting a knowledge entry, publishing, activating a version, judging an
// answer, clearing feedback, erasing a conversation and every tenant action were all
// blocked from a browser — and none of it showed up here, because the CORS tests assert the
// *origin* rules and the API tests are Go clients that do not preflight.
//
// Read off the route table rather than compared against a list written here: a list written
// here is a third copy of the same thing, and it would have been written from the same
// reading of the code that produced the bug.
func TestEveryMethodTheRoutesUseIsAllowedByThePreflight(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("([A-Z]+) `).FindAllStringSubmatch(string(raw), -1)
	if len(routes) < 10 {
		t.Fatalf("read %d routes out of api.go; this test is no longer reading the table",
			len(routes))
	}

	ops, err := admin.ParseOperators("alex:" + operatorToken + ":operator")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	admin.NewServer(admin.NewStore(pool), ticket.NewStore(pool), ops, corsFor(t),
		retention.NewStore(pool), handoffFor(pool), knowledgeFor(pool),
		feedback.NewStore(pool), tenant.NewStore(pool)).Routes(mux)
	live := httptest.NewServer(mux)
	t.Cleanup(live.Close)

	seen := map[string]bool{}
	for _, m := range routes {
		method := m[1]
		if method == "OPTIONS" || seen[method] {
			continue
		}
		seen[method] = true

		// The preflight the browser would send before the real request.
		req, err := http.NewRequest("OPTIONS", live.URL+"/api/admin/v1/overview", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", uiOrigin)
		req.Header.Set("Access-Control-Request-Method", method)
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		resp, err := live.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		allow := resp.Header.Get("Access-Control-Allow-Methods")
		resp.Body.Close()

		if !strings.Contains(allow, method) {
			t.Errorf("routes are registered with %s and the preflight allows %q; a browser "+
				"refuses every one of those requests", method, allow)
		}
	}
	if len(seen) < 4 {
		t.Errorf("only %d distinct methods found in the route table (%v); the surface has "+
			"fewer verbs than it did, or this test has stopped reading them", len(seen), seen)
	}
}
