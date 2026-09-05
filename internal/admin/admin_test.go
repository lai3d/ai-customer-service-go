package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/admin"
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
	admin.NewServer(admin.NewStore(pool), tickets, ops).Routes(mux)
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
	for _, path := range []string{"/admin/", "/api/admin/v1/overview", "/api/admin/v1/tickets"} {
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
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{
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
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{
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
		INSERT INTO turn (id, conversation_id, started_at, outcome, question)
		VALUES ('turn-audited', 'audited-conversation', now(), 'completed', 'where is my order?')`); err != nil {
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
// displayed by design.
func TestTheAdminPageNeverTurnsAStringIntoMarkup(t *testing.T) {
	raw, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	sinks := map[string]*regexp.Regexp{
		"innerHTML assignment": regexp.MustCompile(`\.innerHTML\s*[+]?=`),
		"outerHTML assignment": regexp.MustCompile(`\.outerHTML\s*[+]?=`),
		"insertAdjacentHTML":   regexp.MustCompile(`insertAdjacentHTML\s*\(`),
		"document.write":       regexp.MustCompile(`document\.write\s*\(`),
		"eval":                 regexp.MustCompile(`[^.\w]eval\s*\(`),
		"Function constructor": regexp.MustCompile(`new\s+Function\s*\(`),
	}
	for name, pattern := range sinks {
		if loc := pattern.FindStringIndex(page); loc != nil {
			t.Errorf("the admin page uses %s at line %d",
				name, 1+strings.Count(page[:loc[0]], "\n"))
		}
	}
	// The token is a credential for every conversation in the database. localStorage
	// outlives the tab; sessionStorage does not.
	//
	// Matched as a *use*, not as a mention: the page's own comment explains why it uses
	// sessionStorage instead, and a substring check flagged that comment. Same shape as
	// the innerHTML check on the demo page, which had to learn the same lesson.
	if regexp.MustCompile(`\blocalStorage\s*\.`).MatchString(page) {
		t.Error("the operator token must not go in localStorage: it outlives the tab")
	}
	if !regexp.MustCompile(`\bsessionStorage\s*\.`).MatchString(page) {
		t.Error("the page does not use sessionStorage; where is the token kept?")
	}
}

// A refused attempt is exactly what an audit trail is for. This was missing until a live
// walk through the operator workflow showed a 403 leaving no trace at all -- the deny
// path returned before anything recorded it.
func TestARefusedActionIsAudited(t *testing.T) {
	server, tickets := serve(t)
	created, _, err := tickets.Create(context.Background(), ticket.CreateRequest{
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
