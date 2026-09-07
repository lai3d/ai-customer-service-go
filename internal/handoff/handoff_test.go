package handoff_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/handoff"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

func newTicket(t *testing.T, conversationID string) string {
	t.Helper()
	tk, _, err := ticket.NewStore(pool).Create(context.Background(), ticket.CreateRequest{
		ConversationID: conversationID, Summary: "customer wants a human about ORD-10045",
		Category: "returns", OrderNumber: "ORD-10045"})
	if err != nil {
		t.Fatal(err)
	}
	return tk.Number
}

func store(notifier *handoff.Notifier) *handoff.Store {
	return handoff.NewStore(pool, chat.NewMemory(pool, 40), notifier)
}

// The half nobody builds. A ticket a human answers and a customer never hears about is,
// from the only point of view that matters, a ticket nobody answered.
func TestAnOperatorsReplyReachesTheCustomersConversation(t *testing.T) {
	ctx := context.Background()
	conversation := "handoff-" + fmt.Sprint(time.Now().UnixNano())
	memory := chat.NewMemory(pool, 40)
	if err := memory.Append(ctx, conversation, "user", "where is my refund?"); err != nil {
		t.Fatal(err)
	}
	number := newTicket(t, conversation)

	s := store(handoff.NewNotifier(pool, "", 0))
	if err := s.Reply(ctx, number, "alex", "Your refund went back to your card this morning."); err != nil {
		t.Fatal(err)
	}

	messages, err := s.Transcript(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("the transcript has %d messages, want the question and the human's answer", len(messages))
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" {
		t.Errorf("the reply arrived as role %q", last.Role)
	}
	if !strings.Contains(last.Content, "refund went back") {
		t.Errorf("the reply is not in the conversation: %q", last.Content)
	}
	// The customer cannot see a database column. Without the name in the words, a person's
	// answer is indistinguishable from the machine's.
	if !strings.Contains(last.Content, "alex") {
		t.Errorf("the reply is not attributed to a person: %q", last.Content)
	}

	// And the model must be able to see it, or the next turn tells the customer to wait
	// for something that already arrived.
	history, err := memory.History(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || !strings.Contains(history[len(history)-1].Text, "refund went back") {
		t.Error("the human's reply is not in the history the model composes from")
	}

	// The ticket's own history keeps it too, which is where an erasure can redact it.
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket_event WHERE ticket_number = $1 AND action = 'replied'`,
		number).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("the ticket has %d reply events, want 1", events)
	}
}

func TestAReplyToNothingIsRefusedRatherThanRecorded(t *testing.T) {
	ctx := context.Background()
	s := store(handoff.NewNotifier(pool, "", 0))

	if err := s.Reply(ctx, "TKT-does-not-exist", "alex", "hello"); err != handoff.ErrNoSuchTicket {
		t.Errorf("replying to an unknown ticket returned %v", err)
	}
	conversation := "empty-" + fmt.Sprint(time.Now().UnixNano())
	number := newTicket(t, conversation)
	if err := s.Reply(ctx, number, "alex", "   "); err != handoff.ErrEmptyReply {
		t.Errorf("an empty reply returned %v", err)
	}
	// Neither may leave a message in the customer's conversation.
	messages, err := s.Transcript(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Errorf("a refused reply put %d messages in the conversation", len(messages))
	}
}

// Outbound. The webhook carries no customer text on purpose: everything else in this
// service works to keep that text out of the places it leaks to, and a webhook is by
// definition outside this service's control.
func TestTheNotificationSaysWhatHappenedAndNotWhatWasSaid(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var bodies []string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		mu.Lock()
		bodies = append(bodies, string(body[:n]))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()

	conversation := "notify-" + fmt.Sprint(time.Now().UnixNano())
	memory := chat.NewMemory(pool, 40)
	if err := memory.Append(ctx, conversation, "user",
		"my card 4111 1111 1111 1111 was charged twice"); err != nil {
		t.Fatal(err)
	}
	number := newTicket(t, conversation)

	s := store(handoff.NewNotifier(pool, destination.URL, time.Second))
	if err := s.Reply(ctx, number, "alex", "Refunded the duplicate charge."); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) > 0 })
	mu.Lock()
	body := bodies[0]
	mu.Unlock()

	var event handoff.Event
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("the destination received something that is not the event: %q", body)
	}
	if event.Ticket != number || event.Type != "ticket.replied" {
		t.Errorf("unexpected event: %+v", event)
	}
	for _, secret := range []string{"4111", "Refunded the duplicate", "charged twice"} {
		if strings.Contains(body, secret) {
			t.Errorf("the webhook body carries customer or reply text (%q): %s", secret, body)
		}
	}
}

// The failure mode of a notification is silence, and silence is indistinguishable from
// "nothing happened". Nobody chases a message they do not know was sent.
func TestAFailedNotificationIsRecordedRatherThanLost(t *testing.T) {
	ctx := context.Background()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer destination.Close()

	conversation := "fail-" + fmt.Sprint(time.Now().UnixNano())
	number := newTicket(t, conversation)
	notifier := handoff.NewNotifier(pool, destination.URL, time.Second)

	if err := store(notifier).Reply(ctx, number, "alex", "we are on it"); err != nil {
		// The customer's reply must land even when the chat room is down. Breaking the
		// thing that worked because the thing that did not failed is the wrong trade.
		t.Fatalf("a failing webhook failed the reply: %v", err)
	}

	waitFor(t, func() bool {
		var n int
		pool.QueryRow(ctx,
			`SELECT count(*) FROM handoff_delivery WHERE ticket_number = $1 AND failure IS NOT NULL`,
			number).Scan(&n)
		return n > 0
	})

	var failure string
	var status int
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(failure,''), status FROM handoff_delivery
		WHERE ticket_number = $1 ORDER BY id DESC LIMIT 1`, number).Scan(&failure, &status); err != nil {
		t.Fatalf("the failed delivery left no record: %v", err)
	}
	if failure == "" {
		t.Error("the delivery was recorded as a success")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("the record says status %d", status)
	}

	// And the reply still reached the customer.
	messages, err := store(notifier).Transcript(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 {
		t.Error("the customer got nothing because a webhook was down")
	}

	undelivered, err := notifier.Undelivered(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if undelivered == 0 {
		t.Error("Undelivered reports nothing, so the operations overview cannot show it")
	}
}

func TestWithNoWebhookNothingIsSentAndNothingBreaks(t *testing.T) {
	ctx := context.Background()
	conversation := "quiet-" + fmt.Sprint(time.Now().UnixNano())
	number := newTicket(t, conversation)
	notifier := handoff.NewNotifier(pool, "", 0)
	if notifier.Enabled() {
		t.Error("an empty URL produced an enabled notifier")
	}
	if err := store(notifier).Reply(ctx, number, "alex", "hello"); err != nil {
		t.Fatalf("a reply failed with no webhook configured: %v", err)
	}
	var deliveries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM handoff_delivery WHERE ticket_number = $1`, number).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Errorf("%d deliveries were recorded with no destination configured", deliveries)
	}
}

// The row is the record and the counter is the smoke detector, and the counter is the
// half an alert can reach. `handoff_delivery` has always held every outcome, but a row
// waits for somebody to open the operations UI and notice a number in red.
//
// Asserted through the real Notifier against destinations that refuse and accept, rather
// than by calling the counter directly: the increment lives on the same path that writes
// the row, and a test that drove the metric itself would prove only that a counter counts.
func TestAnUndeliveredNotificationIsCountedAsWellAsRecorded(t *testing.T) {
	ctx := context.Background()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer refusing.Close()
	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer accepting.Close()

	metrics := obs.NewMetrics()
	count := func(outcome string) float64 {
		return testutil.ToFloat64(metrics.Handoffs.WithLabelValues("ticket.replied", outcome))
	}

	conversation := "metered-" + fmt.Sprint(time.Now().UnixNano())
	number := newTicket(t, conversation)
	failing := handoff.NewNotifier(pool, refusing.URL, time.Second).Meter(metrics)
	if err := store(failing).Reply(ctx, number, "alex", "we are on it"); err != nil {
		t.Fatalf("a failing webhook failed the reply: %v", err)
	}
	waitFor(t, func() bool { return count("failed") > 0 })
	if got := count("delivered"); got != 0 {
		t.Errorf("a refused notification counted %v deliveries", got)
	}

	working := handoff.NewNotifier(pool, accepting.URL, time.Second).Meter(metrics)
	if err := store(working).Reply(ctx, number, "alex", "and it is done"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return count("delivered") > 0 })
	if got := count("failed"); got != 1 {
		t.Errorf("the failure count moved to %v while a delivery succeeded; two attempts "+
			"against one destination are one outcome, not two", got)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the asynchronous delivery")
}
