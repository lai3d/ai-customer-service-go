package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/prometheus/client_golang/prometheus"
)

// Everything here drives the two real clients against two httptest providers, for the
// reason stream_test.go gives at length: a stub implementing llm.Client can return
// whatever an error path is supposed to return, and a suite built on one encodes a
// contract no real client satisfies. That is not hypothetical in this repository -- it is
// how the abandoned-usage defect shipped.
//
// It matters twice as much for failover, because the decision to spend money at a second
// provider is made *from* the error the first one returned. A stub choosing which error
// to hand back would be choosing the answer.

const (
	// What the primary reports at message_start, before it dies. Anthropic bills the
	// input count from that frame whatever happens next.
	abandonedInputTokens = 1842
	// What the secondary reports for the call that actually answers.
	secondaryInputTokens  = 97
	secondaryOutputTokens = 42
)

// --- providers on the wire ----------------------------------------------------------

// counted wraps a handler so a test can assert that the second provider was *not* asked.
// A failover that should not have happened is invisible in the result -- the primary's
// error comes back either way -- and only the request count distinguishes it.
type counted struct {
	requests atomic.Int64
	server   *httptest.Server
	// Closed at the end of the test, before the server is. A handler that holds a
	// response open to be interrupted has to wait on this as well as on the request's
	// own context: httptest.Server.Close waits for its handlers to return, and a handler
	// still blocked when the test ends hangs the whole package until the test binary's
	// own timeout. Observed here rather than anticipated -- Close reported
	// "blocked in Close after 5 seconds" and the package then ran for two minutes.
	release chan struct{}
}

func (c *counted) URL() string  { return c.server.URL }
func (c *counted) Count() int64 { return c.requests.Load() }
func (c *counted) hit()         { c.requests.Add(1) }

// serve registers the cleanups in the order they have to run. They are LIFO, so the
// release is registered last and therefore closed first.
func (c *counted) serve(t *testing.T, handler http.HandlerFunc) *counted {
	t.Helper()
	c.release = make(chan struct{})
	c.server = httptest.NewServer(handler)
	t.Cleanup(c.server.Close)
	t.Cleanup(func() { close(c.release) })
	return c
}

// hold keeps a response open until the client gives up or the test ends.
func (c *counted) hold(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-c.release:
	}
}

// anthropicPrimary serves an Anthropic stream that announces its input tokens and then
// does whatever the test asks. `text` is streamed before `after` runs; an empty string
// streams nothing, which is the case failover is allowed to act on.
func anthropicPrimary(t *testing.T, text string, after func(c *counted, w http.ResponseWriter, r *http.Request)) *counted {
	t.Helper()
	c := &counted{}
	return c.serve(t, func(w http.ResponseWriter, r *http.Request) {
		c.hit()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":` + fmt.Sprint(abandonedInputTokens) + `,"output_tokens":0}}}

`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		}
		if text != "" {
			frames = append(frames, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+
				quote(text)+`}}

`)
		}
		for _, frame := range frames {
			fmt.Fprint(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if after != nil {
			after(c, w, r)
		}
	})
}

// cut hijacks the connection and closes it, so the client sees a truncated response
// rather than a clean end of stream. It is the transport failure a failover acts on.
func cut(_ *counted, w http.ResponseWriter, _ *http.Request) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			conn.Close()
		}
	}
}

// held keeps the response open, so the thing that ends the attempt is the caller.
func held(c *counted, _ http.ResponseWriter, r *http.Request) { c.hold(r) }

// anthropicRefusing serves an error status instead of a stream.
func anthropicRefusing(t *testing.T, status int) *counted {
	t.Helper()
	c := &counted{}
	return c.serve(t, func(w http.ResponseWriter, _ *http.Request) {
		c.hit()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"nope"}}`)
	})
}

// anthropicStalling accepts the request and never answers it. What ends the attempt is
// the client's own request timeout, which is the distinction this exists to test.
func anthropicStalling(t *testing.T) *counted {
	t.Helper()
	c := &counted{}
	return c.serve(t, func(_ http.ResponseWriter, r *http.Request) {
		c.hit()
		c.hold(r)
	})
}

