package main

import (
	"bufio"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	_ "modernc.org/sqlite"
)

// provision's stdout is meant to be pasted into .env: every line is a
// comment, blank, or NAME=ffk_…, each name once, each token opening the app
// its callers call with the scope they need.
func TestProvisionPrintsEnvLines(t *testing.T) {
	ks, err := keys.Open(filepath.Join(t.TempDir(), "keys.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ks.Close()

	var out strings.Builder
	if err := provision(ks, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "# ") {
		t.Errorf("output does not open with a header comment:\n%s", out.String())
	}

	envLine := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(ffk_[a-z0-9]+)$`)
	got := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("not an env line: %q", line)
		}
		if _, dup := got[m[1]]; dup {
			t.Errorf("%s printed twice", m[1])
		}
		got[m[1]] = m[2]
	}
	if len(got) != len(provisionEdges) {
		t.Fatalf("printed %d vars, want %d", len(got), len(provisionEdges))
	}
	for _, e := range provisionEdges {
		scope, ok := ks.Check(got[e.Env], e.App)
		if !ok || scope != e.Scope {
			t.Errorf("%s on %s = %q, %v; want %s", e.Env, e.App, scope, ok, e.Scope)
		}
		if _, ok := ks.Check(got[e.Env], "library"); ok {
			t.Errorf("%s opens an app it was not minted for", e.Env)
		}
	}
}
