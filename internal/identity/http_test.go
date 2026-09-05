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
)

// answering records which conversation the chat service was asked to run a turn for, so
// these tests can tell "refused" from "ran it anyway and returned an error afterwards".
type answering struct{ conversations []string }

func (a *answering) Turn(_ context.Context, id, _ string, emit func(chat.Event)) error {
	a.conversations = append(a.conversations, id)
	emit(chat.Event{Type: chat.EventMessage, Text: "ok"})
	return nil
}

func serveWithSessions(t *testing.T) (*httptest.Server, *answering) {
	t.Helper()
	turner := &answering{}
	mux := http.NewServeMux()
	httpapi.NewServer(turner, config.Chat{
		MaxMessageLength: 4000, MaxConversationIDLength: 64,
		KeepAliveInterval: 50 * time.Millisecond,
	}, &httpapi.Identity{
		Sessions:      identity.NewSessions(pool, time.Hour),
		Conversations: identity.NewConversations(pool),
	}).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, turner
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
