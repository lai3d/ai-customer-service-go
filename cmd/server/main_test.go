package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

// The start-up line is the whole mechanism of production-readiness item 4, so it is
// checked rather than asserted in a document.
//
// A service answering order questions from a five-order fixture is indistinguishable from
// one talking to the order system: it returns orders, the meters read `found`, and the
// model writes a fluent answer. Nothing downstream can tell. The only thing that can
// reveal it is the service saying so, out loud, every time it starts -- which makes the
// log line load-bearing code and not decoration.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

func TestAFixtureOrderSourceAnnouncesItselfAsOne(t *testing.T) {
	logged := capture(t)

	source, err := orderSource(config.Orders{})
	if err != nil {
		t.Fatal(err)
	}
	if !tools.IsFixture(source) {
		t.Error("no ORDER_SERVICE_URL did not produce the fixture source")
	}

	line := logged.String()
	if !strings.Contains(line, "level=WARN") {
		// INFO would be true and useless: this is the line that has to survive being
		// skimmed by somebody who believes the order system is wired up.
		t.Errorf("the fixture is announced below WARN: %s", line)
	}
	for _, phrase := range []string{"ORDER_SERVICE_URL", "fixture", "ORD-10042", "not the order system"} {
		if !strings.Contains(line, phrase) {
			t.Errorf("the start-up line does not say %q: %s", phrase, line)
		}
	}
}

func TestAConfiguredOrderServiceIsAnnouncedWithoutItsCredential(t *testing.T) {
	logged := capture(t)

	source, err := orderSource(config.Orders{
		BaseURL: "https://orders.internal/v2", Token: "s3cret-order-token",
		Timeout: 3 * time.Second, Attempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if tools.IsFixture(source) {
		t.Error("a configured order service produced the fixture source")
	}

	line := logged.String()
	if !strings.Contains(line, "https://orders.internal/v2") {
		t.Errorf("the start-up line does not name the order service: %s", line)
	}
	// Which is the question anyone reading this line actually has, and it is answerable
	// without printing the answer.
	if !strings.Contains(line, "authenticated=true") {
		t.Errorf("the start-up line does not say whether a credential is configured: %s", line)
	}
	if strings.Contains(line, "s3cret-order-token") {
		t.Errorf("the credential is in the start-up log: %s", line)
	}
}

// A URL that cannot be requested stops the process. The failure to avoid is a pod that
// starts, reports ready, and tells every customer that their order does not exist.
func TestAnUnbuildableOrderSourceStopsStartup(t *testing.T) {
	capture(t)
	if _, err := orderSource(config.Orders{BaseURL: "orders.internal", Timeout: time.Second, Attempts: 1}); err == nil {
		t.Error("a base URL with no scheme was accepted")
	}
}

// Whether this deployment survives a provider outage is invisible from the outside. It
// cannot be read off /healthz, it does not change any metric until it matters, and nobody
// discovers it from configuration -- they discover it during the outage. So it is said at
// every start-up, and the line is checked here for the same reason the order source's is.
func TestASingleProviderDeploymentSaysThatItIsOne(t *testing.T) {
	logged := capture(t)

	announceProviders(config.Chat{Provider: "anthropic", Model: "claude-opus-5"})

	line := logged.String()
	for _, phrase := range []string{"CHAT_FALLBACK_PROVIDER", "outage of this service", "anthropic"} {
		if !strings.Contains(line, phrase) {
			t.Errorf("the start-up line does not say %q: %s", phrase, line)
		}
	}
	// "replica count" is in the line because scaling out is the thing people reach for
	// when told a dependency is a single point of failure, and it does not help here:
	// every replica calls the same API.
	if !strings.Contains(line, "replica count") {
		t.Errorf("the line does not say that replicas do not help: %s", line)
	}
}

// The other half, and the part that has to name both providers: an operator reading this
// during an incident wants to know what the traffic can move to, and a key is not part of
// the answer.
func TestAConfiguredFallbackIsAnnouncedWithoutAnyCredential(t *testing.T) {
	logged := capture(t)

	announceProviders(config.Chat{
		Provider: "anthropic", Model: "claude-opus-5", APIKey: "sk-primary-secret",
		FallbackProvider: "openai", FallbackModel: "gpt-5", FallbackAPIKey: "sk-fallback-secret",
	})

	line := logged.String()
	for _, phrase := range []string{"anthropic", "claude-opus-5", "openai", "gpt-5"} {
		if !strings.Contains(line, phrase) {
			t.Errorf("the start-up line does not name %q: %s", phrase, line)
		}
	}
	for _, secret := range []string{"sk-primary-secret", "sk-fallback-secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("an API key reached the log: %s", line)
		}
	}
	// The voice question is not a footnote in a document somebody has to find. Turning
	// this on changes what customers read during an outage.
	if !strings.Contains(line, "do not write the same answer") {
		t.Errorf("the line does not say the two providers differ: %s", line)
	}
}
