package llm

import (
	"fmt"

	"github.com/lai3d/ai-customer-service-go/internal/config"
)

// New builds the configured provider, and the failover pair if a second one is set.
//
// The provider is configuration, not code: everything around the model -- memory,
// retrieval, both tools, streaming, metrics -- is written against Client. A failover is
// a Client too, which is why nothing above this line has to know whether there is one.
func New(cfg config.Chat) (Client, error) {
	primary, err := client(cfg.Provider, Options{
		APIKey:         cfg.APIKey,
		Model:          cfg.Model,
		BaseURL:        cfg.BaseURL,
		MaxTokens:      cfg.MaxTokens,
		MaxAttempts:    cfg.RetryMaxAttempts,
		RequestTimeout: cfg.RequestTimeout,
	})
	if err != nil {
		return nil, err
	}
	if cfg.FallbackProvider == "" {
		return primary, nil
	}
	// The same retry and timeout settings, deliberately. They are interactive limits --
	// how long a customer waits -- and a customer who has already waited out the primary
	// has less patience left, not more.
	secondary, err := client(cfg.FallbackProvider, Options{
		APIKey:         cfg.FallbackAPIKey,
		Model:          cfg.FallbackModel,
		BaseURL:        cfg.FallbackBaseURL,
		MaxTokens:      cfg.MaxTokens,
		MaxAttempts:    cfg.RetryMaxAttempts,
		RequestTimeout: cfg.RequestTimeout,
	})
	if err != nil {
		return nil, err
	}
	return NewFailover(primary, secondary), nil
}

// MeterFailover attaches the failover counters to whatever New built.
//
// Separate from New so that New keeps taking configuration and nothing else, and so the
// eval harness -- which builds a client with no metrics registry at all -- is unaffected.
// A client with no fallback ignores it.
func MeterFailover(c Client, meter FailoverMeter) Client {
	if f, ok := c.(*Failover); ok {
		return f.Meter(meter)
	}
	return c
}

func client(provider string, opts Options) (Client, error) {
	switch provider {
	case "anthropic":
		return NewAnthropic(opts), nil
	case "openai":
		return NewOpenAI(opts), nil
	case "xai":
		return NewXAI(opts), nil
	default:
		// Unreachable: config.Load rejects an unknown provider by name, primary or
		// fallback. Here so that adding a case to one switch and not the other is a
		// compile-time nil rather than a runtime surprise.
		return nil, fmt.Errorf("provider %q has no client", provider)
	}
}
