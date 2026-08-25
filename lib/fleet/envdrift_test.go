package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The registry tests next door catch a service that drifts out of compose.
// This one catches a *setting* that drifts out of compose, which is a quieter
// failure and was never covered.
//
// Two knobs — SESSION_EPOCH and FARFIELD_TRUSTED_PROXIES — were read by the
// apps, documented in .env.example, and absent from docker-compose.yml
// entirely. Nothing failed. Setting either in the homelab .env did nothing,
// because compose never passed it through, so the fleet's emergency
// session-revocation lever was inert in production and proxy trust never
// narrowed from its permissive default. The existing drift tests could not
// see it: they compare service topology, and a name absent from compose is
// not a disagreement, it is a hole.
//
// The rule here is deliberately narrow, so it accuses only when all three
// signals line up: a name the code reads, and the operator is told to set,
// and the containers never receive.

var (
	// os.Getenv("NAME") / store.Env("NAME", ...) — the names the code reads.
	envReadRe = regexp.MustCompile(`(?:os\.Getenv|store\.Env)\(\s*"([A-Z][A-Z0-9_]*)"`)
	// NAME= at the head of an .env.example line — what the operator is told about.
	envDocRe = regexp.MustCompile(`(?m)^\s*#?\s*([A-Z][A-Z0-9_]*)=`)
	// "- NAME=..." inside a compose environment list — what containers receive.
	envComposeRe = regexp.MustCompile(`(?m)^\s*-\s*([A-Z][A-Z0-9_]*)=`)
)

// repoRoot walks up from the package directory to the directory holding
// go.work, so the test works from any module in the workspace.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found — running outside the workspace")
	return ""
}

// TestEveryReadSettingIsDelivered fails when a setting the code reads and
// .env.example documents never reaches a container.
func TestEveryReadSettingIsDelivered(t *testing.T) {
	root := repoRoot(t)

	composeRaw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Skipf("compose file not readable: %v", err)
	}
	exampleRaw, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Skipf(".env.example not readable: %v", err)
	}

	delivered := set(envComposeRe.FindAllStringSubmatch(string(composeRaw), -1))
	documented := set(envDocRe.FindAllStringSubmatch(string(exampleRaw), -1))

	// Every name the Go sources actually read, and every file that reads it.
	// All readers, not just the first: a name is only exempt below when nothing
	// in a container reads it, and one reader cannot answer that.
	read := map[string][]string{} // name -> files that read it
	for _, dir := range []string{"apps", "lib"} {
		walkGo(t, filepath.Join(root, dir), func(path string, src []byte) {
			rel, _ := filepath.Rel(root, path)
			for _, m := range envReadRe.FindAllStringSubmatch(string(src), -1) {
				if !slices.Contains(read[m[1]], rel) {
					read[m[1]] = append(read[m[1]], rel)
				}
			}
		})
	}
	if len(read) == 0 {
		t.Fatal("found no os.Getenv/store.Env calls at all — the scan is broken, not the config")
	}

	// A Host service is not a container, so compose is the wrong place to look
	// for its settings — they arrive through its systemd EnvironmentFile. A
	// name read only by host services is therefore correctly absent here, and
	// accusing it would train everyone to ignore this test.
	hostDirs := []string{}
	for _, svc := range Services() {
		if svc.Host {
			hostDirs = append(hostDirs, filepath.Join("apps", svc.Name)+string(filepath.Separator))
		}
	}
	onlyHostReads := func(files []string) bool {
		for _, f := range files {
			hosted := false
			for _, dir := range hostDirs {
				if strings.HasPrefix(f, dir) {
					hosted = true
					break
				}
			}
			if !hosted {
				return false
			}
		}
		return len(files) > 0
	}

	for name, where := range read {
		if !documented[name] || delivered[name] || onlyHostReads(where) {
			continue
		}
		t.Errorf("%s is read by %s and documented in .env.example, but no service in "+
			"docker-compose.yml passes it — setting it on the host does nothing", name, where[0])
	}
}

// TestNoAdminPasswordDefault fails if compose ever reintroduces a default for
// PASSWORD. lib/web refuses every login when the password is empty, so a
// default is strictly worse than a missing value: it turns "fail closed" into
// "known credential" across every admin UI the tunnel exposes.
func TestNoAdminPasswordDefault(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Skipf("compose file not readable: %v", err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- PASSWORD=") {
			continue
		}
		if !strings.Contains(trimmed, "${PASSWORD:?") {
			t.Errorf("docker-compose.yml:%d: PASSWORD must use ${PASSWORD:?...} so a missing "+
				".env fails the deploy; got %s", i+1, trimmed)
		}
	}
}

func set(matches [][]string) map[string]bool {
	out := map[string]bool{}
	for _, m := range matches {
		out[m[1]] = true
	}
	return out
}

func walkGo(t *testing.T, root string, fn func(path string, src []byte)) {
	t.Helper()
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil //nolint:nilerr // an unreadable tree means no findings, not a failure
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fn(path, src)
		return nil
	})
}
