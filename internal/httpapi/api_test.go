package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
)

type fakeTurner struct {
	calls  atomic.Int32
	events []chat.Event
	err    error
	delay  time.Duration
}

func (f *fakeTurner) Turn(ctx context.Context, _, _ string, emit func(chat.Event)) error {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, e := range f.events {
		emit(e)
	}
	return f.err
}

func testConfig() config.Chat {
	return config.Chat{
		MaxMessageLength:        4000,
		MaxConversationIDLength: 64,
		KeepAliveInterval:       50 * time.Millisecond,
	}
}

func serve(t *testing.T, turner httpapi.Turner) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	httpapi.NewServer(turner, testConfig(), obs.NewMetrics(), nil, nil).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, server *httptest.Server, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Both limits cost nothing to enforce at the edge, and are a 500 from a database
// constraint if they are not enforced at all.
func TestRequestsAreRejectedBeforeAnyModelCall(t *testing.T) {
	turner := &fakeTurner{}
	server := serve(t, turner)

	cases := []struct{ name, body string }{
		{"blank message", `{"message":"   "}`},
		{"missing message", `{}`},
		{"oversized message", `{"message":"` + strings.Repeat("x", 4001) + `"}`},
		{"oversized conversation id", `{"conversationId":"` + strings.Repeat("x", 65) + `","message":"hi"}`},
		{"malformed json", `{"message":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, server, "/api/v1/chat", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status is %d, want 400", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("content type is %q, want application/problem+json", got)
			}
		})
	}
	if turner.calls.Load() != 0 {
		t.Errorf("the model was reached %d times by requests that should never get there",
			turner.calls.Load())
	}
}

// A client should be able to tell "retry" from "this will never work" without parsing
// prose, and a conversation that has spent its budget is neither.
func TestFailuresMapToStatusesAClientCanActOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"rate limited", &llm.Error{StatusCode: 429, Retryable: true, Err: errors.New("slow down")}, http.StatusServiceUnavailable},
		{"overloaded", &llm.Error{StatusCode: 529, Retryable: true, Err: errors.New("overloaded")}, http.StatusServiceUnavailable},
		{"bad credentials", &llm.Error{StatusCode: 401, Retryable: false, Err: errors.New("nope")}, http.StatusBadGateway},
		{"rejected request", &llm.Error{StatusCode: 400, Retryable: false, Err: errors.New("nope")}, http.StatusBadGateway},
		{"budget reached", &cost.ErrExceeded{Spent: 10, Limit: 5}, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, &fakeTurner{err: tc.err})
			resp := post(t, server, "/api/v1/chat", `{"message":"hello"}`)
			if resp.StatusCode != tc.want {
				t.Errorf("status is %d, want %d", resp.StatusCode, tc.want)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if detail, _ := body["detail"].(string); strings.Contains(detail, "nope") {
				t.Errorf("the provider's own error text reached the client: %q", detail)
			}
		})
	}
}

func TestAConversationIdComesBackOnEveryResponse(t *testing.T) {
	server := serve(t, &fakeTurner{events: []chat.Event{{Type: chat.EventMessage, Text: "hi"}}})

	resp := post(t, server, "/api/v1/chat", `{"message":"hello"}`)
	if got := resp.Header.Get(httpapi.ConversationIDHeader); got == "" {
		t.Error("no conversation id header; a client cannot continue the conversation")
	}

	resp = post(t, server, "/api/v1/chat", `{"conversationId":"abc-123","message":"hello"}`)
	if got := resp.Header.Get(httpapi.ConversationIDHeader); got != "abc-123" {
		t.Errorf("conversation id header is %q, want the one supplied", got)
	}
}

// The stream carries typed events rather than bare tokens, and retrieval arrives before
// the first token.
func TestTheStreamCarriesTypedEventsInOrder(t *testing.T) {
	server := serve(t, &fakeTurner{events: []chat.Event{
		{Type: chat.EventRetrieval, Passages: []chat.Passage{{EntryID: "returns-window", Score: 0.9}}},
		{Type: chat.EventTool, Tool: &chat.ToolEvent{Name: "lookup_order_status", Outcome: "found"}},
		{Type: chat.EventMessage, Text: "Thirty "},
		{Type: chat.EventMessage, Text: "days."},
		{Type: chat.EventUsage, Usage: &chat.UsageEvent{Model: "m", ModelCalls: 2, InputTokens: 10}},
	}})

	names := readEventNames(t, server, `{"message":"hello"}`)
	want := []string{"retrieval", "tool", "message", "message", "usage"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("events are %v, want %v", names, want)
	}
}

// A failure after the response is committed cannot change the status code, so it has to
// arrive as a terminal event. Otherwise a client cannot tell an apology written by the
// model from a transport failure.
func TestAFailureAfterTheFirstTokenArrivesAsAnErrorEvent(t *testing.T) {
	server := serve(t, &fakeTurner{
		events: []chat.Event{{Type: chat.EventMessage, Text: "Thirty "}},
		err:    &llm.Error{StatusCode: 500, Retryable: true, Err: errors.New("upstream died")},
	})

	names := readEventNames(t, server, `{"message":"hello"}`)
	if len(names) == 0 || names[len(names)-1] != "error" {
		t.Errorf("events are %v, want a terminal error event", names)
	}
}

// SSE connections are legitimately idle between the request and the first token, and
// proxies close idle connections.
func TestTheStreamIsKeptAliveWhileTheModelThinks(t *testing.T) {
	server := serve(t, &fakeTurner{
		delay:  200 * time.Millisecond,
		events: []chat.Event{{Type: chat.EventMessage, Text: "late"}},
	})

	resp, err := http.Post(server.URL+"/api/v1/chat/stream", "application/json",
		strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var sawKeepAlive bool
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), ": keep-alive") {
			sawKeepAlive = true
		}
		if strings.Contains(scanner.Text(), `"late"`) {
			break
		}
	}
	if !sawKeepAlive {
		t.Error("no keep-alive frame arrived during a 200ms wait with a 50ms interval")
	}
}

// Running the turn twice would mean two model calls, two bills, and two sets of
// messages written to memory, while the response still looked correct.
func TestTheTurnRunsExactlyOncePerStreamedRequest(t *testing.T) {
	turner := &fakeTurner{events: []chat.Event{{Type: chat.EventMessage, Text: "hi"}}}
	server := serve(t, turner)

	readEventNames(t, server, `{"message":"hello"}`)

	if got := turner.calls.Load(); got != 1 {
		t.Errorf("the turn ran %d times, want 1", got)
	}
}

func readEventNames(t *testing.T, server *httptest.Server, body string) []string {
	t.Helper()
	resp, err := http.Post(server.URL+"/api/v1/chat/stream", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type is %q, want text/event-stream", got)
	}

	var names []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
			names = append(names, name)
		}
	}
	return names
}
