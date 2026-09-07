package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

// Everything in this file drives the real adapter against a real HTTP server.
//
// Not a stub of tools.OrderSource. A stub can satisfy any contract -- this repository
// shipped a defect exactly that way, a fake llm client returning usage that no real
// client returned, and the note is in CLAUDE.md under "Never return early from a client
// Stream on error". The failure modes worth having tests for here are net/http's:
// a body that stops, a status code nobody planned for, a server that accepts the
// connection and then says nothing. None of them exists above the seam.
//
// **There is no real order service.** These tests say that the adapter behaves as
// designed against a server that behaves as the contract in orders_http.go describes.
// They say nothing about whether that contract matches anybody's order system.

const token = "s3cret-order-token"

// orderService is an httptest server plus a count of the requests that reached it. The
// count is the part that makes a retry assertion possible: an outcome alone cannot tell
// one attempt from two.
type orderService struct {
	*httptest.Server
	requests atomic.Int32
	paths    chan string
	auth     chan string
}

func newOrderService(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *orderService {
	t.Helper()
	s := &orderService{paths: make(chan string, 16), auth: make(chan string, 16)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		// r.RequestURI, not r.URL.Path. The Path field is what net/http *decoded*, so
		// asserting on it measures the server's unescaping rather than what this service
		// put on the wire -- and it reports an escaped `%2F..%2F` as a traversal that
		// never happened. This assertion was red for exactly that reason before the
		// distinction was noticed.
		select {
		case s.paths <- r.Method + " " + r.RequestURI:
		default:
		}
		select {
		case s.auth <- r.Header.Get("Authorization"):
		default:
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// lookupAgainst builds the tool over the HTTP adapter and calls it the way chat.Service
// does -- through Invoke, with JSON arguments, because the assertion that matters is
// about what the model receives and not about what a Go function returns.
func lookupAgainst(t *testing.T, s *orderService, number string, opts ...func(*tools.HTTPOptions)) tools.Result {
	t.Helper()
	options := tools.HTTPOptions{BaseURL: s.URL, Token: token, Timeout: 2 * time.Second, Attempts: 2}
	for _, apply := range opts {
		apply(&options)
	}
	source, err := tools.NewHTTPOrders(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.NewOrderLookup(source).Invoke(context.Background(), "c1",
		args(t, map[string]string{"orderNumber": number}))
	// The whole contract in one line: nothing the order service does is an exception.
	// An exception's message is what a customer ends up reading.
	if err != nil {
		t.Fatalf("the order service made the tool return an error: %v", err)
	}
	return result
}

func fixture(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"orderNumber":"ORD-77001","status":"in_transit",
		"placedOn":"2026-09-01","estimatedDelivery":"2026-09-08","carrier":"DHL",
		"trackingNumber":"JD0002099311","summary":"1 x Standing desk"}`))
}

// The happy path, and the wire contract it depends on. If the real order service does not
// look like this, this is the test that has to change -- which is the point of writing the
// request assertions down rather than only the response ones.
func TestAnOrderComesBackFromTheServiceTheWayTheContractSays(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) { fixture(w) })

	result := lookupAgainst(t, s, "ord-77001  ")

	if got := <-s.paths; got != "GET /orders/ORD-77001" {
		// Uppercased and trimmed before it goes on the wire: customers paste order
		// numbers out of emails, and the normalisation has to happen for both sources
		// rather than only for the one that has a map to miss.
		t.Errorf("the adapter asked for %q, want %q", got, "GET /orders/ORD-77001")
	}
	if got := <-s.auth; got != "Bearer "+token {
		t.Errorf("Authorization was %q, want a bearer token", got)
	}
	if result.Outcome != "found" {
		t.Errorf("outcome is %q, want found", result.Outcome)
	}
	got := decode[lookupResult](t, result)
	if !got.Found || got.Order == nil {
		t.Fatalf("the order did not reach the model: %s", result.Content)
	}
	if got.Order.OrderNumber != "ORD-77001" || got.Order.TrackingNumber != "JD0002099311" {
		t.Errorf("the order reached the model wrong: %+v", got.Order)
	}
	// Normalised on the way in. A real service reporting `in_transit` and this one
	// reporting `IN_TRANSIT` would otherwise be two different things to the model.
	if got.Order.Status != "IN_TRANSIT" {
		t.Errorf("status reached the model as %q, want IN_TRANSIT", got.Order.Status)
	}
}

// 404. A fact about the order number, and the one failure that is not about the service.
func TestAnOrderTheServiceDoesNotHaveIsAValue(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"no such order"}`, http.StatusNotFound)
	})

	result := lookupAgainst(t, s, "ORD-00000")

	if result.Outcome != "not_found" {
		t.Errorf("outcome is %q, want not_found", result.Outcome)
	}
	got := decode[lookupResult](t, result)
	if got.Found {
		t.Error("reported found for an order the service does not have")
	}
	if !strings.Contains(got.Explanation, "mistyped") {
		t.Errorf("a not-found explanation should send the customer back to the number: %q",
			got.Explanation)
	}
	// Not retried: the order will not have appeared a hundred milliseconds later, and a
	// customer reading out a wrong number should not cost two requests.
	if n := s.requests.Load(); n != 1 {
		t.Errorf("a 404 cost %d requests, want 1", n)
	}
}

// 500, and the bounded retry. The common failure is a moment rather than a state, so one
// more try is worth the milliseconds -- and only one, because a retry loop against a
// service that is down is a way of turning one outage into two.
func TestAFailingOrderServiceIsRetriedOnceAndThenBecomesAValue(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded: connection to orders-db-3 refused", http.StatusInternalServerError)
	})

	result := lookupAgainst(t, s, "ORD-77001")

	if result.Outcome != "unavailable" {
		t.Errorf("outcome is %q, want unavailable", result.Outcome)
	}
	if n := s.requests.Load(); n != 2 {
		t.Errorf("a 500 cost %d requests, want 2 -- one try and one retry", n)
	}
	got := decode[lookupResult](t, result)
	if got.Found {
		t.Error("reported found while the service was failing")
	}
	// What the model is told to do about it. "Do not guess" is the load-bearing half:
	// an invented "it's out for delivery" is much worse than an apology.
	for _, phrase := range []string{"cannot be reached", "not guess", "support ticket"} {
		if !strings.Contains(got.Explanation, phrase) {
			t.Errorf("the explanation does not say %q: %q", phrase, got.Explanation)
		}
	}
	// The upstream's own words are for the log. They name internal hosts.
	if strings.Contains(result.Content, "orders-db-3") {
		t.Errorf("the order service's internal error text reached the model: %q", result.Content)
	}
}

