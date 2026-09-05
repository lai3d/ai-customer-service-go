package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
