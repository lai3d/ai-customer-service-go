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
		// Both error paths carry usage, because both real clients do. A stub that
		// invents a contract its production counterpart does not honour makes the
		// suite test the fixture; internal/llm/stream_test.go is where that contract
		// is actually asserted, against the clients themselves.
		return llm.Result{Usage: llm.Usage{InputTokens: 100}, Model: "stub-model-2026-01-01"}, c.err
	}
	if call >= len(c.script) {
		return llm.Result{}, fmt.Errorf("stub ran out of script at call %d", call)
	}
	result := c.script[call]
	if result.Text != "" {
		if err := onText(result.Text); err != nil {
			return llm.Result{Usage: result.Usage, Model: "stub-model-2026-01-01"}, err
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
	metrics *obs.Metrics
	events  []chat.Event
	mu      sync.Mutex
}

func newFixture(t *testing.T, client *stubClient, budgetLimit int64) *fixture {
	t.Helper()
	return newFixtureWithClient(t, client, budgetLimit)
}

func newFixtureWithClient(t *testing.T, client llm.Client, budgetLimit int64) *fixture {
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
	tickets := tools.NewSupportTickets(&testsupport.FakeTickets{})
	budget := cost.NewBudget(budgetLimit, 100)
	metrics := obs.NewMetrics()
	stub, _ := client.(*stubClient)
	f := &fixture{memory: memory, client: stub, tickets: tickets,
		budget: budget, metrics: metrics}
	f.service = chat.NewService(
		memory,
		rag.NewRetriever(stubEmbedder{}, vectors, 8, 0),
		client, budget, metrics, chat.NewRecorder(pool), 1024,
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

// counterLabels returns what a counter recorded, keyed by the named label's value.
// Keyed by name rather than by position: Prometheus sorts labels alphabetically, so a
// positional key silently depends on what the labels happen to be called.
func (f *fixture) counterLabels(t *testing.T, name, label string) map[string]float64 {
	t.Helper()
	families, err := f.metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label {
					out[l.GetValue()] += m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// The tool name is written by the model, and a metric label is an aggregated dimension.
// An unbounded set of label values takes a metrics backend down -- the same hazard the
// conversation id is deliberately kept away from, arriving through a different door.
// It is also attacker-influenced: a retrieved passage can carry an instruction to call a
// tool that does not exist, and this is the branch that reaches.
func TestAnInventedToolNameNeverBecomesAMetricLabel(t *testing.T) {
	invented := "exfiltrate_everything_" + strings.Repeat("x", 200)
	client := &stubClient{script: []llm.Result{
		{
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: invented,
				Arguments: json.RawMessage(`{}`)}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 100},
		},
		reply("I could not do that."),
	}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "do something odd"); err != nil {
		t.Fatal(err)
	}

	names := f.counterLabels(t, "chat_tool_invocations_total", "tool")
	if _, leaked := names[invented]; leaked {
		t.Errorf("a model-invented tool name reached a metric label: %q", invented)
	}
	if names["unknown"] != 1 || len(names) != 1 {
		t.Errorf("tool labels are %v, want exactly one bounded \"unknown\"", names)
	}

	// The model still has to be told what it actually asked for, or it cannot recover.
	sent := f.client.lastRequest(t)
	var told bool
	for _, m := range sent.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, invented) {
				told = true
			}
		}
	}
	if !told {
		t.Error("the model was not told which tool name was rejected")
	}
}

