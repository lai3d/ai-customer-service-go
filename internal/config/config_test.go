package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/config"
)

// A service that starts without credentials, reports itself healthy, is marked ready by
// Kubernetes and then 401s every customer request is the worse failure. This has to stop
// the process.
func TestStartupFailsWhenTheSelectedProviderHasNoKey(t *testing.T) {
	t.Setenv("CHAT_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("configuration loaded with no API key")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error is %q; it should name the variable that is missing", err)
	}
}

func TestAnUnknownProviderIsRejectedByName(t *testing.T) {
	t.Setenv("CHAT_PROVIDER", "mistral")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "mistral") {
		t.Errorf("error is %v, want one naming the unknown provider and the valid ones", err)
	}
}

// Only the selected provider's key is required. Demanding all four would make running
// the service with one key impossible.
func TestOnlyTheSelectedProvidersKeyIsRequired(t *testing.T) {
	t.Setenv("CHAT_PROVIDER", "xai")
	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("xai with only XAI_API_KEY set failed: %v", err)
	}
	if cfg.Chat.Model != "grok-4.6" {
		t.Errorf("default xai model is %q", cfg.Chat.Model)
	}
	if !strings.Contains(cfg.Chat.BaseURL, "x.ai") {
		t.Errorf("xai base url is %q; borrowing OpenAI's slot is the thing this avoids",
			cfg.Chat.BaseURL)
	}
}

// Sampling parameters are absent on purpose, for every provider: Claude Opus 5 returns
// HTTP 400 for temperature, top_p or top_k, and GPT-5 accepts only its own default.
// There is no field to set one by accident, and this test says so out loud.
func TestNoSamplingParameterIsConfigurable(t *testing.T) {
	t.Setenv("CHAT_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Reflection would be indirect; the point is that config.Chat has no such field,
	// which is a compile-time property. This asserts the intent survives a refactor
	// that adds one.
	if got := describeChatFields(cfg.Chat); strings.Contains(got, "Temperature") ||
		strings.Contains(got, "TopP") || strings.Contains(got, "TopK") {
		t.Errorf("a sampling parameter has appeared in the chat configuration: %s", got)
	}
}

func describeChatFields(c config.Chat) string {
	t := reflect.TypeOf(c)
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		names = append(names, t.Field(i).Name)
	}
	return strings.Join(names, ",")
}
