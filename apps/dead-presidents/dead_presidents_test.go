package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readAsset(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"web"}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// assetVersion is the single source of truth in app.js. The tests read it rather
// than hard-coding a string, so a routine version bump never breaks them.
func assetVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`ASSET_VERSION = '([^']+)'`).FindStringSubmatch(readAsset(t, "app.js"))
	if m == nil {
		t.Fatal("app.js missing ASSET_VERSION")
	}
	return m[1]
}

func TestIndexUsesDeadPresidentsBranding(t *testing.T) {
	index := readAsset(t, "index.html")
	for _, want := range []string{
		"<title>farfield · dead presidents</title>",
		"farfield · dead presidents",                         // masthead brand
		"https://farfield.systems/docs/dead-presidents.html", // app's own docs page
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html missing branding %q", want)
		}
	}
	if !strings.Contains(index, "character-level GPT") {
		t.Fatal("index.html missing project copy")
	}
}

// dead-presidents inherits the shared farfield theme served at /static/styles.css
// from lib/theme (see main.go), so its look can't drift from the rest of the site.
func TestUsesSharedFarfieldTheme(t *testing.T) {
	if _, err := os.Stat(filepath.Join("web", "style.css")); err == nil {
		t.Fatal("web/style.css should not exist — the app uses the shared theme")
	}
	if !strings.Contains(readAsset(t, "index.html"), `href="/static/styles.css?v=`) {
		t.Fatal("index.html should link the shared /static/styles.css")
	}
	if !strings.Contains(readAsset(t, "../main.go"), "theme.CSSHandler()") {
		t.Fatal("main.go should serve the shared theme via theme.CSSHandler at /static/styles.css")
	}
}

// The verified engine/worker/client (copied from the dp model repo) and the
// float32 weights must be present and wired together: worker loads the engine and
// fetches the weights; the client exposes the OpenAI-shaped surface.
func TestVendoredInferenceStackPresent(t *testing.T) {
	for _, f := range []string{"engine.js", "worker.js", "openai.js", "model.json", "model.bin"} {
		if _, err := os.Stat(filepath.Join("web", f)); err != nil {
			t.Fatalf("web/%s missing: %v", f, err)
		}
	}
	worker := readAsset(t, "worker.js")
	if !strings.Contains(worker, `importScripts("./engine.js")`) {
		t.Fatal("worker.js must importScripts the engine")
	}
	// The fetches carry the worker's own ?v= query (self.location.search) so the
	// server can serve the 1.86MB of weights with an immutable cache lifetime.
	if !strings.Contains(worker, `fetch("./model.json" + V)`) || !strings.Contains(worker, `fetch("./model.bin" + V)`) {
		t.Fatal("worker.js must fetch the versioned float32 weights (model.json + model.bin)")
	}
	if !strings.Contains(worker, "self.location.search") {
		t.Fatal("worker.js must derive the model version from its own ?v= query")
	}
	if !strings.Contains(readAsset(t, "openai.js"), "root.DeadPresidents = { createClient: createClient }") {
		t.Fatal("openai.js must expose DeadPresidents.createClient")
	}
	if !strings.Contains(readAsset(t, "app.js"), "DeadPresidents.createClient(") {
		t.Fatal("app.js must drive the model through DeadPresidents.createClient")
	}
}

// Assets carry the version as a cache-busting query, the same version everywhere
// (index → app.js + openai.js + styles.css, app.js → worker.js).
func TestAssetsAreVersionedConsistently(t *testing.T) {
	v := assetVersion(t)
	index := readAsset(t, "index.html")
	for _, want := range []string{
		"./app.js?v=" + v,
		"./openai.js?v=" + v,
		"/static/styles.css?v=" + v,
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html should reference %q", want)
		}
	}
	if !strings.Contains(readAsset(t, "app.js"), "./worker.js?v=${ASSET_VERSION}") {
		t.Fatal("app.js should load worker.js at the shared ASSET_VERSION")
	}
}

func TestIndexHasFarfieldHostChrome(t *testing.T) {
	index := readAsset(t, "index.html")
	for _, want := range []string{
		`class="container"`,
		`class="bar"`,
		`<a href="https://farfield.systems/">`, // links back to the host site
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html missing host-chrome marker %q", want)
		}
	}
}