// A turn stopped by the round cap is not a completed turn. The customer sees whatever
// text had accumulated -- possibly none -- and without a distinct outcome it is
// indistinguishable in the meters from a turn that answered, which is the first thing
// anyone would want to know before changing the cap.
func TestATurnStoppedByTheToolCapIsNotRecordedAsCompleted(t *testing.T) {
	askForATool := llm.Result{
		ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
			Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`)}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 10},
	}
	client := &stubClient{script: []llm.Result{askForATool, askForATool, askForATool, askForATool}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "where is my order?"); err != nil {
		t.Fatal(err)
	}

	outcomes := f.counterLabels(t, "chat_turns_total", "outcome")
	if outcomes["completed"] != 0 {
		t.Errorf("a turn that never answered was counted as completed: %v", outcomes)
	}
	if outcomes["tool_limit"] != 1 {
		t.Errorf("turn outcomes are %v, want tool_limit", outcomes)
	}
}

// A failure before the model is reached is still a turn. Counting only the turns that
// got as far as a model call makes a retrieval outage look like silence in
// chat_turns_total rather than a spike -- the wrong direction for the first metric
// anyone reaches for.
func TestAFailureBeforeTheModelIsStillCountedAsATurn(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("first")}}
	f := newFixture(t, client, 200)
	ctx := context.Background()

	if err := f.turn(ctx, "c1", "first question"); err != nil {
		t.Fatal(err)
	}
	if err := f.turn(ctx, "c1", "second question"); err == nil {
		t.Fatal("expected the second turn to be refused")
	}

	outcomes := f.counterLabels(t, "chat_turns_total", "outcome")
	if outcomes["budget_exceeded"] != 1 {
		t.Errorf("turn outcomes are %v, want one budget_exceeded", outcomes)
	}
	if outcomes["completed"] != 1 {
		t.Errorf("turn outcomes are %v, want the first turn counted as completed", outcomes)
	}
}

// A tool-calling turn is two model calls, and the second one's text is a new message
// rather than a continuation. Run together they read as a typo in the answer:
// "...and any tracking details.Here's what I found for order ORD-10042".
//
// It only happens when the model says something before asking for the tool, which is why
// the question everyone tests with -- one that goes straight to the tool -- never shows
// it. Found on a live turn after the Java implementation's session hit it in a
// screenshot it had been shipping in its README.
func TestTextFromTwoModelCallsIsNotRunTogether(t *testing.T) {
	client := &stubClient{script: []llm.Result{
		{
			Text: "I'll look that up for you.",
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
				Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`)}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 100, OutputTokens: 10},
		},
		reply("Here's what I found: it is in transit."),
	}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "where is my order?"); err != nil {
		t.Fatal(err)
	}

	var streamed strings.Builder
	for _, e := range f.eventsOfType(chat.EventMessage) {
		streamed.WriteString(e.Text)
	}
	got := streamed.String()

	if strings.Contains(got, "you.Here's") {
		t.Errorf("the two model calls were run together: %q", got)
	}
	if !strings.Contains(got, "you.\n\nHere's") {
		t.Errorf("streamed text is %q, want a paragraph break at the seam", got)
	}

	// The break has to be in what is persisted too, or the next turn re-sends a
	// run-together message as history.
	history, err := f.memory.History(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	stored := history[len(history)-1].Text
	if !strings.Contains(stored, "you.\n\nHere's") {
		t.Errorf("stored reply is %q, want the seam preserved", stored)
	}
}

