package tools

import (
	"context"
	"encoding/json"
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

type OrderLookup struct{}

func NewOrderLookup() *OrderLookup { return &OrderLookup{} }

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
type lookupResult struct {
	Found       bool   `json:"found"`
	Order       *Order `json:"order,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

func (t *OrderLookup) Invoke(_ context.Context, _ string, arguments json.RawMessage) (Result, error) {
	var args orderLookupArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		// The model produced arguments that do not fit the schema. Telling it so is
		// more useful than failing the turn, and the text says nothing internal.
		return Result{
			Content: mustJSON(lookupResult{Found: false,
				Explanation: "The order number argument could not be read. Ask the customer to repeat it."}),
			Outcome: "bad_arguments",
		}, nil
	}

	// Customers paste order numbers out of emails, so a model relaying "ord-10042 "
	// should not be told the order does not exist.
	key := strings.ToUpper(strings.TrimSpace(args.OrderNumber))
	order, ok := mockOrders[key]
	if !ok {
		return Result{
			Content: mustJSON(lookupResult{Found: false,
				Explanation: "No order matches that number. It may have been mistyped, or it " +
					"may belong to a different account."}),
			Outcome: "not_found",
		}, nil
	}
	return Result{Content: mustJSON(lookupResult{Found: true, Order: &order}), Outcome: "found"}, nil
}