// openAISecondary serves a complete OpenAI-protocol answer, usage and all.
func openAISecondary(t *testing.T, text string) *counted {
	t.Helper()
	c := &counted{}
	return c.serve(t, func(w http.ResponseWriter, _ *http.Request) {
		c.hit()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,` +
				`"model":"gpt-5-2025-08-07","choices":[{"index":0,"delta":{"content":` +
				quote(text) + `},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,` +
				`"model":"gpt-5-2025-08-07","choices":[{"index":0,"delta":{},` +
				`"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
				fmt.Sprint(secondaryInputTokens) + `,"completion_tokens":` +
				fmt.Sprint(secondaryOutputTokens) + `,"total_tokens":` +
				fmt.Sprint(secondaryInputTokens+secondaryOutputTokens) + `}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			fmt.Fprint(w, chunk+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
}

func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func primaryClient(c *counted, timeout time.Duration) llm.Client {
	return llm.NewAnthropic(llm.Options{
		APIKey: "primary-key", Model: "claude-opus-5", BaseURL: c.URL(),
		// One attempt, so the SDK's own retries do not stand in for the failover and
		// make every test below pass for the wrong reason.
		MaxTokens: 1024, MaxAttempts: 1, RequestTimeout: timeout,
	})
}

func secondaryClient(c *counted) llm.Client {
	return llm.NewOpenAI(llm.Options{
		APIKey: "secondary-key", Model: "gpt-5", BaseURL: c.URL(),
		MaxTokens: 1024, MaxAttempts: 1, RequestTimeout: 5 * time.Second,
	})
}

// pair builds a metered failover over the two servers and returns the registry to read
// the counters back out of. The metrics are the real obs.Metrics through a real
// prometheus registry, not a recording double: what a scrape would show is the thing
// being asserted.
func pair(t *testing.T, primary, secondary *counted, timeout time.Duration) (llm.Client, *obs.Metrics) {
	t.Helper()
	metrics := obs.NewMetrics()
	client := llm.MeterFailover(
		llm.NewFailover(primaryClient(primary, timeout), secondaryClient(secondary)),
		metrics)
	return client, metrics
}

func turn() llm.Request {
	return llm.Request{
		System:    "you are a test",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "how long do I have?"}},
		MaxTokens: 1024,
	}
}

// counter reads one labelled sample out of a real Gather. Absent reads as zero, which is
// what "this never happened" means for a counter that is only created on first use.
//
// Every requested label has to be present *and* equal. The first version only compared
// labels the sample already carried, so a sample missing the label entirely matched
// anything -- and a perturbation that renamed `reason` to `conversation_id` left every
// assertion in this file green while the counter grew an unbounded dimension. Only the
// label-name test saw it. An absent label matching is the same silently-blind shape as
// the detectors CLAUDE.md lists, arriving inside the test helper rather than the code.
func counter(t *testing.T, m *obs.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.GetMetric() {
			present := map[string]string{}
			for _, label := range sample.GetLabel() {
				present[label.GetName()] = label.GetValue()
			}
			matched := true
			for name, want := range labels {
				if got, ok := present[name]; !ok || got != want {
					matched = false
				}
			}
			if matched {
				return sample.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// labelNames returns every label name any sample of one family carries, so a test can
// assert what a metric is *not* tagged by.
func labelNames(t *testing.T, m *obs.Metrics, name string) map[string]bool {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.GetMetric() {
			for _, label := range sample.GetLabel() {
				out[label.GetName()] = true
			}
		}
	}
	return out
}

// --- the failures that are worth a second provider ----------------------------------

// The case the whole thing exists for: the primary is up enough to answer and what it
// answers is 529.
func TestAnOverloadedPrimaryIsAnsweredBySecondary(t *testing.T) {
	primary := anthropicRefusing(t, 529) // Anthropic's "overloaded"
	secondary := openAISecondary(t, "Thirty days.")
	client, metrics := pair(t, primary, secondary, 5*time.Second)

	var streamed strings.Builder
	result, err := client.Stream(context.Background(), turn(), func(text string) error {
		streamed.WriteString(text)
		return nil
	})

	if err != nil {
		t.Fatalf("the turn failed even though the secondary was up: %v", err)
	}
	if streamed.String() != "Thirty days." {
		t.Errorf("the customer read %q, want the secondary's answer", streamed.String())
	}
	if secondary.Count() != 1 {
		t.Errorf("the secondary was asked %d times, want exactly 1", secondary.Count())
	}
	// Prices and meters key on the model the provider reported, so a failover that did
	// not carry the new provider's model id through would bill Claude's rate for GPT-5's
	// tokens.
	if result.Model != "gpt-5-2025-08-07" {
		t.Errorf("result model is %q, want the secondary's reported model", result.Model)
	}
	if got := counter(t, metrics, "chat_provider_failovers_total", map[string]string{
		"from": "anthropic", "to": "openai", "reason": "unavailable",
	}); got != 1 {
		t.Errorf("chat_provider_failovers_total{from=anthropic,to=openai,reason=unavailable} "+
			"is %v, want 1: a failover nothing counts is a provider outage nobody sees", got)
	}
}

// A provider that accepts the request and then says nothing. The distinction being made
// is between *this attempt's* deadline and the caller's: the first is a stalled provider
// and the second is a customer who is no longer waiting, and both arrive here as
// context.DeadlineExceeded.
func TestAPrimaryThatStallsIsGivenUpOnRatherThanWaitedOut(t *testing.T) {
	primary := anthropicStalling(t)
	secondary := openAISecondary(t, "Thirty days.")
	// 150ms is this attempt's own budget. The caller below has none at all.
	client, metrics := pair(t, primary, secondary, 150*time.Millisecond)

	var streamed strings.Builder
	_, err := client.Stream(context.Background(), turn(), func(text string) error {
		streamed.WriteString(text)
		return nil
	})

	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if streamed.String() != "Thirty days." {
		t.Errorf("the customer read %q, want the secondary's answer", streamed.String())
	}
	if got := counter(t, metrics, "chat_provider_failovers_total", map[string]string{
		"reason": "stalled",
	}); got != 1 {
		t.Errorf("failovers with reason=stalled is %v, want 1", got)
	}
}

// --- the failed attempt's money -----------------------------------------------------

// Anthropic reports the input count at message_start, before a single token of the
// answer. A primary that dies in that window -- which is exactly the window a failover
// acts in, since a token on screen forbids one -- has already been billed for the whole
// prompt: history, retrieved passages and all.
//
// llm.Client.Stream returns one call's usage and the caller sums, so the only way those
// tokens reach the budget at all is inside the Result this returns. Losing them here
// would put the same defect one layer below the comment in llm.go that promises
// otherwise, which is where the Java implementation's version of it lived.
func TestTheAbandonedAttemptsTokensSurviveTheFailover(t *testing.T) {
	// Cut after message_start and before any text: billed, and nothing on screen.
	primary := anthropicPrimary(t, "", cut)
	secondary := openAISecondary(t, "Thirty days.")
	client, metrics := pair(t, primary, secondary, 5*time.Second)

	result, err := client.Stream(context.Background(), turn(), func(string) error { return nil })
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}

	wantInput := int64(abandonedInputTokens + secondaryInputTokens)
	if result.Usage.InputTokens != wantInput {
		t.Errorf("the turn reports %d input tokens, want %d (%d already billed by the "+
			"abandoned primary plus %d by the secondary). The caller sums Results; "+
			"tokens dropped here never reach the budget or the meters.",
			result.Usage.InputTokens, wantInput, abandonedInputTokens, secondaryInputTokens)
	}
	if result.Usage.OutputTokens != secondaryOutputTokens {
		t.Errorf("output tokens are %d, want %d", result.Usage.OutputTokens, secondaryOutputTokens)
	}

	// And separately, at the model that actually spent them. One Result carries one
	// model id, so those 1842 tokens reach chat_tokens_total labelled gpt-5 and priced
	// at OpenAI's rate; this counter is what makes that skew a number rather than a
	// silent one.
	if got := counter(t, metrics, "chat_failover_abandoned_tokens_total", map[string]string{
		"provider": "anthropic", "model": "claude-opus-5", "type": "input",
	}); got != abandonedInputTokens {
		t.Errorf("chat_failover_abandoned_tokens_total for the primary is %v, want %d",
			got, abandonedInputTokens)
	}
}

// --- the failures that are not worth a second provider ------------------------------

// The most common way a stream ends badly, and the one that looks most like an outage
// from inside a client: the customer is no longer waiting. A second provider would be
// billed in full for an answer nobody will read.
//
// Two shapes, and the second is the one that carries the guard. Both are the caller's
// context expiring, and they arrive here as different errors.
func TestACustomerWhoIsNoLongerWaitingIsNotAnsweredTwice(t *testing.T) {
	// A closed tab: the request is torn down and the client reports context.Canceled.
	//
	// Worth being precise about what this proves. Deleting the ctx.Err() guard does not
	// make this subtest fail, because the bottom of decide refuses an unrecognised error
	// anyway and context.Canceled never becomes an *llm.Error. It is here as a
	// behavioural assertion rather than as cover for that guard; the "deadline" subtest
	// below is what the guard is actually load-bearing for.
	t.Run("cancelled", func(t *testing.T) {
		primary := anthropicPrimary(t, "", held)
		secondary := openAISecondary(t, "Thirty days.")
		client, metrics := pair(t, primary, secondary, 5*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		result, err := client.Stream(ctx, turn(), func(string) error { return nil })

		if err == nil {
			t.Fatal("a cancelled turn returned no error")
		}
		if secondary.Count() != 0 {
			t.Errorf("the secondary was asked %d times; a customer who gave up must not "+
				"be billed at two providers", secondary.Count())
		}
		if got := counter(t, metrics, "chat_provider_failovers_total", nil); got != 0 {
			t.Errorf("a failover was counted (%v) for a cancellation", got)
		}
		// The primary's own tokens are still spent, and still reported. That is the
		// contract stream_test.go pins, and the failover must not break it on the path
		// where it does nothing at all.
		if result.Usage.InputTokens != abandonedInputTokens {
			t.Errorf("usage reports %d input tokens, want %d -- already billed before "+
				"the cancellation", result.Usage.InputTokens, abandonedInputTokens)
		}
	})

	// The turn's own deadline, and the reason decide asks about the caller's context
	// *before* it asks what the error was.
	//
	// This is indistinguishable from a stalled provider by the error alone: both are
	// context.DeadlineExceeded, and one of them is worth a second provider. What
	// separates them is whose clock ran out. Ask the error first and every turn that
	// times out spends a second provider's tokens on a customer whose deadline has
	// already passed -- at exactly the moment the service is slowest and least able to
	// afford it.
	t.Run("deadline", func(t *testing.T) {
		primary := anthropicPrimary(t, "", held)
		secondary := openAISecondary(t, "Thirty days.")
		// The attempt's own budget is generous; the caller's is not. The stalled test
		// above is this one with the two swapped round.
		client, metrics := pair(t, primary, secondary, 30*time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := client.Stream(ctx, turn(), func(string) error { return nil })

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error is %v, want the caller's deadline", err)
		}
		if secondary.Count() != 0 {
			t.Errorf("the secondary was asked %d times after the turn's own deadline "+
				"expired; the same error from a stalled provider is worth a failover and "+
				"this one is not", secondary.Count())
		}
		if got := counter(t, metrics, "chat_provider_failovers_total", map[string]string{
			"reason": "stalled",
		}); got != 0 {
			t.Errorf("%v failovers counted as a stalled provider; it was the caller's "+
				"own deadline", got)
		}
	})
}

// The consumer refusing the text is the same event one layer up: the SSE writer failed
// because the client is gone. It arrives as an error that never passed through classify,
// and the default answer to an unrecognised error is not to spend money on it.
func TestAConsumerThatRefusesTheTextDoesNotTriggerAFailover(t *testing.T) {
	primary := anthropicPrimary(t, "Thirty ", held)
	secondary := openAISecondary(t, "Thirty days.")
	client, _ := pair(t, primary, secondary, 5*time.Second)

	consumerGaveUp := errors.New("the client disconnected")
	_, err := client.Stream(context.Background(), turn(), func(string) error {
		return consumerGaveUp
	})

	if !errors.Is(err, consumerGaveUp) {
		t.Fatalf("error is %v, want the consumer's own error", err)
	}
	if secondary.Count() != 0 {
		t.Errorf("the secondary was asked %d times, want 0", secondary.Count())
	}
}

// A 401 is this deployment's credentials, not the provider's health. Failing over would
// answer every customer correctly while the primary's key was broken, and the first
// thing anyone would learn from is an invoice from the wrong provider.
func TestARejectedRequestIsNotSentToTheSecondProvider(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound} {

		t.Run(fmt.Sprint(status), func(t *testing.T) {
			primary := anthropicRefusing(t, status)
			secondary := openAISecondary(t, "Thirty days.")
			client, _ := pair(t, primary, secondary, 5*time.Second)

			_, err := client.Stream(context.Background(), turn(), func(string) error { return nil })

			if err == nil {
				t.Fatalf("a %d returned no error", status)
			}
			if secondary.Count() != 0 {
				t.Errorf("the secondary was asked %d times for a %d; it would reject the "+
					"same request one invoice later", secondary.Count(), status)
			}
		})
	}
}

// The voice decision, enforced at the only place it can be: a token that has already
// been handed to the consumer.
//
// The two providers do not write the same way, and chat.Service inserts a paragraph
// break between model calls -- so a mid-answer switch would not even look like a fault,
// it would look like a deliberate second paragraph in a different voice. One visibly
// failed turn is the better outcome, and this is where that costs something: a working
// secondary is sitting right there.
func TestATurnTheCustomerIsAlreadyReadingIsNeverHandedToAnotherProvider(t *testing.T) {
	primary := anthropicPrimary(t, "Thirty ", cut)
	secondary := openAISecondary(t, "Thirty days.")
	client, _ := pair(t, primary, secondary, 5*time.Second)

	var streamed strings.Builder
	_, err := client.Stream(context.Background(), turn(), func(text string) error {
		streamed.WriteString(text)
		return nil
	})

	if err == nil {
		t.Fatal("a truncated stream returned no error")
	}
	if secondary.Count() != 0 {
		t.Errorf("the secondary was asked %d times after the customer had already read "+
			"%q from the primary", secondary.Count(), streamed.String())
	}
	if streamed.String() != "Thirty " {
		t.Errorf("the customer read %q; nothing may be appended to a half-written answer",
			streamed.String())
	}
}

// A tool round is not the start of a turn. Its request carries the assistant turn the
// primary produced -- that provider's tool-call ids, and on Anthropic the thinking blocks
// a continuation has to echo back verbatim in Message.Native. Another provider's client
// cannot read any of it, so the request it would send is a different request wearing the
// same name.
func TestAToolRoundIsNotHandedToAnotherProvider(t *testing.T) {
	primary := anthropicRefusing(t, http.StatusServiceUnavailable)
	secondary := openAISecondary(t, "Thirty days.")
	client, _ := pair(t, primary, secondary, 5*time.Second)

	midTurn := turn()
	midTurn.Messages = append(midTurn.Messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "toolu_01", Name: "lookup_order_status",
			Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`),
		}}},
		llm.Message{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{
			CallID: "toolu_01", Content: "shipped",
		}}})

	_, err := client.Stream(context.Background(), midTurn, func(string) error { return nil })

	if err == nil {
		t.Fatal("a 503 mid-turn returned no error")
	}
	if secondary.Count() != 0 {
		t.Errorf("the secondary was asked %d times to continue a turn it did not start",
			secondary.Count())
	}
}

