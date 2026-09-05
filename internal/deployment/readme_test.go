package deployment_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README exists in English and Chinese, and a translation drifts the moment a section
// is added to one and not the other. Nothing re-derives a translation, which is the same
// failure mode that put a stale test count in this README twice in one session.
//
// Comparing heading *text* is impossible across languages; comparing the sequence of
// heading levels catches the drift that actually happens -- a section added, removed or
// moved on one side only.
func TestBothReadmesHaveTheSameSectionStructure(t *testing.T) {
	root := repoRoot(t)
	english := headingLevels(t, filepath.Join(root, "README.md"))
	chinese := headingLevels(t, filepath.Join(root, "README.zh.md"))

	if len(english) == 0 {
		t.Fatal("read no headings from README.md")
	}
	if strings.Join(english, ",") != strings.Join(chinese, ",") {
		t.Errorf("the two READMEs have diverged.\n  README.md    %v\n  README.zh.md %v\n"+
			"A section was added, removed or moved on one side only.", english, chinese)
	}
}

// Each README points at the other, so a reader landing on either can find their language.
func TestTheReadmesLinkToEachOther(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct{ file, wantLink string }{
		{"README.md", "README.zh.md"},
		{"README.zh.md", "README.md"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "("+tc.wantLink+")") {
			t.Errorf("%s does not link to %s", tc.file, tc.wantLink)
		}
	}
}

var heading = regexp.MustCompile(`(?m)^(#{1,6})\s+\S`)

// headingLevels returns the heading depths in order, ignoring anything inside a fenced
// code block -- a shell comment starting with # is not a heading.
func headingLevels(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var levels []string
	inFence := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := heading.FindStringSubmatch(line + "\n"); m != nil {
			levels = append(levels, m[1])
		}
	}
	return levels
}
