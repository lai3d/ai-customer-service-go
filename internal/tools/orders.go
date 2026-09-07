package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

type OrderStatus string

const (
	StatusPreparing        OrderStatus = "PREPARING"
	StatusInTransit        OrderStatus = "IN_TRANSIT"
	StatusDelivered        OrderStatus = "DELIVERED"
	StatusReturnInProgress OrderStatus = "RETURN_IN_PROGRESS"
	StatusCancelled        OrderStatus = "CANCELLED"
)

type Order struct {
	OrderNumber       string      `json:"orderNumber"`
	Status            OrderStatus `json:"status"`
	PlacedOn          string      `json:"placedOn"`
	EstimatedDelivery string      `json:"estimatedDelivery,omitempty"`
	Carrier           string      `json:"carrier,omitempty"`
	TrackingNumber    string      `json:"trackingNumber,omitempty"`
	Summary           string      `json:"summary"`
}

// Outcome is what happened to a lookup, in a closed vocabulary.
//
// Closed matters twice over. It becomes a Prometheus label and a span attribute in
// internal/chat, where an unbounded value is a cardinality bomb; and it is the thing that
// keeps a timeout, a 404 and a 500 from collapsing into one event on a dashboard, which
// is the difference between "customers are mistyping order numbers" and "the order system
// is down".
type Outcome string

const (
	// OutcomeFound: the order exists and is in the result.
	OutcomeFound Outcome = "found"
	// OutcomeNotFound: the source answered, and there is no such order. A fact about the
	// order number, not about the order system.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeTimedOut: the source did not answer inside the tool's budget.
	OutcomeTimedOut Outcome = "timed_out"
	// OutcomeUnavailable: the source answered that it could not serve the request (5xx,
	// 429) or could not be reached at all.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeUnreadable: the source answered with something this service cannot use --
	// malformed JSON, a payload with no status, a status code outside the contract.
	// Distinct from unavailable because it is a contract disagreement rather than an
	// outage, and the two get fixed by different people.
	OutcomeUnreadable Outcome = "unreadable"
	// OutcomeDenied: the source rejected this service's credentials. Distinct because a
	// wrong token otherwise reads as a permanent outage on every dashboard there is.
	OutcomeDenied Outcome = "denied"
	// OutcomeBadArguments: the model's arguments did not fit the schema. Never reaches a
	// source.
	OutcomeBadArguments Outcome = "bad_arguments"
)

// OrderSource is where an order comes from. Two implementations: MemoryOrders, which is
// the fixture this repository has always had, and HTTPOrders, which is the shape a real
// order service arrives through.
//
// The signature is the one internal/ticket uses, for the same reason -- a value, an
// outcome, and an error that is for the log rather than for the caller's control flow.
// Every failure the model is meant to handle is an Outcome; err carries the detail that
// explains an outcome to an operator and must never reach the model, because it is
// written for developers and the model's context is one edit away from a customer's
// screen.
type OrderSource interface {
	Lookup(ctx context.Context, orderNumber string) (Order, Outcome, error)
}

// Stand-in for the order system, in memory on purpose: a fake that answers instantly
// makes the model's behaviour -- when it decides to look an order up, and what it does
// with the answer -- the only variable. The dates match the Java implementation's so a
// conversation can be replayed against either.
var mockOrders = map[string]Order{
	"ORD-10042": {"ORD-10042", StatusInTransit, "2026-08-27", "2026-09-03",
		"SingPost", "SP884213906SG", "1 x Noise-cancelling headphones"},
	"ORD-10043": {"ORD-10043", StatusPreparing, "2026-08-31", "2026-09-05",
		"", "", "2 x Cotton t-shirt (M, navy)"},
	"ORD-10044": {"ORD-10044", StatusDelivered, "2026-08-18", "2026-08-22",
		"DHL", "JD0002088776", "1 x Espresso machine"},
	"ORD-10045": {"ORD-10045", StatusReturnInProgress, "2026-08-09", "2026-08-14",
		"DHL", "JD0002071140", "1 x Desk lamp"},
	"ORD-10046": {"ORD-10046", StatusCancelled, "2026-08-29", "",
		"", "", "1 x Mechanical keyboard"},
}

// MemoryOrders answers from the fixture above. It is the default source and the one the
// tests, the benchmark, the eval and the demo drive: those are measurements of the
// model's behaviour, and an order lookup that could be slow or absent would put variance
// into every one of them.
//
// It cannot fail, which is exactly why it is not evidence that the failure paths work.
// Those are exercised against a real HTTP server in orders_http_test.go.
type MemoryOrders struct{}

func NewMemoryOrders() *MemoryOrders { return &MemoryOrders{} }

// Fixture reports that this source is made up rather than a system of record. The server
// says so at start-up: a service answering from five hard-coded orders while everyone
// believes it is talking to the order system is the failure this seam exists to prevent.
func (*MemoryOrders) Fixture() bool { return true }

func (*MemoryOrders) Lookup(_ context.Context, orderNumber string) (Order, Outcome, error) {
	order, ok := mockOrders[orderNumber]
	if !ok {
		return Order{}, OutcomeNotFound, nil
	}
	return order, OutcomeFound, nil
}

// Fixture is what a source implements to admit it is one. Anything that does not is
// assumed to be real, because the failure being guarded against is a fixture believed to
// be real and never the other way round.
type Fixture interface {
	Fixture() bool
}

// IsFixture reports whether a source answers from made-up data.
func IsFixture(source OrderSource) bool {
	f, ok := source.(Fixture)
	return ok && f.Fixture()
}

type OrderLookup struct {
	source OrderSource
}

