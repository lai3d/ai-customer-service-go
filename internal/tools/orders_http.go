package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPOrders reads orders from a real order service over HTTP.
//
// **Nothing here has been run against a real order system.** There is none to run it
// against, and inventing one would produce a document that reads as evidence and is not.
// What has been exercised, against an httptest server in orders_http_test.go, is every
// way this adapter can fail: a 200, a 404, a 500 with its retry, a body that is not JSON,
// a body with nothing in it, a 401, and a server that never answers. The wire contract
// below is this repository's guess and is the first thing that will be wrong.
//
// The contract:
//
//	GET {base}/orders/{orderNumber}
//	Authorization: Bearer {token}      (omitted when no token is configured)
//	Accept: application/json
//
//	200  a JSON object with the fields of Order
//	404  no such order
//	401, 403  the credential is wrong
//	429, 5xx  the service cannot serve this now -- retried once within the budget
//	anything else  a contract disagreement; not retried
//
// Two properties matter more than the shape and are not negotiable when the shape
// changes.
//
// **The budget covers the whole lookup, retries included.** A tool call happens inside a
// turn a customer is waiting on, so the interesting number is not how long one attempt
// takes, it is how long the model waits before it can say anything at all. An adapter
// with a 3s per-attempt timeout and two attempts is a 6s adapter, and it will be
// described in every document as a 3s one. config.Load refuses a timeout that is not
// shorter than the model request timeout.
//
// **A retry is bounded and only for failures that a moment might fix.** A 404 is not
// retried -- the order still will not exist -- and neither is a timeout, because the
// budget that would have paid for the second attempt is precisely what the first one
// spent. That leaves 5xx, 429 and a connection that could not be made: the failures where
// the second attempt costs milliseconds and sometimes works.
type HTTPOrders struct {
	baseURL  string
	token    string
	timeout  time.Duration
	attempts int
	// retryGap is a field so a test can drive the retry without waiting. Not
	// configuration: nobody needs to tune it, and every knob is a thing that can be set
	// wrong.
	retryGap time.Duration
	client   *http.Client
}

// HTTPOptions is what an HTTPOrders needs. Every one of them is validated, because the
// alternative is a service that starts, reports itself ready, and fails only once a
// customer asks about an order.
type HTTPOptions struct {
	BaseURL string
	// Token is a bearer credential. It is never logged, never put in an error, and
	// never in the result the model reads.
	Token string
	// Timeout bounds the whole lookup, retries and connection setup included.
	Timeout time.Duration
	// Attempts is the total number of tries, not the number of retries. 1 disables
	// retrying.
	Attempts int
}

// maxOrderBody bounds what an order service can put into the model's context.
//
// The response is decoded into Order and only those fields travel, but Summary is free
// text from another system and a turn is billed by the token. An upstream bug that
// streams megabytes should cost a decode error, not a bill.
const maxOrderBody = 64 << 10

func NewHTTPOrders(opts HTTPOptions) (*HTTPOrders, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, errors.New("an order service needs a base URL")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("order service URL %q does not parse: %w", base, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("order service URL %q must be http or https", base)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("order service URL %q has no host", base)
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("order service timeout must be positive, got %s", opts.Timeout)
	}
	if opts.Attempts < 1 {
		return nil, fmt.Errorf("order service attempts must be at least 1, got %d", opts.Attempts)
	}
	return &HTTPOrders{
		baseURL:  base,
		token:    strings.TrimSpace(opts.Token),
		timeout:  opts.Timeout,
		attempts: opts.Attempts,
		retryGap: 100 * time.Millisecond,
		// The context deadline below is what actually bounds a lookup. This is the
		// belt-and-braces copy: an http.Client with no Timeout is the same hazard Spring
		// Boot shipped with, and a future caller that forgets the context should still
		// not be able to hang a turn.
		client: &http.Client{Timeout: opts.Timeout},
	}, nil
}

// Fixture is deliberately absent from this type: it is not one, and IsFixture reports
// false for anything that does not claim otherwise.

func (h *HTTPOrders) Lookup(ctx context.Context, orderNumber string) (Order, Outcome, error) {
	// The budget, once, for everything below. If the caller's context expires sooner --
	// the customer closed the tab -- that wins, which is what WithTimeout does.
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	var lastErr error
	for attempt := 1; ; attempt++ {
		order, outcome, err := h.attempt(ctx, orderNumber)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
		}
		if outcome != OutcomeUnavailable || attempt >= h.attempts {
			return order, outcome, lastErr
		}
		// A gap the budget has to be able to afford. Sleeping past the deadline and then
		// discovering it would turn a retry into a slower way of returning the same
		// failure.
		timer := time.NewTimer(h.retryGap)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Order{}, OutcomeTimedOut, fmt.Errorf(
				"%w (budget %s spent after %d attempt(s)); last failure: %w",
				ctx.Err(), h.timeout, attempt, lastErr)
		}
	}
}

