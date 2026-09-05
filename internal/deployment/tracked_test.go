package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every path the k8s README draws in its file tree must be in the repository -- not on
// this machine, in the repository.
//
// The distinction is the whole test. `k8s/examples/secret.yaml` was written, reviewed,
// and referenced from that tree, and a bare `secret.yaml` line in `k8s/.gitignore` also
// matched it, so it was never committed. It read as present in every way anyone would
// check: it was on disk, `cat` showed it, the README pointed at it. It was absent only
// for someone who cloned the repository -- which is everyone the README is written for.
//
// So this asks git, not the filesystem. An `os.Stat` here would have passed throughout,
// which is the same shape as the three silently blind detectors `CLAUDE.md` lists: a
// check that consults the same source as the thing it is checking.
func TestEveryPathTheKubernetesReadmeDrawsIsInTheRepository(t *testing.T) {
	root := repoRoot(t)
	// A build from a source tarball has no git. Skipped rather than failed, and named
	// loudly, because a silent skip is how a check stops being one.
	requireGit(t, root)

	raw, err := os.ReadFile(filepath.Join(root, "k8s", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	tree := string(raw)
	block := regexp.MustCompile("(?s)```\\nk8s/\\n(.*?)```").FindStringSubmatch(tree)
	if block == nil {
		t.Fatal("the k8s README no longer draws a file tree; this test reads it")
	}

	// The tree is drawn with box characters and trailing prose. What is wanted from each
	// line is the filename, and the directory it hangs under.
	var paths []string
	dir := ""
	for _, line := range strings.Split(block[1], "\n") {
		m := regexp.MustCompile(`[├└]──\s+(\S+)`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		nested := strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "│")
		switch {
		case strings.HasSuffix(name, "/"):
			dir = name
		case nested && dir != "":
			paths = append(paths, "k8s/"+dir+name)
		default:
			paths = append(paths, "k8s/"+name)
		}
	}
	if len(paths) < 5 {
		t.Fatalf("read only %d paths out of the tree (%v); the drawing has changed shape",
			len(paths), paths)
	}

	for _, p := range paths {
		out, err := exec.Command("git", "-C", root, "ls-files", "--error-unmatch", p).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			ignored, _ := exec.Command("git", "-C", root, "check-ignore", "-v", p).Output()
			t.Errorf("the k8s README points at %s, which is not in the repository.\n"+
				"Anyone who clones this will not have it.%s", p, ignoredBy(string(ignored)))
		}
	}
}

// git check-ignore reports the rule that decided the path, which may be a negation --
// i.e. a rule saying the file is *not* ignored. Reporting that as the cause would send
// the next reader after the wrong thing; an untracked-but-not-ignored file just needs
// `git add`.
func ignoredBy(rule string) string {
	rule = strings.TrimSpace(rule)
	if rule == "" || strings.Contains(rule, ":!") {
		return "\nIt is not ignored, so it was simply never added."
	}
	return "\nIt is ignored by: " + rule
}

// The same question as the test above, asked of every link in every document rather than
// of one drawing: a relative link in a markdown file must point at something the
// repository actually contains.
//
// The k8s tree test would not have caught this class anywhere else, because a drawn tree
// is not a link, and the sweep that proved the rest of the documentation clean was run by
// hand once. A check run by hand once is a fact about that afternoon.
//
// Links are checked against `git ls-files`, again not against the filesystem, and the
// count of links checked is asserted -- a link regex that stops matching would otherwise
// pass this test by finding nothing to disagree with.
func TestEveryFileTheDocumentationLinksToIsInTheRepository(t *testing.T) {
	root := repoRoot(t)
	requireGit(t, root)

	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	var dirs []string
	for _, f := range strings.Fields(string(out)) {
		tracked[f] = true
		for d := filepath.Dir(f); d != "." && d != "/"; d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	trackedDir := map[string]bool{}
	for _, d := range dirs {
		trackedDir[d] = true
	}

	// A scheme (http:, mailto:, and the javascript: that docs/operations.md quotes while
	// explaining why the renderer refuses links) is not a path.
	scheme := regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`)
	link := regexp.MustCompile(`\]\(([^)]+)\)`)

	checked := 0
	for f := range tracked {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range link.FindAllStringSubmatch(string(raw), -1) {
			target := strings.Fields(m[1])[0] // drop a ("title") suffix
			target, _, _ = strings.Cut(target, "#")
			if target == "" || scheme.MatchString(target) {
				continue
			}
			p := filepath.Clean(filepath.Join(filepath.Dir(f), target))
			if strings.HasPrefix(p, "..") {
				// A link into the sibling Java repository. Real, and not checkable from
				// inside this one -- whoever clones this has no such sibling.
				continue
			}
			checked++
			if tracked[p] || trackedDir[p] {
				continue
			}
			ignored, _ := exec.Command("git", "-C", root, "check-ignore", "-v", p).Output()
			t.Errorf("%s links to %s, which is not in the repository.%s",
				f, target, ignoredBy(string(ignored)))
		}
	}
	if checked < 20 {
		t.Errorf("only %d relative links were checked; the link pattern has stopped "+
			"matching and this test is no longer looking at anything", checked)
	}
	t.Logf("%d relative links checked against git ls-files", checked)
}

func requireGit(t *testing.T, root string) {
	t.Helper()
	if err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Run(); err != nil {
		t.Skip("not a git checkout: this test cannot tell tracked from merely present")
	}
}
