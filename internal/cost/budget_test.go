package cost_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/cost"
)

func TestAConversationIsRefusedOnceItHasSpentItsBudget(t *testing.T) {
	budget := cost.NewBudget(1000, 10)

	if err := budget.Check("c1"); err != nil {
		t.Fatalf("a fresh conversation was refused: %v", err)
	}
	budget.Record("c1", 999)
	if err := budget.Check("c1"); err != nil {
		t.Fatalf("refused below the limit: %v", err)
	}
	budget.Record("c1", 1)

	var exceeded *cost.ErrExceeded
	if err := budget.Check("c1"); !errors.As(err, &exceeded) {
		t.Fatalf("check returned %v, want a budget error", err)
	}
	if exceeded.Spent != 1000 || exceeded.Limit != 1000 {
		t.Errorf("error reports spent=%d limit=%d", exceeded.Spent, exceeded.Limit)
	}
	// Other conversations are unaffected: the cap is per conversation.
	if err := budget.Check("c2"); err != nil {
		t.Errorf("an unrelated conversation was refused: %v", err)
	}
}

func TestAZeroBudgetDisablesTheCapButStillTracksSpend(t *testing.T) {
	budget := cost.NewBudget(0, 10)
	budget.Record("c1", 1_000_000)
	if err := budget.Check("c1"); err != nil {
		t.Errorf("a disabled cap refused a conversation: %v", err)
	}
	if got := budget.Spent("c1"); got != 1_000_000 {
		t.Errorf("spend is %d, want it tracked even with the cap off", got)
	}
}

// An unbounded map keyed by conversation id is a memory leak with a long fuse: it grows
// with traffic and is noticed weeks later as a slow heap climb.
func TestSpendTrackingIsBounded(t *testing.T) {
	budget := cost.NewBudget(1000, 4)
	for i := range 100 {
		budget.Record(fmt.Sprintf("c%d", i), 10)
	}
	if got := budget.Spent("c0"); got != 0 {
		t.Errorf("the oldest conversation is still tracked with %d tokens", got)
	}
	if got := budget.Spent("c99"); got != 10 {
		t.Errorf("the newest conversation reports %d tokens, want 10", got)
	}
}

// Prices key on the model id the provider reports, not the one requested. Asking for
// "gpt-5" yields "gpt-5-2025-08-07", and the failure of a mismatch is silent: tokens
// keep counting while cost stays at zero.
func TestAnUnpricedModelCountsTokensWithoutInventingACost(t *testing.T) {
	usd, priced := cost.USD("some-model-nobody-priced", 1_000_000, 1_000_000)
	if priced {
		t.Error("reported a price for a model with no entry")
	}
	if usd != 0 {
		t.Errorf("cost is %v, want 0", usd)
	}

	usd, priced = cost.USD("claude-opus-5", 1_000_000, 1_000_000)
	if !priced {
		t.Fatal("claude-opus-5 has no price entry")
	}
	if usd != 30 {
		t.Errorf("a million tokens each way costs %v, want 30 ($5 in + $25 out)", usd)
	}
}
