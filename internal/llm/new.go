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
		// Unreachable: config.Load rejects an unknown provider by name. Here so that
		// adding a case to one switch and not the other is a compile-time nil rather
		// than a runtime surprise.
		return nil, fmt.Errorf("provider %q has no client", cfg.Provider)
	}
}
