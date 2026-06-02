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

// assetVersion is the single source of truth in app.js. The tests read it
// rather than hard-coding a string, so a routine version bump never breaks them.
func assetVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`ASSET_VERSION = '([^']+)'`).FindStringSubmatch(readAsset(t, "app.js"))
	if m == nil {
		t.Fatal("app.js missing ASSET_VERSION")
	}
	return m[1]
}

func TestIndexUsesBardBranding(t *testing.T) {
	index := readAsset(t, "index.html")
	for _, want := range []string{
		"<title>farfield · bard</title>",
		"farfield · bard", // masthead brand
		"https://farfield.systems/docs/bard.html", // app's own docs page
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html missing bard branding %q", want)
		}
	}
	if !strings.Contains(index, "GPT trained on Shakespeare, stored onchain") {
		t.Fatal("index.html missing project copy")
	}
}

func TestAppAutoLoadsWeightsOnStartup(t *testing.T) {
	app := readAsset(t, "app.js")
	if !strings.Contains(app, "loadWeightsFromChain();") {
		t.Fatalf("app.js does not auto-load weights on startup")
	}
	if strings.Contains(app, "els.load.addEventListener('click', () => {") {
		t.Fatalf("app.js still uses an inline click-only loader")
	}
}

// bard dropped its local stylesheet and now inherits the shared farfield theme,
// served at /static/styles.css from lib/theme (see main.go), so the look can't
// drift from the rest of the site.
func TestUsesSharedFarfieldTheme(t *testing.T) {
	if _, err := os.Stat(filepath.Join("web", "style.css")); err == nil {
		t.Fatal("web/style.css should not exist — bard uses the shared theme")
	}
	if !strings.Contains(readAsset(t, "index.html"), `href="/static/styles.css?v=`) {
		t.Fatal("index.html should link the shared /static/styles.css")
	}
	if !strings.Contains(readAsset(t, "../main.go"), "io.WriteString(w, theme.CSS)") {
		t.Fatal("main.go should serve the shared theme.CSS at /static/styles.css")
	}
}

// Assets carry the version as a cache-busting query, and the version is the
// same everywhere it appears (index → app.js + styles.css, app.js → worker.js).
func TestAssetsAreVersionedConsistently(t *testing.T) {
	v := assetVersion(t)
	index := readAsset(t, "index.html")
	for _, want := range []string{"./app.js?v=" + v, "/static/styles.css?v=" + v} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html should load %q", want)
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
		`>Etherscan<`,
		`>Sourcify<`,
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.html missing host-chrome marker %q", want)
		}
	}
}

func TestWorkerUsesArtifactHashFromArtifactCall(t *testing.T) {
	worker := readAsset(t, "worker.js")
	if strings.Contains(worker, "config().artifactHash does not match artifactHash()") {
		t.Fatalf("worker.js still compares config().artifactHash to artifactHash()")
	}
	if !strings.Contains(worker, "const artifactHash = decodeBytes32(artifactResult);") {
		t.Fatalf("worker.js must use artifactHash() as the source of truth")
	}
	if !strings.Contains(worker, "const payload = bytes.subarray(64);") {
		t.Fatalf("worker.js must decode the live config() payload layout")
	}
	if !strings.Contains(worker, "const name = decodeUtf8(payload.subarray(192, 192 + nameLength));") {
		t.Fatalf("worker.js must decode the model name from the config payload")
	}
	if !strings.Contains(worker, "return { quant, nLayer, nHead, nEmbd, blockSize, vocabSize, ffnDim, paramCount, vocab, vocabByteLen, tensors, bosId: vocabSize - 1 };") {
		t.Fatalf("worker.js must expose paramCount from parseArtifact")
	}
}
