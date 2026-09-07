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
