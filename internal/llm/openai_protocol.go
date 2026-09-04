package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// openAIProtocol speaks OpenAI's chat completions wire protocol.
//
// Two providers use it, and they are two providers rather than one. xAI speaks this
// protocol, so reimplementing streaming, tool calling and retry for Grok would be pure
// cost -- but selecting "openai", putting an xAI key in OPENAI_API_KEY and overriding
// the base URL works and lies: the configuration then says OpenAI everywhere while
// talking to xAI, the two cannot be configured side by side, and whoever reads the
// deployment later has to know the trick. The provider name, credentials, base URL and
// model are xAI's own; only the client is shared.
//
// The one thing this does not paper over: xAI's compatibility is xAI's to maintain. If
// they diverge from this protocol, this breaks.
type openAIProtocol struct {
	client    openai.Client
	provider  string
	model     string
	maxTokens int
}

func NewOpenAI(opts Options) Client {
	return newOpenAIProtocol("openai", opts)
}

func NewXAI(opts Options) Client {
	return newOpenAIProtocol("xai", opts)
}

func newOpenAIProtocol(provider string, opts Options) *openAIProtocol {
	clientOpts := []option.RequestOption{
		option.WithAPIKey(opts.APIKey),
		option.WithMaxRetries(max(opts.MaxAttempts-1, 0)),
		option.WithRequestTimeout(opts.RequestTimeout),
	}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	return &openAIProtocol{
		client:    openai.NewClient(clientOpts...),
		provider:  provider,
		model:     opts.Model,
		maxTokens: opts.MaxTokens,
	}
}

func (c *openAIProtocol) Provider() string { return c.provider }
func (c *openAIProtocol) Model() string    { return c.model }

func (c *openAIProtocol) Stream(ctx context.Context, req Request, onText func(string) error) (Result, error) {
	params := openai.ChatCompletionNewParams{
		Model:               c.model,
		Messages:            toOpenAIMessages(req),
		MaxCompletionTokens: openai.Int(int64(c.maxTokens)),
		// Without this the response carries no usage at all, and the failure is
		// silent: a conversation budget built on those numbers never triggers and the
		// cost meters stay at zero while real money is spent. Anthropic sends usage
		// unasked, which is exactly how this stays hidden until someone looks.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
		// No Temperature, TopP or presence/frequency penalties. GPT-5 rejects a
		// temperature other than its own default outright.
	}
	if len(req.Tools) > 0 {
		params.Tools = toOpenAITools(req.Tools)
	}

	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	accumulator := openai.ChatCompletionAccumulator{}

	// Counted so the wire's behaviour is observable rather than assumed: on this
	// protocol the same cumulative usage is attached to many chunks, which is what made
	// summing frames wrong in an implementation that could not see call boundaries.
	usageFrames := 0

	for stream.Next() {
		chunk := stream.Current()
		accumulator.AddChunk(chunk)
		if chunk.Usage.TotalTokens > 0 {
			usageFrames++
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := onText(choice.Delta.Content); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Result{}, classifyOpenAI(err)
	}

	slog.Debug("model call finished",
		"provider", c.provider, "usage_frames", usageFrames,
		"input_tokens", accumulator.Usage.PromptTokens,
		"output_tokens", accumulator.Usage.CompletionTokens)

	result := Result{
		Model: accumulator.Model,
		// One call in, one call's usage out. The accumulator holds the final
		// cumulative totals for this call; the repetition across chunks is not
		// something the caller ever sees.
		Usage: Usage{
			InputTokens:  accumulator.Usage.PromptTokens,
			OutputTokens: accumulator.Usage.CompletionTokens,
		},
		Native: accumulator.ChatCompletion,
	}
	if len(accumulator.Choices) > 0 {
		choice := accumulator.Choices[0]
		result.Text = choice.Message.Content
		result.StopReason = choice.FinishReason
		for _, call := range choice.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments),
			})
		}
	}
	if result.Model == "" {
		result.Model = c.model
	}
	return result, nil
}

func toOpenAIMessages(req Request) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			if len(m.ToolCalls) > 0 {
				assistant := openai.ChatCompletionAssistantMessageParam{}
				if m.Text != "" {
					assistant.Content.OfString = openai.String(m.Text)
				}
				for _, call := range m.ToolCalls {
					assistant.ToolCalls = append(assistant.ToolCalls,
						openai.ChatCompletionMessageToolCallParam{
							ID: call.ID,
							Function: openai.ChatCompletionMessageToolCallFunctionParam{
								Name:      call.Name,
								Arguments: string(call.Arguments),
							},
						})
				}
				out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
				continue
			}
			if m.Text != "" {
				out = append(out, openai.AssistantMessage(m.Text))
			}
		default:
			// Tool results are their own role on this protocol, one message each,
			// rather than blocks inside a user message as on Anthropic's.
			for _, r := range m.ToolResults {
				out = append(out, openai.ToolMessage(r.Content, r.CallID))
			}
			if m.Text != "" {
				out = append(out, openai.UserMessage(m.Text))
			}
		}
	}
	return out
}

func toOpenAITools(tools []ToolDefinition) []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  shared.FunctionParameters(t.Schema),
			},
		})
	}
	return out
}

func classifyOpenAI(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &Error{
			StatusCode: apiErr.StatusCode,
			Retryable:  retryable(apiErr.StatusCode),
			Err:        err,
		}
	}
	return &Error{StatusCode: 0, Retryable: true, Err: fmt.Errorf("%w", err)}
}
