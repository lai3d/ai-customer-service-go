package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/llm"
)

// These tests drive the real clients against a fake provider, because the thing being
// asserted is a property of the clients and cannot be checked above them.
//
// A stub implementing llm.Client can return whatever it likes on an error path, and a
// suite built on one will happily encode a contract that no real client satisfies -- the
// test passes, its subject is the fixture, and the production code is never executed.
// That is exactly what happened here: the service-level stub returned usage alongside an
// error, both real clients returned a zero Result, and nothing failed.

const inputTokensOnTheWire = 1842

// anthropicStream serves a stream that reports its input tokens up front, streams one
// visible token, and then does whatever the test asks.
func anthropicStream(t *testing.T, after func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		frames := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":` + fmt.Sprint(inputTokensOnTheWire) + `,"output_tokens":1}}}

`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Thirty "}}

`,
		}
		for _, frame := range frames {
			fmt.Fprint(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if after != nil {
			after(w)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func anthropicClient(server *httptest.Server) llm.Client {
	return llm.NewAnthropic(llm.Options{
		APIKey: "test-key", Model: "claude-opus-5", BaseURL: server.URL,
		MaxTokens: 1024, MaxAttempts: 1, RequestTimeout: 5 * time.Second,
	})
}

func request() llm.Request {
	return llm.Request{
		System:    "you are a test",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "how long do I have?"}},
		MaxTokens: 1024,
	}
}

// The client-disconnect path, and the most likely mid-stream failure in production.
//
// Anthropic reports the input count at message_start, before a single token of the
// answer, so by the time the consumer gives up those tokens are already billed. A client
// that returned a zero Result here would drop them one layer below the service comment
// promising they are recorded -- which is the shape of the bug the Java implementation
// shipped under the name "aborted-stream accounting".
func TestAnthropicKeepsTheUsageItWasAlreadyToldWhenTheConsumerGivesUp(t *testing.T) {
	server := anthropicStream(t, func(w http.ResponseWriter) {
		// Keep the response open; the consumer is the one that stops.
		time.Sleep(50 * time.Millisecond)
	})
	client := anthropicClient(server)

	consumerGaveUp := errors.New("the client disconnected")
	result, err := client.Stream(context.Background(), request(), func(string) error {
		return consumerGaveUp
	})

	if !errors.Is(err, consumerGaveUp) {
		t.Fatalf("error is %v, want the consumer's own error", err)
	}
	if result.Usage.InputTokens != inputTokensOnTheWire {
		t.Errorf("usage reports %d input tokens, want %d -- the provider had already "+
			"billed them before the stream was abandoned",
			result.Usage.InputTokens, inputTokensOnTheWire)
	}
	if result.Native != nil {
		t.Error("a half-streamed assistant turn must not be offered back to the provider")
	}
}

// The same requirement on the transport-failure path rather than the consumer's.
func TestAnthropicKeepsItsUsageWhenTheStreamIsCutOff(t *testing.T) {
	server := anthropicStream(t, func(w http.ResponseWriter) {
		// Hijack and close, so the client sees a truncated response rather than a
		// clean end of stream.
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err == nil {
				conn.Close()
			}
		}
	})
	client := anthropicClient(server)

	var streamed string
	result, err := client.Stream(context.Background(), request(), func(text string) error {
		streamed += text
		return nil
	})

	if err == nil {
		t.Fatal("a truncated stream returned no error")
	}
	if streamed != "Thirty " {
		t.Errorf("forwarded %q before the cut, want %q", streamed, "Thirty ")
	}
	if result.Usage.InputTokens != inputTokensOnTheWire {
		t.Errorf("usage reports %d input tokens, want %d", result.Usage.InputTokens, inputTokensOnTheWire)
	}
}

// The other half of the same rule, and the reason it is a rule about protocols rather
// than about care: on the OpenAI protocol usage arrives in a single final chunk, so an
// aborted call has nothing to report and reporting zero is correct. One protocol loses
// real tokens on an abort and the other loses nothing; a client that returned early
// would treat them the same and be wrong about one of them.
func TestTheOpenAIProtocolHasNoUsageToKeepWhenAStreamIsAbandoned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,`+
			`"model":"gpt-5-2025-08-07","choices":[{"index":0,"delta":{"content":"Thirty "},`+
			`"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		// The usage-bearing final chunk never arrives.
	}))
	t.Cleanup(server.Close)

	client := llm.NewOpenAI(llm.Options{
		APIKey: "test-key", Model: "gpt-5", BaseURL: server.URL,
		MaxTokens: 1024, MaxAttempts: 1, RequestTimeout: 5 * time.Second,
	})

	consumerGaveUp := errors.New("the client disconnected")
	result, err := client.Stream(context.Background(), request(), func(string) error {
		return consumerGaveUp
	})

	if !errors.Is(err, consumerGaveUp) {
		t.Fatalf("error is %v, want the consumer's own error", err)
	}
	if result.Usage.InputTokens != 0 || result.Usage.OutputTokens != 0 {
		t.Errorf("usage is %+v, want zero: this protocol had not reported any yet",
			result.Usage)
	}
	if result.Model != "gpt-5-2025-08-07" {
		t.Errorf("model is %q; metrics key on what the provider reported, which arrives "+
			"on the first chunk and survives an abort", result.Model)
	}
}

// There is deliberately no test for `defer stream.Close()`.
//
// The clients close the stream on every path, which is correct resource discipline: the
// SDK does not close the response body when a stream ends in an error. But two attempts
// at a test for it both passed against the *unfixed* code, because what released the
// response was the request timeout, not the close -- the first version measured a 5s
// timeout and the second a 60s one, and neither distinguished the fix from its absence.
//
// A test that passes either way is the exact failure this repository keeps finding, so it
// was deleted rather than kept for the look of it. The close stays on the strength of the
// argument; it is not evidence-backed, and saying so is better than a green assertion
// that means nothing. Demonstrating it would need the connection pool constrained to one
// connection and a second request proving the first was released, which needs an http
// client injected through llm.Options -- worth doing if this ever matters more than it
// does now.