// A server that accepts the connection and never answers -- the failure that a status
// code cannot express and that an http.Client with no timeout waits on for ever.
func TestAnOrderServiceThatNeverAnswersCostsTheBudgetAndNothingMore(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := newOrderService(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	budget := 300 * time.Millisecond
	started := time.Now()
	result := lookupAgainst(t, s, "ORD-77001", func(o *tools.HTTPOptions) { o.Timeout = budget })
	elapsed := time.Since(started)

	if result.Outcome != "timed_out" {
		t.Errorf("outcome is %q, want timed_out -- a slow order system is not a missing order",
			result.Outcome)
	}
	// The property that matters is not the outcome, it is that the turn got its budget
	// back. The tool call happens inside a turn the customer is watching.
	// budget*2 rather than a generous multiple: the mistake this is watching for is a
	// per-attempt timeout, which doubles the wait exactly. A slacker bound passes for it.
	if elapsed > budget*2 {
		t.Errorf("a %s budget took %s; the tool held the turn open", budget, elapsed)
	}
	// One attempt, because the budget the retry would have used is what the first
	// attempt spent. A second try here would make a 300ms adapter a 600ms one.
	if n := s.requests.Load(); n != 1 {
		t.Errorf("a timeout cost %d requests, want 1: the budget covers the whole lookup", n)
	}
	got := decode[lookupResult](t, result)
	if !strings.Contains(got.Explanation, "did not answer in time") {
		t.Errorf("the model was not told it was a timeout: %q", got.Explanation)
	}
	if strings.Contains(strings.ToLower(result.Content), "context deadline") {
		t.Errorf("a Go error string reached the model: %q", result.Content)
	}
}

// 200 with a body that is not an order. Three shapes, because they fail in three
// different places and the middle one is the one that gets shipped: json.Unmarshal is
// perfectly happy with `{}`.
func TestAnAnswerThisServiceCannotReadIsNeverPassedOnAsAnOrder(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not JSON at all", `<html><body>Sign in to continue</body></html>`},
		{"JSON with nothing in it", `{}`},
		{"an order with no status", `{"orderNumber":"ORD-77001","summary":"1 x Desk"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			result := lookupAgainst(t, s, "ORD-77001")

			if result.Outcome != "unreadable" {
				t.Errorf("outcome is %q, want unreadable", result.Outcome)
			}
			got := decode[lookupResult](t, result)
			if got.Found || got.Order != nil {
				t.Errorf("an unreadable answer became an order the model can describe: %s",
					result.Content)
			}
			// Not retried. A malformed body is a disagreement about the contract, and
			// the second attempt disagrees identically.
			if n := s.requests.Load(); n != 1 {
				t.Errorf("an unreadable answer cost %d requests, want 1", n)
			}
			// Neither the body nor the decoder's complaint. A 200 from the wrong host is
			// somebody's login page, and "invalid character '<'" is written for a
			// developer.
			if strings.Contains(result.Content, "Sign in") ||
				strings.Contains(result.Content, "invalid character") {
				t.Errorf("the unreadable body reached the model: %q", result.Content)
			}
		})
	}
}

// A body big enough to be a bill. The response is decoded into seven fields, but Summary
// is free text from another system and a turn is billed by the token.
func TestAnEnormousAnswerIsRefusedRatherThanBilled(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"orderNumber":"ORD-77001","status":"IN_TRANSIT","summary":"` +
			strings.Repeat("A", 128<<10) + `"}`))
	})

	result := lookupAgainst(t, s, "ORD-77001")

	if result.Outcome != "unreadable" {
		t.Errorf("outcome is %q, want unreadable", result.Outcome)
	}
	if len(result.Content) > 4096 {
		t.Errorf("%d bytes of it reached the model's context", len(result.Content))
	}
}

