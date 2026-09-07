package deployment_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Splitting the UI out gave it its own copy of two things the server also knows, and a
// copy in another language is one nothing re-derives. These are the two that would go
// wrong quietly rather than loudly.

// The UI offers the moves it believes are legal. The server decides. If the two disagree
// the UI either offers a move that will be refused with a 422 -- annoying but visible --
// or hides one that is legal, which nobody reports because it looks like the feature was
// never built.
func TestTheUIsStateMachineMatchesTheServers(t *testing.T) {
	root := repoRoot(t)
	server := goTransitions(t, filepath.Join(root, "internal", "ticket", "admin.go"))
	ui := tsTransitions(t, filepath.Join(root, "admin-ui", "src", "api", "types.ts"))

	if len(server) < 4 {
		t.Fatalf("read %d states out of the Go state machine; the shape has changed and "+
			"this test is no longer reading it", len(server))
	}
	for state, want := range server {
		got, ok := ui[state]
		if !ok {
			t.Errorf("the UI does not know the state %s", state)
			continue
		}
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("from %s the server allows %v and the UI offers %v", state, want, got)
		}
	}
	for state := range ui {
		if _, ok := server[state]; !ok {
			t.Errorf("the UI offers moves from %s, which the server does not have", state)
		}
	}
}

// The equivalent of the markup check the embedded page carried. React escapes by default,
// so the only way to put a model's words into the DOM as markup is to ask for it by name.
func TestTheUINeverTurnsAStringIntoMarkup(t *testing.T) {
	root := filepath.Join(repoRoot(t), "admin-ui", "src")
	sinks := map[string]*regexp.Regexp{
		// Matched as a use and not a mention. The plain word first fired on the comment
		// in Markdown.tsx that explains why the prop is never used -- the third time in
		// this repository that a detector has measured the prose instead of the code.
		"dangerouslySetInnerHTML": regexp.MustCompile(`dangerouslySetInnerHTML\s*=`),
		"innerHTML assignment":    regexp.MustCompile(`\.innerHTML\s*[+]?=`),
		"document.write":          regexp.MustCompile(`document\.write\s*\(`),
		"eval":                    regexp.MustCompile(`[^.\w]eval\s*\(`),
		"Function constructor":    regexp.MustCompile(`new\s+Function\s*\(`),
	}
	// The token reads every conversation in the database; localStorage outlives the tab.
	// Matched as a use, not a mention, because the comment explaining the choice named it.
	local := regexp.MustCompile(`\blocalStorage\s*\.`)

	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".ts" && ext != ".tsx" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		src := string(raw)
		rel, _ := filepath.Rel(root, path)
		for name, pattern := range sinks {
			if loc := pattern.FindStringIndex(src); loc != nil {
				t.Errorf("%s uses %s at line %d",
					rel, name, 1+strings.Count(src[:loc[0]], "\n"))
			}
		}
		if local.MatchString(src) {
			t.Errorf("%s puts something in localStorage; the operator token outlives the tab there", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 5 {
		t.Errorf("only %d source files were scanned; this test is not looking at the UI", files)
	}
}

// goTransitions reads `allowedTransitions` out of internal/ticket/admin.go.
func goTransitions(t *testing.T, path string) map[string][]string {
	t.Helper()
	return parseTransitions(t, path,
		regexp.MustCompile(`(?s)allowedTransitions\s*=\s*map\[State\]\[\]State\{(.*?)\n\}`),
		regexp.MustCompile(`State(\w+):\s*\{([^}]*)\}`),
		func(s string) string { return stateConstant(s) },
		regexp.MustCompile(`State(\w+)`))
}

// tsTransitions reads NEXT_STATES out of admin-ui/src/api/types.ts.
func tsTransitions(t *testing.T, path string) map[string][]string {
	t.Helper()
	return parseTransitions(t, path,
		regexp.MustCompile(`(?s)NEXT_STATES:\s*Record<TicketState,\s*TicketState\[\]>\s*=\s*\{(.*?)\n\}`),
		regexp.MustCompile(`(\w+):\s*\[([^\]]*)\]`),
		func(s string) string { return s },
		regexp.MustCompile(`'(\w+)'`))
}

func parseTransitions(t *testing.T, path string, block, entry *regexp.Regexp,
	name func(string) string, member *regexp.Regexp) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := block.FindSubmatch(raw)
	if found == nil {
		t.Fatalf("no state machine found in %s; this test reads its source", path)
	}
	out := map[string][]string{}
	for _, m := range entry.FindAllStringSubmatch(string(found[1]), -1) {
		var to []string
		for _, s := range member.FindAllStringSubmatch(m[2], -1) {
			to = append(to, name(s[1]))
		}
		sort.Strings(to)
		out[name(m[1])] = to
	}
	return out
}

// stateConstant maps the Go identifier suffix to the value the wire and the UI use.
func stateConstant(suffix string) string {
	switch suffix {
	case "Open":
		return "OPEN"
	case "InProgress":
		return "IN_PROGRESS"
	case "Resolved":
		return "RESOLVED"
	case "Closed":
		return "CLOSED"
	}
	return fmt.Sprintf("UNKNOWN(%s)", suffix)
}

// Every background job this service depends on is actually started.
//
// Written because two of them were not. `Limits.SweepWindows` and `Sessions.Sweep` were
// both implemented, both unit-tested, and called from nothing, so `rate_window` grew for
// ever behind two correct sweepers — found by another agent reading the code rather than
// by anything here. A unit test proves a function works; only this kind proves it runs.
//
// A source-level check, which is crude, and the crudeness is the point: the failure it
// catches is a call that does not exist, and there is nothing subtler than absence.
func TestEveryBackgroundJobIsStartedByTheServer(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	main := string(raw)

	// Each entry: what has to appear in main, and what silently grows or rots without it.
	jobs := []struct{ call, consequence string }{
		{"identity.NewHygiene", "expired sessions and closed rate windows accumulate for ever"},
		{"retention.NewSweeper", "customer conversations are kept past the retention period that was promised"},
		{"recorder.Sweep", "a turn whose process died stays in_flight for ever and is counted as abandoned by the customer"},
	}
	for _, job := range jobs {
		if !strings.Contains(main, job.call) {
			t.Errorf("cmd/server/main.go never calls %s, so %s", job.call, job.consequence)
			continue
		}
		// Started, not merely constructed. `x := identity.NewHygiene(...)` with no `go`
		// and no `Run` is the same defect wearing a constructor.
		if !regexp.MustCompile(`go\s+[^\n]*`+regexp.QuoteMeta(job.call)).MatchString(main) &&
			!regexp.MustCompile(regexp.QuoteMeta(job.call)+`[^\n]*\n?[^\n]*\.Run\(`).MatchString(main) &&
			!regexp.MustCompile(`go func\(\)`).MatchString(main) {
			t.Errorf("%s is constructed but never started", job.call)
		}
	}
	if len(jobs) < 3 {
		t.Error("the job list has shrunk; this test is no longer looking at anything")
	}
}