// --- what the counter may be tagged by ----------------------------------------------

// The cardinality rule, asserted against a real Gather rather than read off the source.
// Every other metric here is tagged by model and never by conversation id; a failover
// counter is exactly the kind of thing somebody would want "just the affected
// conversations" from.
func TestTheFailoverCounterIsNotTaggedByAnythingUnbounded(t *testing.T) {
	primary := anthropicRefusing(t, 529)
	secondary := openAISecondary(t, "Thirty days.")
	client, metrics := pair(t, primary, secondary, 5*time.Second)

	if _, err := client.Stream(context.Background(), turn(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}

	got := labelNames(t, metrics, "chat_provider_failovers_total")
	want := map[string]bool{"from": true, "to": true, "reason": true}
	for name := range got {
		if !want[name] {
			t.Errorf("chat_provider_failovers_total carries a %q label; the labels are "+
				"from, to and reason, and every one of them is bounded by configuration",
				name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("chat_provider_failovers_total has no %q label", name)
		}
	}
}

// --- no fallback configured ---------------------------------------------------------

// The default. A single client, not a Failover wrapping a nil, and nothing in the path
// that could decide to call something.
func TestWithoutASecondProviderTheClientIsTheProviderItself(t *testing.T) {
	primary := anthropicRefusing(t, 529)
	client := primaryClient(primary, 5*time.Second)

	if _, ok := client.(*llm.Failover); ok {
		t.Fatal("a client built with no fallback is a Failover")
	}
	// MeterFailover is called unconditionally by main, so it has to be a no-op here
	// rather than a panic on a client that is not a Failover.
	if metered := llm.MeterFailover(client, obs.NewMetrics()); metered != client {
		t.Error("MeterFailover replaced a client that has no fallback")
	}
}

// A guard for the two counters this file reads: if they were ever renamed or dropped,
// every `counter(...) != 1` assertion above would keep passing by finding nothing, which
// is the exact shape of a silently blind detector.
func TestTheFailoverCountersExistBeforeAnyFailoverHasHappened(t *testing.T) {
	metrics := obs.NewMetrics()
	descs := make(chan *prometheus.Desc, 64)
	go func() { metrics.Registry.Describe(descs); close(descs) }()

	described := map[string]bool{}
	for d := range descs {
		described[d.String()] = true
	}
	for _, name := range []string{"chat_provider_failovers_total", "chat_failover_abandoned_tokens_total"} {
		found := false
		for d := range described {
			if strings.Contains(d, `fqName: "`+name+`"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not registered; every assertion in this file that reads it "+
				"would pass by reading zero", name)
		}
	}
}
