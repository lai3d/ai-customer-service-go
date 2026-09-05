package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic is the default provider. Claude has no embedding API, which is why
// retrieval runs a local model; for chat it is the reference implementation here.
type Anthropic struct {
	client    anthropic.Client
	model     string
	maxTokens int
}

type Options struct {
	APIKey  string
	Model   string
	BaseURL string

	MaxTokens int

	// Interactive settings, not batch ones. The SDK retries twice by default with its
	// own backoff; three attempts total caps the added wait at a few seconds. A
	// provider that is still failing after that is better reported than waited on.
	MaxAttempts int
	// The SDK's default request timeout is ten minutes, which is a stall, not a
	// slowness, for an interactive turn.
	RequestTimeout time.Duration
}

func NewAnthropic(opts Options) *Anthropic {
	clientOpts := []option.RequestOption{
		option.WithAPIKey(opts.APIKey),
		option.WithMaxRetries(max(opts.MaxAttempts-1, 0)),
		option.WithRequestTimeout(opts.RequestTimeout),
	}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	return &Anthropic{
		client:    anthropic.NewClient(clientOpts...),
		model:     opts.Model,
		maxTokens: opts.MaxTokens,
	}
}

func (a *Anthropic) Provider() string { return "anthropic" }
func (a *Anthropic) Model() string    { return a.model }

func (a *Anthropic) Stream(ctx context.Context, req Request, onText func(string) error) (Result, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: int64(a.maxTokens),
		Messages:  toAnthropicMessages(req.Messages),
		// No Temperature, TopP or TopK. Claude Opus 5 rejects all three with HTTP 400,
		// and no configuration seeds one here -- the field simply is not set. Spring AI
		// needed a BeanPostProcessor to strip a value its own properties class had put
		// there in a field initialiser.
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	if len(req.Tools) > 0 {
		params.Tools = toAnthropicTools(req.Tools)
	}

	stream := a.client.Messages.NewStreaming(ctx, params)
	message := anthropic.Message{}

	// Counted so the wire's behaviour is a measurement rather than a belief: Anthropic
	// reports usage across several frames per call (input at message_start, a growing
	// output count on every message_delta), and the SDK's accumulator resolves them --
	// cumulative totals overwrite rather than add. See docs/reliability.md.
	usageFrames := 0

	// A failure does not return early, because by the time one happens the provider has
	// usually already told us what it billed. Anthropic sends the input count at
	// message_start, before a single token of the answer, so a stream that dies
	// half-way through -- most often because the customer closed the tab -- has spent
	// real money that the caller is willing to record. Returning a zero Result here
	// would throw it away one layer below the comment that promises otherwise.
	var streamErr error

	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			streamErr = fmt.Errorf("accumulate stream event: %w", err)
			break
		}
		if event.Usage.OutputTokens > 0 || event.Message.Usage.InputTokens > 0 {
			usageFrames++
		}
		// Only visible text is forwarded. Thinking blocks stream as their own delta
		// type and are not the customer's business; with the default display they
		// carry no text anyway.
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
				if err := onText(text.Text); err != nil {
					streamErr = err
					break
				}
			}
		}
	}
	if streamErr == nil {
		streamErr = classify(stream.Err())
	}

	slog.Debug("model call finished",
		"provider", "anthropic", "usage_frames", usageFrames,
		"input_tokens", message.Usage.InputTokens,
		"output_tokens", message.Usage.OutputTokens,
		"error", streamErr)

	result := Result{
		StopReason: string(message.StopReason),
		Model:      string(message.Model),
		// Accumulate has already resolved usage for this call: Anthropic sends
		// cumulative whole-message totals, so the last frame overwrites rather than
		// adds. One call in, one call's usage out.
		Usage: Usage{
			InputTokens:  message.Usage.InputTokens,
			OutputTokens: message.Usage.OutputTokens,
		},
	}
	// Only for a call that finished. Native exists so a tool round can echo an assistant
	// turn back unchanged, and a half-streamed one is not something to send anywhere.
	if streamErr == nil && len(message.Content) > 0 {
		result.Native = message.ToParam()
	}
	for _, block := range message.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			result.Text += variant.Text
		case anthropic.ToolUseBlock:
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        variant.ID,
				Name:      variant.Name,
				Arguments: []byte(variant.JSON.Input.Raw()),
			})
		}
	}
	return result, streamErr
}

func toAnthropicMessages(messages []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for _, m := range messages {
		// An assistant turn that is being continued goes back exactly as it arrived.
		// Rebuilding it from Text would drop the thinking blocks Claude expects to see
		// again, and the tool_use ids would have to be invented.
		if native, ok := m.Native.(anthropic.MessageParam); ok {
			out = append(out, native)
			continue
		}
		switch m.Role {
		case RoleAssistant:
			if m.Text != "" {
				out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
			}
		default:
			var blocks []anthropic.ContentBlockParamUnion
			for _, r := range m.ToolResults {
				blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
			}
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Text))
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		}
	}
	return out
}

func toAnthropicTools(tools []ToolDefinition) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		properties, _ := t.Schema["properties"].(map[string]any)
		schema := anthropic.ToolInputSchemaParam{Properties: properties}
		if required, ok := t.Schema["required"].([]string); ok {
			schema.Required = required
		}
		tool := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out
}

// Error separates the failures a client should retry from the ones it should not.
// A customer told "try again in a moment" when the credentials are wrong is being
// misled, and one told "this cannot work" when the provider is briefly overloaded is
// being turned away needlessly.
type Error struct {
	StatusCode int
	Retryable  bool
	Err        error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &Error{
			StatusCode: apiErr.StatusCode,
			Retryable:  retryable(apiErr.StatusCode),
			Err:        err,
		}
	}
	// A transport failure that got past the SDK's own retries. Worth another attempt
	// from the customer's side, unlike a rejected request.
	return &Error{StatusCode: 0, Retryable: true, Err: err}
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529: // Anthropic's "overloaded"
		return true
	}
	return false
}