// A rejected credential. Its own outcome because a rotated token otherwise reads as a
// permanent outage on every dashboard there is -- and the same words to the customer as
// an outage, because from where they are sitting it is one.
func TestARejectedCredentialIsItsOwnOutcomeAndSaysNothingAboutCredentials(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid bearer token", status)
		})

		result := lookupAgainst(t, s, "ORD-77001")

		if result.Outcome != "denied" {
			t.Errorf("HTTP %d produced outcome %q, want denied", status, result.Outcome)
		}
		// Not retried: the token will not have become valid in a hundred milliseconds.
		if n := s.requests.Load(); n != 1 {
			t.Errorf("HTTP %d cost %d requests, want 1", status, n)
		}
		lowered := strings.ToLower(result.Content)
		for _, leak := range []string{"credential", "token", "bearer", "authoriz", "401", "403"} {
			if strings.Contains(lowered, leak) {
				t.Errorf("HTTP %d put %q in front of the model: %q", status, leak, result.Content)
			}
		}
	}
}

// The requirement in one test: a timeout, a 404 and a 500 are three different things to a
// customer, and the seam must not let them collapse. Collapsing is the easy mistake --
// one `return errors.New("lookup failed")` does it, and every test above still passes.
func TestATimeoutA404AndA500StayThreeDifferentThings(t *testing.T) {
	slow := make(chan struct{})
	t.Cleanup(func() { close(slow) })

	handlers := map[string]func(w http.ResponseWriter, r *http.Request){
		"missing": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"broken": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"slow": func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-slow:
			case <-r.Context().Done():
			}
		},
	}

	outcomes := map[string]string{}
	explanations := map[string]string{}
	for name, handler := range handlers {
		s := newOrderService(t, handler)
		result := lookupAgainst(t, s, "ORD-77001",
			func(o *tools.HTTPOptions) { o.Timeout = 300 * time.Millisecond })
		outcomes[name] = result.Outcome
		explanations[name] = decode[lookupResult](t, result).Explanation
	}

	if len(distinct(outcomes)) != 3 {
		t.Errorf("three failures produced %v; the meters cannot tell them apart", outcomes)
	}
	if len(distinct(explanations)) != 3 {
		t.Errorf("three failures produced %d distinct explanations; "+
			"the customer cannot tell them apart: %v", len(distinct(explanations)), explanations)
	}
	// And the reason travels into the model's own input, not only onto a dashboard.
	if outcomes["missing"] != "not_found" || outcomes["broken"] != "unavailable" ||
		outcomes["slow"] != "timed_out" {
		t.Errorf("outcomes are %v", outcomes)
	}
}

func distinct(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, v := range m {
		out[v] = true
	}
	return out
}

// The order number is written by the model, from what the customer typed. Concatenating
// it into a URL makes "what shall I ask the order service for" a decision the customer
// gets to make.
func TestAnOrderNumberCannotSteerTheRequestSomewhereElse(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) { fixture(w) })

	lookupAgainst(t, s, "../../internal/all?dump=true")

	// The raw request target, as it went out. One path segment under /orders/ and no
	// query string: every separator the model wrote is escaped into the segment.
	got := <-s.paths
	target := strings.TrimPrefix(got, "GET ")
	if !strings.HasPrefix(target, "/orders/") {
		t.Fatalf("the request went to %q", got)
	}
	segment := strings.TrimPrefix(target, "/orders/")
	if strings.ContainsAny(segment, "/?#") {
		t.Errorf("the order number escaped its path segment: %q", got)
	}
}

// A status code outside the contract. Not an outage and not a missing order: this service
// and that one disagree about what the API is, and those get fixed by different people.
func TestAStatusCodeOutsideTheContractIsAContractProblem(t *testing.T) {
	s := newOrderService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	result := lookupAgainst(t, s, "ORD-77001")

	if result.Outcome != "unreadable" {
		t.Errorf("outcome is %q, want unreadable", result.Outcome)
	}
}