// attempt is one request. It returns an outcome for every path, so the loop above never
// has to interpret an error.
func (h *HTTPOrders) attempt(ctx context.Context, orderNumber string) (Order, Outcome, error) {
	// PathEscape, not concatenation. The order number is written by the model from what
	// the customer typed, so "../../admin" or a query string in it is a request this
	// service would otherwise make on a stranger's behalf.
	endpoint := h.baseURL + "/orders/" + url.PathEscape(orderNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Order{}, OutcomeUnreadable, fmt.Errorf("build the order request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		// A deadline and a refused connection are the same *return* from net/http and
		// two different things to everyone downstream: one is a slow order system, the
		// other is one that is not there. ctx.Err() is the reliable discriminator --
		// url.Error's Timeout() is also true for a dial timeout that the budget did not
		// cause.
		if ctx.Err() != nil {
			return Order{}, OutcomeTimedOut, fmt.Errorf("no answer within the %s budget: %s",
				h.timeout, scrub(err, h.token))
		}
		return Order{}, OutcomeUnavailable, errors.New(scrub(err, h.token))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return h.decode(resp, orderNumber)
	case resp.StatusCode == http.StatusNotFound:
		drain(resp.Body)
		return Order{}, OutcomeNotFound, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		drain(resp.Body)
		// Loud, because this one is silent everywhere else: a rotated token makes every
		// order lookup fail forever while the service stays healthy, ready and green.
		return Order{}, OutcomeDenied, fmt.Errorf(
			"the order service rejected this service's credentials with HTTP %d; "+
				"check ORDER_SERVICE_TOKEN", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		drain(resp.Body)
		return Order{}, OutcomeUnavailable, fmt.Errorf(
			"the order service answered HTTP %d", resp.StatusCode)
	default:
		drain(resp.Body)
		return Order{}, OutcomeUnreadable, fmt.Errorf(
			"the order service answered HTTP %d, which is not in the contract this "+
				"adapter implements", resp.StatusCode)
	}
}

func (h *HTTPOrders) decode(resp *http.Response, orderNumber string) (Order, Outcome, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOrderBody+1))
	if err != nil {
		// A body that stops half way is not a readable answer, and it is not an outage
		// either: the request got a 200.
		return Order{}, OutcomeUnreadable, fmt.Errorf("read the order service response: %s",
			scrub(err, h.token))
	}
	if len(body) > maxOrderBody {
		return Order{}, OutcomeUnreadable, fmt.Errorf(
			"the order service returned more than %d bytes for one order", maxOrderBody)
	}

	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		// The error carries the decoder's complaint and never the body: a 200 from the
		// wrong host is somebody's HTML login page, and that is not something to copy
		// into this service's log.
		return Order{}, OutcomeUnreadable, fmt.Errorf(
			"the order service answered 200 with %d bytes that are not an order: %w",
			len(body), err)
	}

	// A 200 carrying `{}` is the failure this catches: json.Unmarshal is perfectly happy
	// with it, and without this the model would be handed found:true and an order with no
	// status and would describe it to a customer.
	order.Status = OrderStatus(strings.ToUpper(strings.TrimSpace(string(order.Status))))
	if order.Status == "" {
		return Order{}, OutcomeUnreadable, errors.New(
			"the order service answered 200 with no status")
	}
	// Statuses outside the five this repository knows are passed through rather than
	// rejected. A real order system has states nobody here has heard of, and the model
	// reads the value as text; refusing them would turn a working lookup into an outage
	// the first time somebody adds AWAITING_PAYMENT.
	if strings.TrimSpace(order.OrderNumber) == "" {
		order.OrderNumber = orderNumber
	}
	return order, OutcomeFound, nil
}

// drain reads and discards what is left so the connection can be reused. Bounded,
// because the reason for reading it is keep-alive and not curiosity.
func drain(body io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10)) }

// scrub keeps the credential out of a message that is going to a log.
//
// net/http puts the request URL into every transport error, and a service configured with
// a token in the URL -- which somebody will do, because plenty of internal services want
// one there -- would otherwise write it into the log on every failure. It costs one
// string replacement to make that impossible.
func scrub(err error, token string) string {
	text := err.Error()
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}
