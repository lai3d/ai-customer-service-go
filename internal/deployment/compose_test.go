package deployment_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Compose does not inject an undeclared variable into a container.
//
// Anything documented in .env.example has to be listed in the app service's
// `environment:` block, or setting it silently does nothing and a default quietly takes
// effect. The failure has no error message: the service starts, works, and uses the
// wrong configuration.
//
// Never dump `docker compose config` output to check this. It interpolates real values
// from the shell, which is how a live API key ends up in a terminal transcript.
func TestEveryDocumentedVariableReachesTheContainer(t *testing.T) {
	root := repoRoot(t)
	documented := variablesIn(t, filepath.Join(root, ".env.example"))
	declared := appServiceEnvironment(t, filepath.Join(root, "docker-compose.yml"))

	// Set by the deployment rather than passed through, and each for a reason:
	//
	//   POSTGRES_HOST/PORT  the compose file points the app at the service name, so a
	//                       value meant for a host-side run does not send the container
	//                       looking for a database on itself.
	//   HTTP_ADDR           the image fixes it at :8081 and the compose file publishes
	//                       8081:8081. Passing it through would let .env move the
	//                       listener out from under the port mapping. It is documented
	//                       because it matters for `make run` from source.
	forwarded := map[string]bool{
		"POSTGRES_HOST": true, "POSTGRES_PORT": true, "HTTP_ADDR": true,
	}

	for _, name := range documented {
		if forwarded[name] {
			continue
		}
		if !declared[name] {
			t.Errorf(".env.example documents %s but the app service does not declare it; "+
				"setting it would silently do nothing", name)
		}
	}
	if len(documented) == 0 {
		t.Fatal("read no variables from .env.example")
	}
}

var envLine = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=`)

func variablesIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range envLine.FindAllStringSubmatch(string(raw), -1) {
		names = append(names, m[1])
	}
	return names
}

// appServiceEnvironment reads the keys under the app service's environment: block. A
// YAML parser would be more robust and would also pull in a dependency to read eleven
// lines; the block is indented consistently and the test fails loudly if that changes.
func appServiceEnvironment(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")

	declared := map[string]bool{}
	inApp, inEnv := false, false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "  app:"):
			inApp = true
		case len(line) > 2 && line[0] == ' ' && line[1] == ' ' && line[2] != ' ' &&
			strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.HasPrefix(line, "  app:"):
			inApp = false
			inEnv = false
		}
		if !inApp {
			continue
		}
		if strings.TrimSpace(line) == "environment:" {
			inEnv = true
			continue
		}
		if inEnv {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if !strings.HasPrefix(line, "      ") {
				inEnv = false
				continue
			}
			if name, _, ok := strings.Cut(trimmed, ":"); ok {
				declared[strings.TrimSpace(name)] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no environment declarations for the app service")
	}
	return declared
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
