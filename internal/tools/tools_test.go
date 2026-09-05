package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decode[T any](t *testing.T, result tools.Result) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("tool result is not JSON the model can read: %v (%q)", err, result.Content)
	}
	return out
}

type lookupResult struct {
	Found bool `json:"found"`
	Order *struct {
		OrderNumber    string `json:"orderNumber"`
		Status         string `json:"status"`
		TrackingNumber string `json:"trackingNumber"`
	} `json:"order"`
	Explanation string `json:"explanation"`
}

// A missing order is a value, not an error.
//
// Whatever a tool layer does with a returned error, the model ends up seeing something
// written for developers -- and it has nothing to reason about. found:false lets the
// assistant ask the customer to check the number.
func TestAMissingOrderIsAValueRatherThanAnError(t *testing.T) {
	tool := tools.NewOrderLookup()

	result, err := tool.Invoke(context.Background(), "c1",
		args(t, map[string]string{"orderNumber": "ORD-99999"}))
	if err != nil {
		t.Fatalf("a missing order must not be an error: %v", err)
	}
	got := decode[lookupResult](t, result)
	if got.Found {
		t.Error("reported found for an order that does not exist")
	}
	if got.Explanation == "" {
		t.Error("a not-found result needs an explanation the assistant can relay")
	}
	if result.Outcome != "not_found" {
		t.Errorf("outcome is %q, want not_found", result.Outcome)
	}
}

// Customers paste order numbers out of emails.
func TestOrderLookupToleratesCaseAndWhitespace(t *testing.T) {
	tool := tools.NewOrderLookup()
	for _, number := range []string{"ORD-10042", "ord-10042", "  ORD-10042  ", "Ord-10042\n"} {
		result, err := tool.Invoke(context.Background(), "c1",
			args(t, map[string]string{"orderNumber": number}))
		if err != nil {
			t.Fatal(err)
		}
		got := decode[lookupResult](t, result)
		if !got.Found {
			t.Errorf("%q was not found", number)
			continue
		}
		if got.Order.TrackingNumber != "SP884213906SG" {
			t.Errorf("%q returned the wrong order: %+v", number, got.Order)
		}
	}
}

// Arguments the model got wrong should not fail the turn.
func TestUnreadableArgumentsBecomeSomethingTheModelCanAnswerWith(t *testing.T) {
	tool := tools.NewOrderLookup()
	result, err := tool.Invoke(context.Background(), "c1", json.RawMessage(`{"orderNumber": 42}`))
	if err != nil {
		t.Fatalf("bad arguments must not be an error: %v", err)
	}
	if result.Outcome != "bad_arguments" {
		t.Errorf("outcome is %q, want bad_arguments", result.Outcome)
	}
	if strings.Contains(result.Content, "json:") || strings.Contains(result.Content, "cannot unmarshal") {
		t.Errorf("the model was handed a Go error string: %q", result.Content)
	}
}

type ticketResult struct {
	Created bool `json:"created"`
	Ticket  *struct {
		TicketNumber   string `json:"ticketNumber"`
		Category       string `json:"category"`
		AlreadyExisted bool   `json:"alreadyExisted"`
	} `json:"ticket"`
	Refusal string `json:"refusal"`
}

func createTicket(t *testing.T, tool *tools.SupportTickets, conversationID, summary string) tools.Result {
	t.Helper()
	result, err := tool.Invoke(context.Background(), conversationID, args(t, map[string]string{
		"summary": summary, "category": "returns",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The tool's job is turning three storage outcomes into three things a model can act on.
// Whether the cap and the deduplication are *correct* is a property of the schema and is
// tested in internal/ticket against a real Postgres; what is tested here is that neither
// reaches the model as an error.
func TestEveryStorageOutcomeReachesTheModelAsAValue(t *testing.T) {
	cases := []struct {
		outcome ticket.Outcome
		wantKey string
	}{
		{ticket.OutcomeCreated, `"created":true`},
		{ticket.OutcomeExisted, `"created":false`},
		{ticket.OutcomeCapped, `"refusal"`},
	}
	for _, tc := range cases {
		tool := tools.NewSupportTickets(&testsupport.FakeTickets{Outcome: tc.outcome})
		result, err := tool.Invoke(context.Background(), "c1", args(t, map[string]string{
			"summary": "a problem", "category": "returns",
		}))
		if err != nil {
			t.Fatalf("%s reached the model as an error: %v", tc.outcome, err)
		}
		if !strings.Contains(result.Content, tc.wantKey) {
			t.Errorf("%s produced %s, want it to contain %s", tc.outcome, result.Content, tc.wantKey)
		}
		if result.Outcome != string(tc.outcome) {
			t.Errorf("metric outcome is %q, want %q", result.Outcome, tc.outcome)
		}
	}
}

// A storage failure is the one thing the tool does return as an error, so the service
// replaces it with a single fixed sentence rather than putting a database error in front
// of a customer.
func TestAStorageFailureIsTheOnlyErrorTheToolReturns(t *testing.T) {
	tool := tools.NewSupportTickets(&testsupport.FakeTickets{Err: errors.New("connection refused")})
	result, err := tool.Invoke(context.Background(), "c1", args(t, map[string]string{
		"summary": "a problem", "category": "returns",
	}))
	if err == nil {
		t.Fatal("a storage failure must surface as an error, not as a plausible reply")
	}
	if strings.Contains(result.Content, "connection refused") {
		t.Errorf("the database error text reached the model: %q", result.Content)
	}
}

// Tool descriptions are prompt, not documentation: they are the entire basis on which
// the model decides whether to call a tool instead of answering from retrieved text. A
// rename or a dropped description changes behaviour without changing anything else a
// test would notice.
func TestToolDefinitionsSayWhatTheToolIsNotFor(t *testing.T) {
	cases := []struct {
		tool      tools.Tool
		name      string
		required  []string
		mustSay   []string
		mustNotBe string
	}{
		{
			tool:     tools.NewOrderLookup(),
			name:     "lookup_order_status",
			required: []string{"orderNumber"},
			mustSay: []string{
				"Does not modify the order",
				"asked to check it rather than told the order does not exist",
			},
		},
		{
			tool:     tools.NewSupportTickets(&testsupport.FakeTickets{}),
			name:     "create_support_ticket",
			required: []string{"summary", "category"},
			mustSay: []string{
				"Do not use it to answer questions that documentation already covers",
				"do not paste the whole conversation",
			},
		},
	}

	for _, tc := range cases {
		d := tc.tool.Definition()
		if d.Name != tc.name {
			t.Errorf("tool name is %q, want %q -- the model calls it by this name", d.Name, tc.name)
		}
		for _, phrase := range tc.mustSay {
			if !strings.Contains(d.Description, phrase) {
				t.Errorf("%s: description no longer says %q", d.Name, phrase)
			}
		}
		if len(d.Required) != len(tc.required) {
			t.Errorf("%s: required is %v, want %v", d.Name, d.Required, tc.required)
		}
		for _, name := range tc.required {
			if _, ok := d.Properties[name]; !ok {
				t.Errorf("%s: required parameter %q is not in the schema", d.Name, name)
			}
		}
		for name, spec := range d.Properties {
			property, _ := spec.(map[string]any)
			if desc, _ := property["description"].(string); desc == "" {
				t.Errorf("%s: parameter %q has no description; the model reads these",
					d.Name, name)
			}
		}
	}
}
