package llm

import (
	"context"
	"errors"
	"log/slog"
)

// Failover is two providers, one of which is used.
//
// It is a Client wrapping two Clients, so nothing above this package changes: the tool
// loop, the meters, the budget and the spans all still talk to one Client and still see
// one Result per Stream call.
//
// # What "the primary failed" means
//
// Not every error is a reason to spend money at a second provider, and the ones that are
// not are the majority. Four questions are asked in order, and only a yes to all four
// makes a second call:
//
//  1. **Did the customer go away?** A cancelled context or a consumer that refused the
//     text is not a provider failure. Failing over there bills a second provider to
//     answer a browser tab that is already closed. This is the case the whole guard
//     exists for -- it is the most common way a stream ends badly in production, and it
//     is the one that looks most like an outage from inside a client.
//
//  2. **Would the second provider reject it too?** A 400, 401, 403 or 404 is this
//     service's own request being wrong: a malformed tool schema, a missing key, a model
//     id that does not exist. `classify` already draws that line as Error.Retryable, and
//     a non-retryable error stays non-retryable at the fallback. Failing over on a 401
//     would be worse than useless -- it would answer every customer correctly while the
//     primary's credentials were broken, and nothing would say so until the bill arrived
//     from the wrong provider.
//
//  3. **Has the customer already read part of the primary's answer?** Then no. See
//     "Never mid-answer" below.
//
//  4. **Is this the first call of a turn?** A tool round carries an assistant turn the
//     primary produced -- its tool-call ids, and on Anthropic the thinking blocks a
//     continuation must echo back verbatim in Message.Native. That is not portable to
//     another provider's protocol, and a request rebuilt without it is a different
//     request. A turn picks a provider at its first call and keeps it.
//
// What is left is what failover is for: 429 and 5xx after the SDK's own retries are
// exhausted, a transport failure with no HTTP response at all, and a stall -- an attempt
// whose own request timeout expired while the caller's context was still perfectly
// alive.
//
// # Never mid-answer
//
// The two providers do not produce the same answers. Claude and GPT-5 differ in voice,
// in length and in how they hedge, and a customer watching tokens arrive would see one
// writer replaced by another in the middle of a sentence -- and chat.Service, which
// inserts a paragraph break between model calls, would present the seam as if it were
// deliberate. So the switch happens before the first byte reaches the customer or not at
// all: onText is watched, and a single forwarded token settles it.
//
// The cost of that rule is real and worth naming: a provider that dies after streaming
// half an answer produces a failed turn even though a working provider was available.
// That is the trade -- one visibly failed answer beats an answer in two voices, because
// the failure is legible and the seam is not. Whether it should instead restart the turn
// from the beginning at the second provider (losing the text already on screen, spending
// the input tokens twice) is a product decision this does not make; docs/providers.md
// records it as open.
//
// # The usage of an attempt that was thrown away
//
// An abandoned stream has usually already been billed. Anthropic reports the input count
// at message_start, before a single token of the answer, so a primary that dies between
// that frame and the first text delta -- the exact window this failover acts in -- has
// spent real money on a call whose output nobody will ever see.
//
// So the Result handed back carries both attempts' tokens: the caller sums Results, and
// dropping the first one here would lose them one layer below the comment in llm.go that
// promises the caller sees what a call cost. What that costs in exchange is attribution:
// chat.Service meters one Result under one model, so the primary's tokens are counted
// against the fallback's model id and priced at the fallback's rate.
// chat_failover_abandoned_tokens_total exists so that slice is a number somebody can
// look at rather than a silent skew, and chat_model_calls_total plus
// chat_provider_failovers_total is the true count of calls made.
type Failover struct {
	primary   Client
	secondary Client
	meter     FailoverMeter
}

// FailoverMeter is the metrics sink, declared here as an interface so this package does
// not depend on internal/obs. Both methods are label-only: from, to, provider and model
// are bounded sets fixed by configuration, and there is deliberately no way to pass a
// conversation id.
type FailoverMeter interface {
	RecordFailover(from, to, reason string)
	RecordAbandonedAttempt(provider, model string, inputTokens, outputTokens int64)
}

// Why a failover happened. A bounded set, because it is a metric label.
const (
	// The provider answered, and the answer was 429 or 5xx after its own retries.
	reasonUnavailable = "unavailable"
	// No HTTP response at all: DNS, connection refused, a connection cut mid-stream.
	reasonTransport = "transport"
	// The attempt's own request timeout expired while the caller was still waiting.
	// A provider that accepts a request and then says nothing is down in the way that
	// matters most, and it is the failure a status page is slowest to admit.
	reasonStalled = "stalled"
)

func NewFailover(primary, secondary Client) *Failover {
	return &Failover{primary: primary, secondary: secondary}
}

// Meter attaches the counters. Without one the failover still works and is invisible,
// which is why main always calls this.
func (f *Failover) Meter(m FailoverMeter) *Failover {
	f.meter = m
	return f
}

// Provider and Model report the primary's. A span is opened before the call is made, so
// there is nothing else they could honestly report; chat.Service overwrites the model it
// records from Result.Model, which is whichever provider actually answered.
func (f *Failover) Provider() string { return f.primary.Provider() }
func (f *Failover) Model() string    { return f.primary.Model() }

