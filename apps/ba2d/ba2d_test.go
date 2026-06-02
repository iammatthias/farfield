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
