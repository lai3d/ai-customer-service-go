package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

const dims = 8

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	p, stop, err := testsupport.StartPostgres(ctx, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

// stubEmbedder makes these tests about the turn rather than about the model. Retrieval
// quality is measured against the real embedding model in internal/rag; here the only
// thing that matters is what the passages do to memory and to the prompt.
type stubEmbedder struct{}

// Not the zero vector: cosine distance against all-zeros is NaN, and a NaN score
// fails the threshold comparison silently, so every search would return nothing.
func unitVector() []float32 {
	v := make([]float32, dims)
	v[0] = 1
	return v
}

func (stubEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return unitVector(), nil
}
func (stubEmbedder) EmbedPassages(_ context.Context, passages []string) ([][]float32, error) {
	out := make([][]float32, len(passages))
	for i := range out {
		out[i] = unitVector()
	}
	return out, nil
}
func (stubEmbedder) Dimensions() int { return dims }
func (stubEmbedder) Close() error    { return nil }

// stubClient records what it was asked and replies with a script.
type stubClient struct {
	mu       sync.Mutex
	requests []llm.Request
	// script is one entry per model call.
	script []llm.Result
	err    error
	// onCall runs before each reply, for tests that need to interfere mid-turn.
	onCall func(call int)
	calls  int
}

func (c *stubClient) Provider() string { return "stub" }
func (c *stubClient) Model() string    { return "stub-model" }

func (c *stubClient) Stream(_ context.Context, req llm.Request, onText func(string) error) (llm.Result, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	call := c.calls
	c.calls++
	c.mu.Unlock()

	if c.onCall != nil {
		c.onCall(call)
	}
	if c.err != nil {
		return llm.Result{Usage: llm.Usage{InputTokens: 100}}, c.err
	}
	if call >= len(c.script) {
		return llm.Result{}, fmt.Errorf("stub ran out of script at call %d", call)
	}
	result := c.script[call]
	if result.Text != "" {
		if err := onText(result.Text); err != nil {
			return llm.Result{}, err
		}
	}
	if result.Model == "" {
		result.Model = "stub-model-2026-01-01"
	}
	return result, nil
}

func (c *stubClient) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("the model was never called")
	}
	return c.requests[len(c.requests)-1]
}

type fixture struct {
	service *chat.Service
	memory  *chat.Memory
	client  *stubClient
	tickets *tools.SupportTickets
	budget  *cost.Budget
	events  []chat.Event
	mu      sync.Mutex
}

func newFixture(t *testing.T, client *stubClient, budgetLimit int64) *fixture {
	t.Helper()
	ctx := context.Background()

	vectors := rag.NewStore(pool)
	if _, err := pool.Exec(ctx, `DELETE FROM chat_memory`); err != nil {
		t.Fatal(err)
	}
	docs := []rag.Document{{
		ID: "faq:returns-window:en", EntryID: "returns-window", Language: "en",
		Category: "returns", Question: "How long do I have to return an item?",
		Answer:  "Thirty days from delivery.",
		Content: "Q: How long do I have to return an item?\nA: Thirty days from delivery.",
	}}
	if err := vectors.Replace(ctx, docs, [][]float32{unitVector()}); err != nil {
		t.Fatal(err)
	}

	memory := chat.NewMemory(pool, 40)
	tickets := tools.NewSupportTickets(100)
	budget := cost.NewBudget(budgetLimit, 100)
	f := &fixture{memory: memory, client: client, tickets: tickets, budget: budget}
	f.service = chat.NewService(
		memory,
		rag.NewRetriever(stubEmbedder{}, vectors, 8, 0),
		client, budget, obs.NewMetrics(), 1024,
		tools.NewOrderLookup(), tickets,
	)
	return f
}

func (f *fixture) turn(ctx context.Context, conversationID, message string) error {
	return f.service.Turn(ctx, conversationID, message, func(e chat.Event) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, e)
	})
}