// Secondary is the configured fallback, for the start-up log line.
func (f *Failover) Secondary() Client { return f.secondary }

func (f *Failover) Stream(ctx context.Context, req Request, onText func(string) error) (Result, error) {
	// Watched rather than passed through: whether a token reached the customer is the
	// difference between a failover and an answer in two voices, and it has to be known
	// before the decision, not inferred from Result.Text afterwards. Text accumulated by
	// a client that then failed is not the same as text the consumer was handed.
	forwarded := false
	watched := func(text string) error {
		forwarded = true
		return onText(text)
	}

	result, err := f.primary.Stream(ctx, req, watched)
	reason, failOver := f.decide(ctx, req, err, forwarded)
	if !failOver {
		return result, err
	}

	from, to := f.primary.Provider(), f.secondary.Provider()
	slog.Warn("failing over to the secondary provider",
		"from", from, "to", to, "reason", reason,
		"abandoned_input_tokens", result.Usage.InputTokens,
		"abandoned_output_tokens", result.Usage.OutputTokens,
		"error", err)

	// The model the *provider* reported, not the one asked for, on the same rule as
	// every other meter here. Anthropic names it in message_start, so it survives a
	// stream that died immediately after; a request that never got a response has
	// nothing to report and the configured id is the honest second choice.
	abandonedModel := result.Model
	if abandonedModel == "" {
		abandonedModel = f.primary.Model()
	}
	if f.meter != nil {
		f.meter.RecordFailover(from, to, reason)
		if result.Usage.Total() > 0 {
			f.meter.RecordAbandonedAttempt(from, abandonedModel,
				result.Usage.InputTokens, result.Usage.OutputTokens)
		}
	}

	second, secondErr := f.secondary.Stream(ctx, req, onText)
	// Both attempts, in one Result, because the caller sums Results and this is the only
	// way the first attempt's tokens reach the budget at all.
	second.Usage = second.Usage.Add(result.Usage)
	// Only the second error is returned. Joining them would be more informative and is
	// wrong: chat.Service reads context.Canceled and DeadlineExceeded out of the error to
	// tell a customer who left from a service that broke, and a joined error satisfies
	// errors.Is for either side. A stalled primary would then make every subsequent
	// fallback failure read as "cancelled". The primary's error is logged above instead.
	return second, secondErr
}

// decide answers whether the secondary should be called, and says why for the counter.
//
// The default is no. Every branch that returns true is an argued exception, which is the
// right way round for a function that spends money.
func (f *Failover) decide(ctx context.Context, req Request, err error, forwarded bool) (string, bool) {
	if err == nil || f.secondary == nil {
		return "", false
	}

	// The customer is no longer waiting. Failing over would bill a second provider in
	// full for an answer nobody will read.
	//
	// **This has to come before the error is examined at all**, and that ordering is the
	// whole of it. A turn whose own deadline expired and a provider that accepted the
	// request and went silent both arrive here as context.DeadlineExceeded; nothing in
	// the error distinguishes them, and one of the two is worth a second provider. What
	// separates them is whose clock ran out, which only the caller's context can say.
	// Reading the error first sends a second call to a second provider on every turn that
	// times out -- at the exact moment the service is slowest and least able to afford
	// it.
	//
	// The errors.Is is belt and braces: context.Canceled reaches the bottom of this
	// function and is refused there anyway, because classify passes it through unwrapped
	// and it never becomes an *Error. It is written out because "cancelled means no" is
	// the rule, and leaving it to be inferred from where a default lands is how a rule
	// gets deleted by someone tidying up.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return "", false
	}

	// A token already on screen. Not a judgement about the error at all: whatever went
	// wrong, the customer is mid-answer and a second voice cannot continue it.
	if forwarded {
		return "", false
	}

	// A continuation of a turn the primary started. Its assistant message carries that
	// provider's tool-call ids and, on Anthropic, thinking blocks that must be echoed
	// back unchanged; the second provider would be handed a request that has been
	// silently rewritten.
	if !startOfTurn(req) {
		return "", false
	}

	// The caller's context is alive (checked above), so this deadline is the attempt's
	// own request timeout: the provider took the request and said nothing.
	if errors.Is(err, context.DeadlineExceeded) {
		return reasonStalled, true
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		if !apiErr.Retryable {
			// 400, 401, 403, 404, 422: this service's request or credentials, not the
			// provider's health. The fallback would reject it in the same way, one
			// invoice later.
			return "", false
		}
		if apiErr.StatusCode == 0 {
			return reasonTransport, true
		}
		return reasonUnavailable, true
	}

	// Anything else is the consumer's own error returned from onText -- the only error
	// shape that reaches here without passing through classify. Not a provider failure,
	// and not ours to retry somewhere else.
	return "", false
}

// startOfTurn reports whether this request is the first model call of a turn.
//
// It reads the request rather than being told, because llm.Client has no notion of a
// turn and inventing one -- a counter, a context value -- would be a second source of
// truth about something the messages already say. A turn's first call carries user and
// assistant text only; every later call carries the tool round that produced it.
func startOfTurn(req Request) bool {
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 || m.Native != nil {
			return false
		}
	}
	return true
}
