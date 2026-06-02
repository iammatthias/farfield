package main

import (
	"os"
	"path/filepath"
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

func TestIndexCopyUsesBARD(t *testing.T) {
	index := readAsset(t, "index.html")
	if !strings.Contains(index, "<h1>BARD</h1>") {
		t.Fatalf("index.html missing BARD headline")
	}
	if !strings.Contains(index, "small Shakespeare-trained GPT onchain") {
		t.Fatalf("index.html missing project copy")
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

func TestStyleUsesFarfieldPaperSystem(t *testing.T) {
	style := readAsset(t, "style.css")
	for _, want := range []string{"--surface: #fafaf7", "--ink: #0a0a0a", "box-shadow: none", "font: 500 0.7rem/1 var(--font-mono)"} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}
	for _, bad := range []string{"radial-gradient", "box-shadow:0", "border-radius:20px"} {
		if strings.Contains(style, bad) {
			t.Fatalf("style.css still contains non-Farfield token %q", bad)
		}
	}
}

func TestAppUsesVersionedAssets(t *testing.T) {
	index := readAsset(t, "index.html")
	if !strings.Contains(index, "./app.js?v=20260602-farfield-2") {
		t.Fatalf("index.html should load versioned app.js")
	}
	app := readAsset(t, "app.js")
	if !strings.Contains(app, "./worker.js?v=") {
		t.Fatalf("app.js should load a versioned worker.js")
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
	if !strings.Contains(worker, "maxChunkBytes") {
		t.Fatalf("worker.js must decode maxChunkBytes in config()")
	}
}