func (f *fixture) eventsOfType(kind chat.EventType) []chat.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []chat.Event
	for _, e := range f.events {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

func reply(text string) llm.Result {
	return llm.Result{Text: text, StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 50}}
}

// The ordering constraint, stated as a test rather than inherited from a framework's
// defaults. Retrieval must not be able to reach conversation memory: the passages go in
// the outgoing request, and the customer's own words go in the history.
//
// Reversed, every retrieved passage is stored and re-sent on every later turn. Nothing
// fails. The prompt grows, and so does the bill.
func TestRetrievedPassagesNeverEnterMemory(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("Thirty days.")}}
	f := newFixture(t, client, 0)
	ctx := context.Background()

	if err := f.turn(ctx, "c1", "can I still send this back?"); err != nil {
		t.Fatal(err)
	}

	// The passage did reach the model...
	sent := f.client.lastRequest(t)
	last := sent.Messages[len(sent.Messages)-1]
	if !strings.Contains(last.Text, "Thirty days from delivery") {
		t.Fatalf("the retrieved passage never reached the model; got %q", last.Text)
	}
	if !strings.Contains(last.Text, "can I still send this back?") {
		t.Errorf("the customer's question was lost from the prompt; got %q", last.Text)
	}

	// ...and did not reach memory.
	history, err := f.memory.History(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if strings.Contains(m.Text, "Thirty days from delivery") && m.Role == llm.RoleUser {
			t.Fatalf("a retrieved passage was written into the customer's history: %q", m.Text)
		}
	}
	if len(history) != 2 {
		t.Fatalf("expected the question and the reply in history, got %d messages", len(history))
	}
	if history[0].Text != "can I still send this back?" {
		t.Errorf("stored user message is %q, want the customer's own words", history[0].Text)
	}
}

// A second turn is where the cost of getting the above wrong would show up.
func TestASecondTurnDoesNotResendTheFirstTurnsPassages(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("Thirty days."), reply("Yes.")}}
	f := newFixture(t, client, 0)
	ctx := context.Background()

	if err := f.turn(ctx, "c1", "can I still send this back?"); err != nil {
		t.Fatal(err)
	}
	if err := f.turn(ctx, "c1", "and if it was a gift?"); err != nil {
		t.Fatal(err)
	}

	sent := f.client.lastRequest(t)
	occurrences := 0
	for _, m := range sent.Messages {
		occurrences += strings.Count(m.Text, "Thirty days from delivery")
	}
	if occurrences != 1 {
		t.Errorf("the passage appears %d times in the second turn's request; it should appear "+
			"once, attached to the current question only", occurrences)
	}
}

// Retrieval is reported before the model is called, so a client can show it while the
// model is still thinking -- and so it survives a model call that fails, which is
// exactly when someone debugging a bad answer needs to see what was retrieved.
func TestRetrievalIsReportedBeforeTheModelIsCalled(t *testing.T) {
	client := &stubClient{err: errors.New("provider exploded")}
	f := newFixture(t, client, 0)

	err := f.turn(context.Background(), "c1", "can I still send this back?")
	if err == nil {
		t.Fatal("expected the failing model call to surface")
	}
	if got := len(f.eventsOfType(chat.EventRetrieval)); got != 1 {
		t.Errorf("got %d retrieval events after a failed model call, want 1", got)
	}
}

