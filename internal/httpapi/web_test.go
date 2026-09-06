package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
)

func demoPage(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The page renders a small markdown subset -- bold, lists, inline code -- because the
// model emits markdown and the page used to show the asterisks to the customer.
// Rendering it means the page decides what is markup, which is a decision worth pinning.
//
// Every sink below turns a string into markup. The page has none of them: it builds DOM
// nodes and appends strings, which become text nodes. That is what makes model-authored
// content unable to become markup, and it matters more here than in an ordinary chat
// client because the model's input includes retrieved passages -- the injection path the
// system prompt can only ask about, not enforce.
func TestTheDemoPageNeverTurnsAStringIntoMarkup(t *testing.T) {
	page := demoPage(t)

	// Assignment, not the identifier: a comment mentioning innerHTML is fine.
	sinks := map[string]*regexp.Regexp{
		"innerHTML assignment": regexp.MustCompile(`\.innerHTML\s*[+]?=`),
		"outerHTML assignment": regexp.MustCompile(`\.outerHTML\s*[+]?=`),
		"insertAdjacentHTML":   regexp.MustCompile(`insertAdjacentHTML\s*\(`),
		"document.write":       regexp.MustCompile(`document\.write\s*\(`),
		"eval":                 regexp.MustCompile(`[^.\w]eval\s*\(`),
		"Function constructor": regexp.MustCompile(`new\s+Function\s*\(`),
		"srcdoc assignment":    regexp.MustCompile(`\.srcdoc\s*=`),
	}
	for name, pattern := range sinks {
		if loc := pattern.FindStringIndex(page); loc != nil {
			line := 1 + strings.Count(page[:loc[0]], "\n")
			t.Errorf("the page uses %s at line %d; model-authored text would be parsed "+
				"as markup", name, line)
		}
	}
}

// Links are the one markdown construct that is a capability rather than formatting, so
// the renderer does not implement them. If that changes, the href has to be validated.
func TestTheRendererDoesNotBuildLinks(t *testing.T) {
	page := demoPage(t)
	for _, sink := range []string{`el('a'`, `createElement('a')`, `.href =`} {
		if strings.Contains(page, sink) && !strings.Contains(page, "a.href = `http://localhost:16687") {
			t.Errorf("the page builds an anchor with %q; the only anchor should be the "+
				"Jaeger link, whose href the page composes itself", sink)
		}
	}
}

func TestTheDemoPageIsServedAtTheRoot(t *testing.T) {
	server := httptest.NewServer(httpapi.DemoUI())
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type is %q", ct)
	}
}

// Three 404s -- /favicon.ico, /favicon.svg, /apple-touch-icon.png -- were the page's only
// console errors. A console with a permanent error in it teaches its reader to ignore
// the console, which is expensive for a page whose whole purpose is to be inspected.
func TestTheFaviconIsInlineSoTheBrowserAsksForNothing(t *testing.T) {
	page := demoPage(t)
	if !strings.Contains(page, `rel="icon" href="data:image/svg+xml`) {
		t.Error("no inline favicon; the browser will request /favicon.ico and get a 404")
	}
}

// The page must dispatch on the SSE `event:` field, not on a field of the payload.
//
// Chat events carry a `type`; a failure after the response was committed carries a
// problem+json object whose `type` is a URI and never the string "error". Switching on
// the payload meant every post-commit failure -- budget exhausted, provider error,
// retrieval failure -- was silently dropped and the customer saw a partial answer or a
// blank one. The server had been emitting the frame correctly the whole time, and
// TestAFailureAfterTheFirstTokenArrivesAsAnErrorEvent passed, because it asserted on the
// server and the page was never in the loop.
func TestTheDemoPageDispatchesOnTheEventName(t *testing.T) {
	page := demoPage(t)

	if !strings.Contains(page, "startsWith('event: ')") {
		t.Error("the page does not read the SSE event name")
	}
	if regexp.MustCompile(`\bevent\.type\s*===`).MatchString(page) {
		t.Error("the page still switches on a field of the payload; a problem+json " +
			"error object has no chat event type in it")
	}
	for _, name := range []string{"'message'", "'retrieval'", "'tool'", "'usage'", "'error'"} {
		if !strings.Contains(page, "name === "+name) {
			t.Errorf("the page has no branch for the %s event", name)
		}
	}
}

// The server contract the page now relies on: an error frame is named, and its payload is
// a problem object rather than something carrying a chat event type.
func TestTheErrorFrameIsNamedAndCarriesAProblem(t *testing.T) {
	server := serve(t, &fakeTurner{
		events: []chat.Event{{Type: chat.EventMessage, Text: "Thirty "}},
		err:    &cost.ErrExceeded{Spent: 10, Limit: 5},
	})
	resp, err := http.Post(server.URL+"/api/v1/chat/stream", "application/json",
		strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	frames := strings.Split(string(body), "\n\n")
	var errorFrame string
	for _, f := range frames {
		if strings.HasPrefix(f, "event: error") {
			errorFrame = f
		}
	}
	if errorFrame == "" {
		t.Fatalf("no named error frame in:\n%s", body)
	}

	_, data, _ := strings.Cut(errorFrame, "data: ")
	var payload struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("the error payload is not JSON: %v", err)
	}
	if payload.Title == "" || payload.Status == 0 {
		t.Errorf("the error payload has nothing to show a customer: %+v", payload)
	}
	if payload.Type == "error" {
		t.Error("the payload's type happens to say \"error\", which would let a page " +
			"switch on the wrong field and appear to work")
	}
}

// Every error bubble must go through showError, which scrolls the log.
//
// The page scrolled when a user message was appended and as answer chunks arrived, and not
// when an error was appended -- so a refused message filled the log with the customer's own
// text and rendered the explanation 38px below the visible area, measured in Chrome with a
// 4,001-character message against the 4,000 limit. The customer saw their words and no
// reason.
//
// This is a structural check rather than a rendered one: the browser check that found it
// needs a browser, and what keeps it fixed is that no branch appends an error any other
// way. Found in the .NET implementation of this system, which shares this page's shape.
func TestEveryErrorBubbleGoesThroughTheHelperThatScrolls(t *testing.T) {
	page := demoPage(t)

	if !strings.Contains(page, "function showError(") {
		t.Fatal("showError is gone; the three error branches will drift apart again")
	}
	if !regexp.MustCompile(`(?s)function showError\([^)]*\)\s*\{[^}]*scrollTop\s*=\s*log\.scrollHeight`).
		MatchString(page) {
		t.Error("showError does not scroll the log, which is the only reason it exists")
	}

	// The defect, stated as a pattern: an error class appended anywhere but in the helper.
	direct := regexp.MustCompile(`log\.append\(el\('div',\s*'msg err'`)
	for _, loc := range direct.FindAllStringIndex(page, -1) {
		line := 1 + strings.Count(page[:loc[0]], "\n")
		if !strings.Contains(page[max(0, loc[0]-260):loc[0]], "function showError(") {
			t.Errorf("line %d appends an error bubble outside showError, so it will not scroll", line)
		}
	}
}
