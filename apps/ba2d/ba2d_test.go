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

func TestStyleUsesFarfieldHostSystem(t *testing.T) {
	style := readAsset(t, "style.css")
	for _, want := range []string{
		"--surface: #040810",
		"--ink: #e8eef8",
		"box-shadow: none",
		"font: 500 0.7rem/1 var(--font-mono)",
		"radial-gradient",
		"backdrop-filter: blur(8px)",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}
	for _, bad := range []string{"--surface: #fafaf7", "border-radius:20px"} {
		if strings.Contains(style, bad) {
			t.Fatalf("style.css still contains non-host token %q", bad)
		}
	}
}

func TestAppUsesVersionedAssets(t *testing.T) {
	index := readAsset(t, "index.html")
	if !strings.Contains(index, "./app.js?v=20260602-farfield-host-1") {
		t.Fatalf("index.html should load versioned app.js")
	}
	if !strings.Contains(index, "./style.css?v=20260602-farfield-host-1") {
		t.Fatalf("index.html should load versioned style.css")
	}
	app := readAsset(t, "app.js")
	if !strings.Contains(app, "./worker.js?v=") {
		t.Fatalf("app.js should load a versioned worker.js")
	}
	if !strings.Contains(app, "20260602-farfield-host-1") {
		t.Fatalf("app.js should expose the host-aligned ASSET_VERSION")
	}
}

func TestIndexHasFarfieldHostChrome(t *testing.T) {
	index := readAsset(t, "index.html")
	for _, want := range []string{
		`<title>ba2d · Farfield Systems</title>`,
		`class="masthead"`,
		`class="brand"`,
		`Farfield Systems · 0xba2d · onchain inference`,
		`class="colophon"`,
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