// Nothing about the deployment reaches the model on any failing path: not the host, not
// the port, not the credential. The model's context is one turn away from a customer's
// screen.
func TestNoFailingPathLeaksTheDeploymentIntoTheModelsContext(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	handlers := []func(w http.ResponseWriter, r *http.Request){
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) },
		func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-stall:
			case <-r.Context().Done():
			}
		},
	}
	for i, handler := range handlers {
		s := newOrderService(t, handler)
		result := lookupAgainst(t, s, "ORD-77001",
			func(o *tools.HTTPOptions) { o.Timeout = 300 * time.Millisecond })
		host := strings.TrimPrefix(s.URL, "http://")
		for _, leak := range []string{token, s.URL, host} {
			if strings.Contains(result.Content, leak) {
				t.Errorf("handler %d put %q in the model's context: %q", i, leak, result.Content)
			}
		}
		// And it is JSON in the shape the model has been reading all along.
		var probe map[string]any
		if err := json.Unmarshal([]byte(result.Content), &probe); err != nil {
			t.Errorf("handler %d produced content that is not JSON: %q", i, result.Content)
		}
	}
}

// The credential does not reach the log either, and this is the path that would put it
// there without anybody deciding to.
//
// net/http puts the request URL into every transport error, and an order service whose
// credential lives in the URL -- which somebody will configure, because plenty of internal
// services want it there -- writes that URL into this service's log on every failure. The
// log is the one place a token can sit for months looking like nothing.
func TestACredentialInTheURLDoesNotReachTheLog(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// Port 1 on the loopback: refused immediately, so this costs a millisecond rather
	// than a timeout, and the refusal carries the URL.
	source, err := tools.NewHTTPOrders(tools.HTTPOptions{
		BaseURL: "http://127.0.0.1:1/" + token, Token: token,
		Timeout: time.Second, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.NewOrderLookup(source).Invoke(context.Background(), "c1",
		args(t, map[string]string{"orderNumber": "ORD-77001"}))
	if err != nil {
		t.Fatalf("an unreachable order service returned an error: %v", err)
	}
	if result.Outcome != "unavailable" {
		t.Errorf("a refused connection produced outcome %q, want unavailable", result.Outcome)
	}
	if strings.Contains(logged.String(), token) {
		t.Errorf("the credential is in the log: %s", logged.String())
	}
	// And the line is still worth having: without the URL in it, nobody can tell which
	// order service failed.
	if !strings.Contains(logged.String(), "redacted") {
		t.Errorf("the log line lost the URL entirely rather than redacting it: %s", logged.String())
	}
}

// A source that cannot be built must stop the process rather than answer nothing for
// ever. Every one of these is a value somebody typed.
func TestAnUnusableOrderServiceConfigurationIsRefused(t *testing.T) {
	good := tools.HTTPOptions{BaseURL: "https://orders.internal", Token: "t",
		Timeout: time.Second, Attempts: 2}
	cases := map[string]func(*tools.HTTPOptions){
		"no URL":       func(o *tools.HTTPOptions) { o.BaseURL = "" },
		"not a URL":    func(o *tools.HTTPOptions) { o.BaseURL = "orders.internal" },
		"no host":      func(o *tools.HTTPOptions) { o.BaseURL = "https://" },
		"no timeout":   func(o *tools.HTTPOptions) { o.Timeout = 0 },
		"no attempts":  func(o *tools.HTTPOptions) { o.Attempts = 0 },
		"bad attempts": func(o *tools.HTTPOptions) { o.Attempts = -1 },
	}
	for name, breakIt := range cases {
		opts := good
		breakIt(&opts)
		if _, err := tools.NewHTTPOrders(opts); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := tools.NewHTTPOrders(good); err != nil {
		t.Errorf("a usable configuration was refused: %v", err)
	}
}

// The default source is the fixture, and it says so.
//
// This is the item's whole point: a service answering from five hard-coded orders while
// everyone believes it is talking to the order system is indistinguishable from a working
// one. Something has to be able to ask.
func TestTheDefaultSourceIsAFixtureAndAdmitsIt(t *testing.T) {
	if !tools.IsFixture(tools.NewOrderLookup().Source()) {
		t.Error("the default order source does not report itself as a fixture; " +
			"nothing at start-up can then tell anyone what it is answering from")
	}
	real, err := tools.NewHTTPOrders(tools.HTTPOptions{
		BaseURL: "https://orders.internal", Timeout: time.Second, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if tools.IsFixture(real) {
		t.Error("the HTTP source claims to be a fixture")
	}
	if tools.NewOrderLookup(real).Source() != tools.OrderSource(real) {
		t.Error("a configured source did not reach the tool")
	}
}
