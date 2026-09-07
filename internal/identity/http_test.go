package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/identity"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// answering records which conversation the chat service was asked to run a turn for, so
// these tests can tell "refused" from "ran it anyway and returned an error afterwards".
type answering struct{ conversations []string }

func (a *answering) Turn(_ context.Context, id, _ string, emit func(chat.Event)) error {
	a.conversations = append(a.conversations, id)
	emit(chat.Event{Type: chat.EventMessage, Text: "ok"})
	// A real turn reports what it spent, and the day's ledger is fed from this event. A
	// stub that stays silent would let the spend path be removed with every test still
	// green -- which is how this stub was written the first time.
	emit(chat.Event{Type: chat.EventUsage, Usage: &chat.UsageEvent{
		Model: "test-model", ModelCalls: 1, InputTokens: 100, OutputTokens: 20,
	}})
	return nil
}

// serveEdge builds the edge the way cmd/server does, with whatever limits and registry
// the caller wants to look at afterwards. The two helpers below are the common cases.
func serveEdge(t *testing.T, limits *identity.Limits, metrics *obs.Metrics) (*httptest.Server, *answering) {
	t.Helper()
	turner := &answering{}
	mux := http.NewServeMux()
	httpapi.NewServer(turner, config.Chat{
		MaxMessageLength: 4000, MaxConversationIDLength: 64,
		KeepAliveInterval: 50 * time.Millisecond,
	}, metrics, &httpapi.Identity{
		Sessions:      identity.NewSessions(pool, time.Hour),
		Conversations: identity.NewConversations(pool),
		Limits:        limits,
	}, nil).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, turner
}

func serveWithSessions(t *testing.T) (*httptest.Server, *answering) {
	t.Helper()
	return serveEdge(t, nil, obs.NewMetrics())
}

func newSession(t *testing.T, server *httptest.Server) string {
	t.Helper()
	resp, err := server.Client().Post(server.URL+"/api/v1/session", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/session returned %d", resp.StatusCode)
	}
	if cache := resp.Header.Get("Cache-Control"); cache != "no-store" {
		t.Errorf("a credential was returned with Cache-Control %q", cache)
	}
	var body struct{ Token string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" {
		t.Fatal("the session response carried no token")
	}
	return body.Token
}

func chatAs(t *testing.T, server *httptest.Server, token, conversation, path string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": "hello", "conversationId": conversation})
	req, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The body is drained before returning, and that is load-bearing for the streaming
	// path: Do returns as soon as the headers arrive, so a test that counts turns
	// afterwards is racing the goroutine that runs them. The first version of this
	// measured "before" while the previous turn was still in flight and reported that a
	// refused request had run.
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp
}

// The whole point, at the edge: one customer's conversation id is useless to another.
func TestAnotherSessionsConversationIsNotFound(t *testing.T) {
	for _, path := range []string{"/api/v1/chat", "/api/v1/chat/stream"} {
		t.Run(path, func(t *testing.T) {
			server, turner := serveWithSessions(t)
			mine := newSession(t, server)
			theirs := newSession(t, server)

			// A first turn with no id: the server issues one and binds it.
			resp := chatAs(t, server, mine, "", path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("the owner's first turn returned %d", resp.StatusCode)
			}
			id := resp.Header.Get(httpapi.ConversationIDHeader)
			if id == "" {
				t.Fatal("no conversation id came back")
			}

			before := len(turner.conversations)
			resp = chatAs(t, server, theirs, id, path)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("another session got %d for someone else's conversation, want 404",
					resp.StatusCode)
			}
			// A 404 that still ran the turn has leaked the answer into the other
			// customer's history, which is most of the damage.
			if len(turner.conversations) != before {
				t.Errorf("the turn ran anyway: %v", turner.conversations)
			}

			// And the owner can still continue it.
			if resp := chatAs(t, server, mine, id, path); resp.StatusCode != http.StatusOK {
				t.Errorf("the owner was refused their own conversation: %d", resp.StatusCode)
			}
		})
	}
}

// An id that exists and an id that does not must be indistinguishable, or the endpoint is
// an oracle for which conversations exist.
func TestAnUnknownIdAndSomeoneElsesIdLookTheSame(t *testing.T) {
	server, _ := serveWithSessions(t)
	mine := newSession(t, server)
	theirs := newSession(t, server)

	resp := chatAs(t, server, mine, "", "/api/v1/chat")
	id := resp.Header.Get(httpapi.ConversationIDHeader)

	owned := chatAs(t, server, theirs, id, "/api/v1/chat")
	unknown := chatAs(t, server, theirs, "definitely-not-a-conversation", "/api/v1/chat")
	if owned.StatusCode != unknown.StatusCode {
		t.Errorf("someone else's conversation returned %d and an unknown one %d; the "+
			"difference says which ids exist", owned.StatusCode, unknown.StatusCode)
	}

	var a, b map[string]any
	json.NewDecoder(owned.Body).Decode(&a)
	json.NewDecoder(unknown.Body).Decode(&b)
	if a["title"] != b["title"] || a["detail"] != b["detail"] {
		t.Errorf("the two refusals read differently: %v vs %v", a, b)
	}
}

