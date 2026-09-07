package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The manifests in `k8s/` carry a tag, and `scripts/pin-images.sh` turns that into a digest
// on the way to a real cluster.
//
// The tag is not a recommendation, it is what the verification harness needs: `kind load
// docker-image` moves an image into the node by tag and gives it no registry digest, so a
// digest-pinned `k8s/` would make every harness run pull from GHCR. That is the trade, and
// the script is the other half of it — a tag is a pointer somebody can move, a digest is
// the image.
//
// A script nobody runs is a script that stops working, so this runs it. It needs no cluster
// and no registry: the digests are arguments.
func TestPinningTurnsEveryImageIntoADigest(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "pin-images.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("scripts/pin-images.sh is missing: %v", err)
	}

	const (
		app = "ghcr.io/lai3d/ai-customer-service-go"
		ui  = "ghcr.io/lai3d/ai-customer-service-go-admin-ui"
		a   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		b   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)

	out := t.TempDir()
	run := exec.Command("bash", script, out, app+"="+a, ui+"="+b)
	run.Dir = root
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("pinning failed: %v\n%s", err, output)
	}

	images := imagesIn(t, out)
	if len(images) < 2 {
		t.Fatalf("found %d image references in the pinned output (%v); the script is no "+
			"longer rewriting the manifests this test believes it is", len(images), images)
	}
	for _, ref := range images {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s is still on a tag after pinning", ref)
		}
		// `repo:tag@sha256:...` is legal and is worse than either half: the tag becomes
		// decoration a reader will trust and nothing checks.
		if regexp.MustCompile(`:[^@/]+@sha256:`).MatchString(ref) {
			t.Errorf("%s carries both a tag and a digest", ref)
		}
	}

	// Every image the source manifests have, and no more: a pinned set that quietly dropped
	// one is the outcome that looks done.
	if got, want := len(images), len(imagesIn(t, filepath.Join(root, "k8s"))); got != want {
		t.Errorf("the pinned output has %d image references and k8s/ has %d", got, want)
	}
}

// Half a pinned set is the outcome that looks done. An image with no digest given is an
// error rather than a line left on its tag.
//
// **Two independent guards stand behind this**, and removing either one alone leaves this
// test green — measured, with `-count=1`. The per-image lookup refuses when no digest was
// given for a repository; the read-back at the end refuses when anything in the output is
// still on a tag, whatever the reason. Both had to go to make this red, which is the same
// shape as the layered NetworkPolicy denials in `k8s/`: worth knowing before somebody
// simplifies one away and finds the tests unchanged.
//
// `-count=1` is not incidental. **`go test` caches a result and does not know this test
// shells out to a file**, so editing `scripts/pin-images.sh` and re-running reports the
// previous verdict. The first attempt at the paragraph above was written from a cached
// pass, which would have put an unverified claim in a comment that says "verified".
func TestPinningRefusesToLeaveAnImageOnItsTag(t *testing.T) {
	root := repoRoot(t)
	out := t.TempDir()

	run := exec.Command("bash", filepath.Join(root, "scripts", "pin-images.sh"), out,
		"ghcr.io/lai3d/ai-customer-service-go=sha256:1111111111111111111111111111111111111111111111111111111111111111")
	run.Dir = root
	output, err := run.CombinedOutput()
	if err == nil {
		t.Fatal("pinning succeeded with a digest missing for one of the images")
	}
	if !strings.Contains(string(output), "no digest given") {
		t.Errorf("the error does not say which image was unpinned: %s", output)
	}
}

func imagesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	line := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`)
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range line.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, m[1])
		}
	}
	return out
}