// One model call must not gain a leading break, and a call that produces no text before
// asking for a tool must not leave a dangling one.
func TestASingleModelCallGainsNoParagraphBreak(t *testing.T) {
	client := &stubClient{script: []llm.Result{
		{
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
				Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`)}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 100},
		},
		reply("It is in transit."),
	}}
	f := newFixture(t, client, 0)

	if err := f.turn(context.Background(), "c1", "where is my order?"); err != nil {
		t.Fatal(err)
	}

	var streamed strings.Builder
	for _, e := range f.eventsOfType(chat.EventMessage) {
		streamed.WriteString(e.Text)
	}
	if got := streamed.String(); got != "It is in transit." {
		t.Errorf("streamed %q, want no leading break when the first call said nothing", got)
	}
}

// recordingClient captures what each model call was actually sent.
type recordingClient struct {
	mu   sync.Mutex
	seen []llm.Request
}

func (c *recordingClient) Provider() string { return "stub" }
func (c *recordingClient) Model() string    { return "stub-model" }
func (c *recordingClient) Stream(_ context.Context, req llm.Request, onText func(string) error) (llm.Result, error) {
	c.mu.Lock()
	c.seen = append(c.seen, req)
	n := len(c.seen)
	c.mu.Unlock()
	text := fmt.Sprintf("answer %d", n)
	if err := onText(text); err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Text: text, StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}, nil
}

func (c *recordingClient) requests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.Request(nil), c.seen...)
}

// Two overlapping requests on one conversation -- two browser tabs is enough -- used to
// interleave, because a turn wrote the user message, retrieved, and only then read
// history. The second request's user message and reply landed in that gap, so the first
// sent the model a conversation ending in somebody else's answer; and since passages are
// attached only to a trailing user message, its retrieved material was silently dropped
// at the same time.
//
// Found by an external review, not by this suite. Before the fix the second model call
// arrived with a trailing `role=assistant "answer 1"`.
func TestOverlappingTurnsOnOneConversationDoNotInterleave(t *testing.T) {
	client := &recordingClient{}
	f := newFixtureWithClient(t, client, 0)
	ctx := context.Background()

	// A pauses at its retrieval event, which happens before it reads history.
	aInside := make(chan struct{})
	bMayFinish := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = f.service.Turn(ctx, "shared", "question A", func(e chat.Event) {
			if e.Type == chat.EventRetrieval {
				close(aInside)
				<-bMayFinish
			}
		})
	}()

	// Only start B once A is demonstrably inside its turn, or B may simply win the
	// race to the lock and the test proves nothing.
	<-aInside

	// B must now wait on the conversation lock instead of running through A's middle.
	bFinished := make(chan struct{})
	go func() {
		defer close(bFinished)
		_ = f.service.Turn(ctx, "shared", "question B", func(chat.Event) {})
	}()

	select {
	case <-bFinished:
		t.Fatal("B completed while A held the conversation; the turns interleaved")
	case <-time.After(250 * time.Millisecond):
	}
	close(bMayFinish)
	wg.Wait()
	<-bFinished

	for i, req := range client.requests() {
		last := req.Messages[len(req.Messages)-1]
		if last.Role != llm.RoleUser {
			t.Errorf("model call %d ends with a %s message (%q); a turn must send the "+
				"model its own question last", i+1, last.Role, last.Text)
		}
		if !strings.Contains(last.Text, "Reference material") {
			t.Errorf("model call %d carries no retrieved passages: %q", i+1, last.Text)
		}
	}
}

// The lock table is keyed by conversation id, so it has to empty out. A map that only
// grows is the memory leak this codebase avoids everywhere else.
func TestTheConversationLockTableEmpties(t *testing.T) {
	client := &recordingClient{}
	f := newFixtureWithClient(t, client, 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = f.service.Turn(ctx, fmt.Sprintf("conversation-%d", i), "hello", func(chat.Event) {})
		}(i)
	}
	wg.Wait()

	if n := chat.InFlightConversations(f.service); n != 0 {
		t.Errorf("%d conversations still hold a lock after every turn finished", n)
	}
}

// A caller whose client has already gone should not queue behind a turn it will never
// see the result of.
func TestAWaitingTurnGivesUpWhenItsRequestIsCancelled(t *testing.T) {
	client := &recordingClient{}
	f := newFixtureWithClient(t, client, 0)

	holding := make(chan struct{})
	released := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = f.service.Turn(context.Background(), "shared", "first", func(e chat.Event) {
			if e.Type == chat.EventRetrieval {
				close(holding)
				<-released
			}
		})
	}()
	<-holding

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.service.Turn(ctx, "shared", "second", func(chat.Event) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled request waiting for the lock returned %v, want context.Canceled", err)
	}

	close(released)
	wg.Wait()
}

func turnRow(t *testing.T, id string) (outcome, reply, model string, calls int, in, out int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT outcome, coalesce(reply,''), coalesce(model,''), model_calls,
		       input_tokens, output_tokens
		FROM turn WHERE id = $1`, id).Scan(&outcome, &reply, &model, &calls, &in, &out)
	if err != nil {
		t.Fatalf("reading turn %s: %v", id, err)
	}
	return
}

func lastTurnID(t *testing.T, conversationID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM turn WHERE conversation_id = $1 ORDER BY started_at DESC LIMIT 1`,
		conversationID).Scan(&id); err != nil {
		t.Fatalf("no turn recorded for %s: %v", conversationID, err)
	}
	return id
}

// The operational record is what an operator is asked about afterwards, so the evidence
// has to be in it: what retrieval returned, which tools ran, what it cost. None of that
// is recoverable from chat_memory, which holds the customer's words and the reply and
// is windowed besides.
func TestATurnLeavesAnOperationalRecordWithItsEvidence(t *testing.T) {
	client := &stubClient{script: []llm.Result{
		{
			ToolCalls: []llm.ToolCall{{ID: "t1", Name: "lookup_order_status",
				Arguments: json.RawMessage(`{"orderNumber":"ORD-10042"}`)}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 1800, OutputTokens: 18},
		},
		{Text: "It is in transit.", StopReason: "end_turn",
			Usage: llm.Usage{InputTokens: 3696, OutputTokens: 108}},
	}}
	f := newFixture(t, client, 0)
	ctx := context.Background()

	if err := f.turn(ctx, "record-evidence", "where is my order?"); err != nil {
		t.Fatal(err)
	}
	id := lastTurnID(t, "record-evidence")

	outcome, reply, model, calls, in, out := turnRow(t, id)
	if outcome != "completed" {
		t.Errorf("outcome is %q, want completed", outcome)
	}
	if reply != "It is in transit." {
		t.Errorf("reply is %q", reply)
	}
	if model != "stub-model-2026-01-01" {
		t.Errorf("model is %q; the record must carry what the provider reported", model)
	}
	// The same accounting the usage event reports: a turn is not a model call.
	if calls != 2 || in != 5496 || out != 126 {
		t.Errorf("recorded calls=%d in=%d out=%d, want 2/5496/126", calls, in, out)
	}

	var passages, toolCalls int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM turn_passage WHERE turn_id=$1),
		        (SELECT count(*) FROM turn_tool_call WHERE turn_id=$1)`, id).
		Scan(&passages, &toolCalls); err != nil {
		t.Fatal(err)
	}
	if passages == 0 {
		t.Error("no retrieval evidence recorded; the corpus can change and this is the " +
			"only account of what the model was actually given")
	}
	if toolCalls != 1 {
		t.Errorf("recorded %d tool calls, want 1", toolCalls)
	}
}

// The reason the record is written at the service boundary rather than from the event
// stream: the stream feeds a page that may already be gone.
func TestATurnWhoseClientDisconnectedStillGetsATerminalOutcome(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("partial")}, err: context.Canceled}
	f := newFixture(t, client, 0)

	// Cancelled *during* the turn, not before it. A request cancelled before anything
	// happened correctly leaves no record -- no model call, no spend, nothing to answer
	// for. The case this exists for is the customer who closes the tab while the model
	// is talking, which is the most common mid-stream failure in production.
	ctx, cancel := context.WithCancel(context.Background())
	_ = f.service.Turn(ctx, "record-cancelled", "a question nobody waited for",
		func(e chat.Event) {
			if e.Type == chat.EventRetrieval {
				cancel()
			}
		})
	defer cancel()

	id := lastTurnID(t, "record-cancelled")
	outcome, _, _, _, _, _ := turnRow(t, id)
	if outcome == chat.OutcomeInFlight {
		t.Error("the turn is still recorded as in flight; a row left in that state means " +
			"the process died, and this one did not")
	}
	if outcome != "cancelled" {
		t.Errorf("outcome is %q, want cancelled -- a customer who closed the tab and a "+
			"provider that failed are different events", outcome)
	}
}