func TestWithoutASessionThereIsNoTurn(t *testing.T) {
	server, turner := serveWithSessions(t)
	for _, token := range []string{"", "not-a-real-token"} {
		resp := chatAs(t, server, token, "", "/api/v1/chat")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q returned %d, want 401", token, resp.StatusCode)
		}
	}
	if len(turner.conversations) != 0 {
		t.Errorf("a turn ran without a session: %v", turner.conversations)
	}
}

// The streaming path writes its headers and a 200 early, so an authorisation failure has
// to be decided before any of that. Reported as an error *event* it would reach the page
// as something to render rather than as a refusal.
func TestTheStreamRefusesBeforeItStartsStreaming(t *testing.T) {
	server, _ := serveWithSessions(t)
	resp := chatAs(t, server, "", "", "/api/v1/chat/stream")
	if got := resp.Header.Get("Content-Type"); got == "text/event-stream" {
		t.Errorf("the refusal was sent as a stream (%s), not as a problem document", got)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the stream returned %d for a request with no session", resp.StatusCode)
	}
}

func serveWithLimits(t *testing.T, limits *identity.Limits) (*httptest.Server, *answering) {
	t.Helper()
	return serveEdge(t, limits, obs.NewMetrics())
}

// A per-conversation token budget already existed, and it is not this: conversation ids
// are free, so anyone who wants more of one starts another conversation. This bounds the
// subject.
func TestASubjectAskingTooFastIsRefusedAndToldWhenToReturn(t *testing.T) {
	limits := identity.NewLimits(pool)
	limits.TurnsPerMinute = 3
	server, turner := serveWithLimits(t, limits)
	token := newSession(t, server)

	for i := 1; i <= 3; i++ {
		if resp := chatAs(t, server, token, "", "/api/v1/chat"); resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d returned %d within the limit", i, resp.StatusCode)
		}
	}
	ran := len(turner.conversations)

	resp := chatAs(t, server, token, "", "/api/v1/chat")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the fourth turn in a minute returned %d, want 429", resp.StatusCode)
	}
	// A limit that refuses after running the turn has cost the money it exists to save.
	if len(turner.conversations) != ran {
		t.Error("the refused turn ran anyway")
	}
	// Without this a client backs off by guessing, and the guesses that get written are
	// "immediately" and "a minute".
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After")
	}

	// Another subject is unaffected: the bucket is per subject, not global.
	other := newSession(t, server)
	if resp := chatAs(t, server, other, "", "/api/v1/chat"); resp.StatusCode != http.StatusOK {
		t.Errorf("a different session was refused (%d); the limit is not per subject",
			resp.StatusCode)
	}
}

// The endpoint that mints subjects has to be bounded, or a per-subject limit is a limit on
// how fast one subject can be discarded.
func TestSessionsCannotBeMintedWithoutLimit(t *testing.T) {
	limits := identity.NewLimits(pool)
	limits.SessionsPerHourPerIP = 2
	server, _ := serveWithLimits(t, limits)

	for i := 1; i <= 2; i++ {
		resp, err := server.Client().Post(server.URL+"/api/v1/session", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("session %d returned %d", i, resp.StatusCode)
		}
	}
	resp, err := server.Client().Post(server.URL+"/api/v1/session", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the third session from one address returned %d, want 429", resp.StatusCode)
	}
}

// The ceiling a new conversation id cannot reset.
func TestWhenTheDayIsSpentTheServiceSaysSoRatherThanSlowingDown(t *testing.T) {
	ctx := context.Background()
	limits := identity.NewLimits(pool)
	limits.DailyTokenBudget = 1_000_000_000
	server, turner := serveWithLimits(t, limits)
	token := newSession(t, server)

	if resp := chatAs(t, server, token, "", "/api/v1/chat"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a turn under the budget returned %d", resp.StatusCode)
	}
	ran := len(turner.conversations)

	if err := limits.RecordSpend(ctx, limits.DailyTokenBudget); err != nil {
		t.Fatal(err)
	}
	resp := chatAs(t, server, token, "", "/api/v1/chat")
	// 503, not 429: this is the service saying no, and telling the customer to slow down
	// would be a lie about whose problem it is.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a turn over the daily budget returned %d, want 503", resp.StatusCode)
	}
	if len(turner.conversations) != ran {
		t.Error("the turn ran anyway, which is the money the budget exists to not spend")
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on the budget refusal")
	}

	// And a fresh conversation does not reset it, which is what the per-conversation
	// budget could never do.
	if resp := chatAs(t, server, token, "", "/api/v1/chat"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a new conversation got past the daily budget: %d", resp.StatusCode)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM daily_spend`); err != nil {
		t.Fatal(err)
	}
}