// NewOrderLookup builds the tool over a source. With no source it uses the in-memory
// fixture, which keeps every existing caller -- the tests, the benchmark, the eval, the
// demo -- working unchanged, and makes using the real thing a deliberate act.
func NewOrderLookup(source ...OrderSource) *OrderLookup {
	t := &OrderLookup{source: NewMemoryOrders()}
	if len(source) > 0 && source[0] != nil {
		t.source = source[0]
	}
	return t
}

// Source is what the start-up log reads to say which one is wired.
func (t *OrderLookup) Source() OrderSource { return t.source }

func (t *OrderLookup) Definition() Definition {
	return Definition{
		Name: "lookup_order_status",
		Description: "Look up the current delivery status of one order by its order number. " +
			"Use this whenever a customer asks where their order is, when it will arrive, " +
			"or whether it has shipped. Returns the status, estimated delivery date, and " +
			"carrier tracking details when they exist. Does not modify the order. If the " +
			"order number cannot be found the result says so, which means the customer " +
			"should be asked to check it rather than told the order does not exist.",
		Properties: map[string]any{
			"orderNumber": map[string]any{
				"type":        "string",
				"description": "The order number, for example ORD-10042",
			},
		},
		Required: []string{"orderNumber"},
	}
}

type orderLookupArgs struct {
	OrderNumber string `json:"orderNumber"`
}

// lookupResult is deliberately a value with found:false rather than an error.
//
// Whatever a tool layer does with a returned error, the model ends up seeing something
// written for developers -- and it has nothing to reason about. found:false with a plain
// explanation lets the assistant say "I can't find that order number, could you check
// it?" instead.
//
// Reason carries the outcome into the model's own input, from the closed set above. It is
// there so that the difference between "there is no such order" and "the order system did
// not answer" survives into the model's context rather than existing only on a dashboard.
type lookupResult struct {
	Found       bool    `json:"found"`
	Order       *Order  `json:"order,omitempty"`
	Reason      Outcome `json:"reason,omitempty"`
	Explanation string  `json:"explanation,omitempty"`
}

// explanations is what the model reads for each way a lookup can fail. They are written
// for the model to relay, so they say what the customer should be told and what the
// assistant must not do -- guessing is the failure that matters, because a plausible
// invented "it's out for delivery" is much worse than an apology.
//
// A missing order, a slow order system and an unreachable one are three different things
// to a customer and have three different sentences. unreadable and denied share the
// unavailable wording, and that is the one deliberate collapse here: they are different
// things to an operator and the same thing to a customer, which is "we cannot check right
// now". A rejected credential in particular says nothing about credentials -- that
// belongs in the log and the metric label, where the person who can fix it is looking,
// and not in a paragraph a model may decide to read out.
var explanations = map[Outcome]string{
	OutcomeNotFound: "No order matches that number. It may have been mistyped, or it " +
		"may belong to a different account.",
	OutcomeTimedOut: "The order system did not answer in time. This says nothing about " +
		"the order itself: tell the customer the status could not be read just now and " +
		"offer to try again in a moment. Do not guess at a status.",
	OutcomeUnavailable: "The order system cannot be reached at the moment. This is a " +
		"problem on our side rather than with the customer's order: say so plainly, do " +
		"not guess at a status, and offer to raise a support ticket so a person can check.",
}

func explain(outcome Outcome) string {
	if text, ok := explanations[outcome]; ok {
		return text
	}
	return explanations[OutcomeUnavailable]
}

func (t *OrderLookup) Invoke(ctx context.Context, _ string, arguments json.RawMessage) (Result, error) {
	var args orderLookupArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		// The model produced arguments that do not fit the schema. Telling it so is
		// more useful than failing the turn, and the text says nothing internal.
		return Result{
			Content: mustJSON(lookupResult{Found: false, Reason: OutcomeBadArguments,
				Explanation: "The order number argument could not be read. Ask the customer to repeat it."}),
			Outcome: string(OutcomeBadArguments),
		}, nil
	}

	// Customers paste order numbers out of emails, so a model relaying "ord-10042 "
	// should not be told the order does not exist. Normalising here rather than in a
	// source means both sources get it and neither has to remember.
	key := strings.ToUpper(strings.TrimSpace(args.OrderNumber))

	order, outcome, err := t.source.Lookup(ctx, key)
	if err != nil {
		// The one place this error is allowed to exist. It is written for a developer, it
		// names hosts and status codes, and it goes to the log -- not to the model, and
		// not into the turn's error path either. The outcome beside it is what the model
		// and the meters get.
		slog.Error("order lookup failed", "outcome", outcome, "error", err)
	}

	// Note what is not here: a branch that returns an error. A failing order service is
	// somebody else's outage arriving through a seam this service does not control, and
	// the model has something useful and different to say about each one. internal/ticket
	// is the opposite case -- a storage failure there is this service's own database, and
	// there is nothing for the model to do with it but say the one fixed sentence.
	switch outcome {
	case OutcomeFound:
		return Result{
			Content: mustJSON(lookupResult{Found: true, Order: &order, Reason: OutcomeFound}),
			Outcome: string(OutcomeFound),
		}, nil
	case OutcomeNotFound, OutcomeTimedOut, OutcomeUnavailable, OutcomeUnreadable, OutcomeDenied:
		return Result{
			Content: mustJSON(lookupResult{Found: false, Reason: outcome, Explanation: explain(outcome)}),
			Outcome: string(outcome),
		}, nil
	default:
		// A source returned an outcome outside the closed set. Reading it as unavailable
		// is the safe choice, and the metric label stays bounded whatever a future source
		// decides to invent.
		slog.Error("order source returned an unknown outcome", "outcome", outcome)
		return Result{
			Content: mustJSON(lookupResult{Found: false, Reason: OutcomeUnavailable,
				Explanation: explain(OutcomeUnavailable)}),
			Outcome: string(OutcomeUnavailable),
		}, nil
	}
}