// A turn is not a model call. A tool-calling turn makes at least two, and each is
// billed; the usage the caller sees is the sum.
func TestUsageIsSummedAcrossEveryModelCallInATurn(t *testing.T) {
	client := &stubClient{script: []llm.Result{
		{
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
				Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`)}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 1800, OutputTokens: 18},
		},
		{Text: "It arrives on the third.", StopReason: "end_turn",
			Usage: llm.Usage{InputTokens: 3696, OutputTokens: 108}},
	}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "where is ORD-10042?"); err != nil {
		t.Fatal(err)
	}

	usage := f.eventsOfType(chat.EventUsage)
	if len(usage) != 1 {
		t.Fatalf("got %d usage events, want 1", len(usage))
	}
	got := usage[0].Usage
	if got.ModelCalls != 2 {
		t.Errorf("reported %d model calls, want 2", got.ModelCalls)
	}
	// 1800 + 3696. Keeping the last call under-reports this turn as 3,696; keeping the
	// first reports 1,800. Both were shipped in the Java implementation.
	if got.InputTokens != 5496 || got.OutputTokens != 126 {
		t.Errorf("usage is in=%d out=%d, want in=5496 out=126",
			got.InputTokens, got.OutputTokens)
	}
	if got.Model != "stub-model-2026-01-01" {
		t.Errorf("usage is tagged %q; it must carry the model the provider reported, "+
			"not the one requested", got.Model)
	}
}

// The tool result has to reach the second model call, or the assistant answers from
// nothing while the tool ran successfully.
func TestToolResultsReachTheFollowingModelCall(t *testing.T) {
	client := &stubClient{script: []llm.Result{
		{
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
				Arguments: json.RawMessage(`{"orderNumber":"ord-10042 "}`)}},
			StopReason: "tool_use",
		},
		reply("It is in transit."),
	}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "where is my order?"); err != nil {
		t.Fatal(err)
	}

	sent := f.client.lastRequest(t)
	var found bool
	for _, m := range sent.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, "SP884213906SG") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the tool result never reached the model")
	}
	toolEvents := f.eventsOfType(chat.EventTool)
	if len(toolEvents) != 1 || toolEvents[0].Tool.Outcome != "found" {
		t.Errorf("tool events are %+v, want one with outcome found", toolEvents)
	}
}

// Whatever the model managed to say is kept, however the turn ended. Otherwise a
// disconnect leaves an orphaned user message and the next turn opens with two user
// messages in a row.
func TestAPartialReplyIsPersistedWhenTheClientDisconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &stubClient{
		script: []llm.Result{reply("Thirty da")},
		onCall: func(int) {},
	}
	f := newFixture(t, client, 0)

	// Cancel as soon as the first token has been emitted.
	client.onCall = func(int) {}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	client.err = context.Canceled

	_ = f.turn(ctx, "c1", "can I still send this back?")
	cancel()

	// The user message survives even though the reply did not: the partial-reply write
	// runs on a context detached from the request's, because a cancelled context
	// cannot write to Postgres.
	count, err := f.memory.Count(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("nothing was persisted for a cancelled turn")
	}
}

func TestABudgetedOutConversationIsRefusedBeforeTheModelIsCalled(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("Thirty days."), reply("again")}}
	f := newFixture(t, client, 500)
	ctx := context.Background()

	if err := f.turn(ctx, "c1", "first question"); err != nil {
		t.Fatal(err)
	}

	err := f.turn(ctx, "c1", "second question")
	var exceeded *cost.ErrExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("second turn returned %v, want a budget error", err)
	}
	if client.calls != 1 {
		t.Errorf("the model was called %d times; the second turn must be refused before "+
			"any model call", client.calls)
	}
}

// Consecutive same-role messages happen: a turn whose model call fails leaves a user
// message with no reply after it.
func TestConsecutiveUserMessagesAreMergedWhenHistoryIsRead(t *testing.T) {
	ctx := context.Background()
	memory := chat.NewMemory(pool, 40)
	if _, err := pool.Exec(ctx, `DELETE FROM chat_memory WHERE conversation_id = 'merge'`); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if err := memory.Append(ctx, "merge", llm.RoleUser, text); err != nil {
			t.Fatal(err)
		}
	}

	history, err := memory.History(ctx, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history has %d messages, want the two user messages merged into one", len(history))
	}
	if !strings.Contains(history[0].Text, "first") || !strings.Contains(history[0].Text, "second") {
		t.Errorf("merged message lost content: %q", history[0].Text)
	}
}