// The ledger the budget reads has to be fed by the turn, not only by tests calling
// RecordSpend. Without this the budget is a ceiling on a number that never rises.
func TestATurnAddsWhatItSpentToTheDay(t *testing.T) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM daily_spend`); err != nil {
		t.Fatal(err)
	}
	limits := identity.NewLimits(pool)
	limits.DailyTokenBudget = 1_000_000
	server, _ := serveWithLimits(t, limits)
	token := newSession(t, server)

	for _, path := range []string{"/api/v1/chat", "/api/v1/chat/stream"} {
		before, err := limits.UsedToday(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if resp := chatAs(t, server, token, "", path); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", path, resp.StatusCode)
		}
		// The spend is recorded on a detached context after the response, so it can land
		// just after the client is answered.
		var after int64
		for i := 0; i < 50; i++ {
			if after, err = limits.UsedToday(ctx); err != nil {
				t.Fatal(err)
			}
			if after > before {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if after != before+120 {
			t.Errorf("%s moved the day's total from %d to %d; the turn reported 120 tokens",
				path, before, after)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM daily_spend`); err != nil {
		t.Fatal(err)
	}
}

// Every refusal at this edge answers before chat.Service.Turn runs, so none of the turn
// meters move for it: the service can refuse every customer it has while chat_turns_total
// stays flat and every dashboard reads green. That was the state of this repository for a
// day, written down in docs/observability.md as the largest hole in it.
//
// All four reasons are driven through the real edge against a real Postgres rather than
// asserted against the counter directly. A test that incremented the counter itself would
// prove that a counter counts; what is in doubt is whether the four *refusal paths* reach
// it, and each of them was seen to fail here with its own increment removed.
func TestEveryRefusalAtTheEdgeIsCounted(t *testing.T) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM daily_spend`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM daily_spend`); err != nil {
			t.Fatal(err)
		}
	})

	limits := identity.NewLimits(pool)
	limits.TurnsPerMinute = 2
	limits.DailyTokenBudget = 1_000_000_000
	metrics := obs.NewMetrics()
	server, _ := serveEdge(t, limits, metrics)

	// No session at all.
	if resp := chatAs(t, server, "", "", "/api/v1/chat"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a request with no session returned %d, want 401", resp.StatusCode)
	}

	// Someone else's conversation. The owner's first turn spends one of their two.
	owner, stranger := newSession(t, server), newSession(t, server)
	first := chatAs(t, server, owner, "", "/api/v1/chat")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("the owner's first turn returned %d", first.StatusCode)
	}
	id := first.Header.Get(httpapi.ConversationIDHeader)
	if resp := chatAs(t, server, stranger, id, "/api/v1/chat"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a stranger got %d for someone else's conversation, want 404", resp.StatusCode)
	}

	// The per-subject limit. The stranger has already spent one of their two on the
	// refusal above, because the limiter runs before ownership is checked.
	if resp := chatAs(t, server, stranger, "", "/api/v1/chat"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the stranger's second turn returned %d, still inside the limit", resp.StatusCode)
	}
	if resp := chatAs(t, server, stranger, "", "/api/v1/chat"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the third turn in a minute returned %d, want 429", resp.StatusCode)
	}

	// The day's budget, on a subject that has not been rate limited.
	if err := limits.RecordSpend(ctx, limits.DailyTokenBudget); err != nil {
		t.Fatal(err)
	}
	if resp := chatAs(t, server, newSession(t, server), "", "/api/v1/chat"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a turn over the day's budget returned %d, want 503", resp.StatusCode)
	}

	for _, reason := range []string{"no_session", "not_yours", "rate_limited", "daily_budget"} {
		if got := testutil.ToFloat64(metrics.Refusals.WithLabelValues(reason)); got != 1 {
			t.Errorf("chat_edge_refusals_total{reason=%q} is %v after one such refusal, want 1",
				reason, got)
		}
	}
	// A fifth series would be a label value no alert names and nothing here expects --
	// including one refusal counted twice under two spellings.
	if series := testutil.CollectAndCount(metrics.Refusals); series != 4 {
		t.Errorf("chat_edge_refusals_total has %d series; this test caused four refusals "+
			"and there are four reasons", series)
	}
}
