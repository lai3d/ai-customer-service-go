package llm

import (
	"fmt"

	"github.com/lai3d/ai-customer-service-go/internal/config"
)

// New builds the configured provider.
//
// The provider is configuration, not code: everything around the model -- memory,
// retrieval, both tools, streaming, metrics -- is written against Client.
func New(cfg config.Chat) (Client, error) {
	opts := Options{
		APIKey:         cfg.APIKey,
		Model:          cfg.Model,
		BaseURL:        cfg.BaseURL,
		MaxTokens:      cfg.MaxTokens,
		MaxAttempts:    cfg.RetryMaxAttempts,
		RequestTimeout: cfg.RequestTimeout,
	}
	switch cfg.Provider {
	case "anthropic":
		return NewAnthropic(opts), nil
	case "openai":
		return NewOpenAI(opts), nil
	case "xai":
		return NewXAI(opts), nil
	default:
		return nil, fmt.Errorf("provider %q is configured but not implemented yet", cfg.Provider)
	}
}