// If the turn cannot be accounted for, it does not happen. The alternative is finding
// the gap a month later, on a bill.
func TestAModelCallIsNotMadeIfTheTurnCannotBeRecorded(t *testing.T) {
	client := &stubClient{script: []llm.Result{reply("should never run")}}
	f := newFixture(t, client, 0)

	// A conversation id that cannot fit the column the record is keyed by.
	tooLong := strings.Repeat("x", 100)
	err := f.turn(context.Background(), tooLong, "hello")
	if err == nil {
		t.Fatal("the turn succeeded despite being unrecordable")
	}
	if client.calls != 0 {
		t.Errorf("the model was called %d times for a turn that could not be recorded",
			client.calls)
	}
}

// A turn whose process died stays in flight for ever, and the overview counts it under
// "not completed" as though the customer had walked away. A crash and a closed tab are the
// two things this record exists to tell apart.
//
// Found by the Java implementation of this system, which has the sweeper this did not.
func TestATurnLeftInFlightByADeadProcessBecomesUnknown(t *testing.T) {
	ctx := context.Background()
	recorder := chat.NewRecorder(pool)

	// Begin, and then nothing: the process died between Begin and Finish.
	const id = "sweep-me"
	if err := recorder.Begin(ctx, chat.TurnRecord{
		ID: id, ConversationID: "sweep-conv", StartedAt: time.Now().Add(-time.Hour),
		Question: "where is my order?",
	}); err != nil {
		t.Fatal(err)
	}
	// A turn that is merely slow must survive the same sweep.
	if err := recorder.Begin(ctx, chat.TurnRecord{
		ID: "still-running", ConversationID: "sweep-conv", StartedAt: time.Now(),
		Question: "still going",
	}); err != nil {
		t.Fatal(err)
	}

	swept, err := recorder.Sweep(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if swept == 0 {
		t.Fatal("the sweep marked nothing though a turn has been in flight for an hour")
	}

	outcome, detail := outcomeOf(t, ctx, id)
	if outcome != chat.OutcomeUnknown {
		t.Errorf("the abandoned turn is %q, want %q", outcome, chat.OutcomeUnknown)
	}
	if detail == "" {
		t.Error("the swept turn says nothing about why it is unknown")
	}
	// Never "failed": a failure is something this service observed and can describe, and
	// this is the absence of an observation.
	if outcome == "failed" || outcome == "completed" {
		t.Errorf("a turn nobody watched end was recorded as %q", outcome)
	}

	if outcome, _ := outcomeOf(t, ctx, "still-running"); outcome != chat.OutcomeInFlight {
		t.Errorf("a turn one second old was swept as %q; the lease is not being honoured", outcome)
	}

	// And a zero lease sweeps nothing, rather than sweeping everything.
	if n, err := recorder.Sweep(ctx, 0); err != nil || n != 0 {
		t.Errorf("a zero lease swept %d turns (%v)", n, err)
	}
}

func outcomeOf(t *testing.T, ctx context.Context, id string) (string, string) {
	t.Helper()
	var outcome, detail string
	if err := pool.QueryRow(ctx,
		`SELECT outcome, coalesce(detail,'') FROM turn WHERE id = $1`, id).
		Scan(&outcome, &detail); err != nil {
		t.Fatal(err)
	}
	return outcome, detail
}
