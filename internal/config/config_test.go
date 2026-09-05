package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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

// A Postgres password may legally contain any of / ? # @ : -- and generated ones often
// do. Formatting a DSN with fmt.Sprintf turns those into URL syntax: `test/a#b%` used to
// fail with "invalid port after host" before a connection was ever attempted, so a
// perfectly good password made the service refuse to start.
func TestCredentialsWithURLSyntaxSurviveIntoTheDSN(t *testing.T) {
	for _, password := range []string{
		"test/a#b%25", "p@ssw:rd", "a?b=c&d", "plain", "with spaces",
	} {
		p := config.Postgres{
			Host: "db", Port: 5432, Database: "csagent",
			User: "csagent", Password: password,
		}
		cfg, err := pgx.ParseConfig(p.URL())
		if err != nil {
			t.Errorf("password %q produced an unparseable DSN: %v", password, err)
			continue
		}
		if cfg.Password != password {
			t.Errorf("password %q came back as %q", password, cfg.Password)
		}
		if cfg.Database != "csagent" || cfg.Host != "db" || cfg.Port != 5432 {
			t.Errorf("password %q corrupted the rest of the DSN: %s:%d/%s",
				password, cfg.Host, cfg.Port, cfg.Database)
		}
	}
}

// The source default, the container, the healthcheck and every document that names a port
// have to agree. They did not: the default was 8080 and only the Dockerfile's explicit
// override hid it, so `make run` listened somewhere the README did not point at -- and on
// the port the Java implementation of this system uses.
func TestTheDefaultPortMatchesWhatTheDocumentsPromise(t *testing.T) {
	t.Setenv("CHAT_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("HTTP_ADDR", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8081" {
		t.Errorf("default listen address is %q, want :8081", cfg.HTTPAddr)
	}

	root := repoRoot(t)
	for _, name := range []string{"README.md", "README.zh.md", "Dockerfile", ".env.example"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "localhost:8080") {
			t.Errorf("%s still points at 8080", name)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root")
	return ""
}
