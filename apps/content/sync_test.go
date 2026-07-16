package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideMatrix(t *testing.T) {
	cases := []struct {
		name                                          string
		hasLocal, hasRemote, known, localCh, remoteCh bool
		want                                          syncAction
	}{
		{"untouched", true, true, true, false, false, actNone},
		{"local edit", true, true, true, true, false, actPush},
		{"remote edit", true, true, true, false, true, actPull},
		{"both edited", true, true, true, true, true, actConflict},
		{"new local", true, false, false, true, false, actCreate},
		{"new remote", false, true, false, false, true, actPullNew},
		{"remote vanished", true, false, true, false, false, actRemoteGone},
		{"local vanished", false, true, true, false, false, actLocalGone},
		{"never seen anywhere", false, false, false, false, false, actNone},
	}
	for _, c := range cases {
		if got := decide(c.hasLocal, c.hasRemote, c.known, c.localCh, c.remoteCh); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVaultFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posts", "1700000000000-tricky.md")
	e := &Entry{
		Collection: "posts",
		Slug:       "1700000000000-tricky",
		Title:      "Tricky: a title with — punctuation & \"quotes\"",
		Excerpt:    "Colons: everywhere, and #hashes",
		Body:       "Line one.\n\n![alt text](blob://bafkabc123)\n\nDone.",
		Tags:       []string{"a-tag", "with: colon"},
		Published:  true,
		CreatedAt:  "2024-05-01T12:30:00Z",
		UpdatedAt:  "2026-07-01T08:00:00Z",
	}
	if err := writeVaultFile(path, e); err != nil {
		t.Fatal(err)
	}
	got, err := entryFromFile(path, "posts")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != e.Title || got.Excerpt != e.Excerpt || got.Published != e.Published {
		t.Errorf("round trip mangled scalar fields: %+v", got)
	}
	if strings.Join(got.Tags, "|") != strings.Join(e.Tags, "|") {
		t.Errorf("tags mangled: %v", got.Tags)
	}
	if got.Body != e.Body {
		t.Errorf("body mangled:\n%q\nwant\n%q", got.Body, e.Body)
	}
	if got.CreatedAt != e.CreatedAt {
		t.Errorf("created mangled: %s want %s", got.CreatedAt, e.CreatedAt)
	}
	if entryHash(got) != entryHash(e) {
		t.Error("hash unstable across round trip")
	}
}

// fakeContent is an in-memory content service for the sync loop.
type fakeContent struct {
	entries map[string]*Entry
}

func (f *fakeContent) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/entries", func(w http.ResponseWriter, r *http.Request) {
		out := []Entry{}
		for _, e := range f.entries {
			out = append(out, *e)
		}
		json.NewEncoder(w).Encode(map[string]any{"entries": out})
	})
	upsert := func(w http.ResponseWriter, r *http.Request, create bool) {
		var e Entry
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Fatal(err)
		}
		e.CID = "cid-" + entryHash(&e)[:12]
		e.UpdatedAt = "2026-07-16T00:00:00Z"
		if create {
			if e.CreatedAt == "" {
				e.CreatedAt = e.UpdatedAt
			}
		} else if prev, ok := f.entries[e.Slug]; ok {
			e.CreatedAt = prev.CreatedAt
		}
		f.entries[e.Slug] = &e
		json.NewEncoder(w).Encode(e)
	}
	mux.HandleFunc("POST /api/entries", func(w http.ResponseWriter, r *http.Request) { upsert(w, r, true) })
	mux.HandleFunc("PUT /api/entries/{slug}", func(w http.ResponseWriter, r *http.Request) { upsert(w, r, false) })
	// blobs meta stand-in: every bafk CID "exists"
	mux.HandleFunc("GET /blobs/{cid}/meta", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.PathValue("cid"), "bafk") {
			w.Write([]byte(`{"mime":"image/png"}`))
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

func newSyncTest(t *testing.T) (*fakeContent, *syncClient, string) {
	t.Helper()
	f := &fakeContent{entries: map[string]*Entry{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := &syncClient{base: srv.URL, blobsBase: srv.URL, key: "k", http: srv.Client()}
	return f, c, t.TempDir()
}

func TestSyncLifecycle(t *testing.T) {
	f, c, dir := newSyncTest(t)
	var out strings.Builder

	// Remote has one entry the vault lacks; vault has one the remote lacks.
	f.entries["1700000000001-remote-only"] = &Entry{
		Collection: "posts", Slug: "1700000000001-remote-only", Title: "Remote",
		Body: "from the server", CID: "cid-r1",
		CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z",
	}
	mustWrite(t, dir, "posts/1700000000002-local-only.md", `---
title: Local
slug: 1700000000002-local-only
published: true
created: 2024-02-02 10:00
updated: 2024-02-02 10:00
tags:
  - x
excerpt: local piece
---

hello from the vault
`)

	// First sync: pull-new + create.
	if err := syncVault(c, dir, false, "manual", false, &out); err != nil {
		t.Fatalf("sync 1: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "posts", "1700000000001-remote-only.md")); err != nil {
		t.Fatal("remote-only entry was not pulled into the vault")
	}
	if f.entries["1700000000002-local-only"] == nil {
		t.Fatal("local-only entry was not created remotely")
	}
	if got := f.entries["1700000000002-local-only"].CreatedAt; got != "2024-02-02T10:00:00Z" {
		t.Errorf("created create should carry the vault date, got %s", got)
	}

	// Second sync: no changes anywhere.
	out.Reset()
	if err := syncVault(c, dir, false, "manual", false, &out); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if !strings.Contains(out.String(), "0 push · 0 create · 0 pull · 0 pull-new") {
		t.Errorf("second sync should be a no-op:\n%s", out.String())
	}

	// Local edit pushes; remote edit pulls.
	p := filepath.Join(dir, "posts", "1700000000002-local-only.md")
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(raw), "hello from the vault", "hello, edited locally", 1)), 0o644)
	f.entries["1700000000001-remote-only"].Body = "edited on the server"
	f.entries["1700000000001-remote-only"].CID = "cid-r2"

	out.Reset()
	if err := syncVault(c, dir, false, "manual", false, &out); err != nil {
		t.Fatalf("sync 3: %v\n%s", err, out.String())
	}
	if !strings.Contains(f.entries["1700000000002-local-only"].Body, "edited locally") {
		t.Error("local edit did not push")
	}
	pulled, _ := os.ReadFile(filepath.Join(dir, "posts", "1700000000001-remote-only.md"))
	if !strings.Contains(string(pulled), "edited on the server") {
		t.Error("remote edit did not pull")
	}
}

func TestSyncConflict(t *testing.T) {
	f, c, dir := newSyncTest(t)
	var out strings.Builder

	f.entries["1700000000003-both"] = &Entry{
		Collection: "posts", Slug: "1700000000003-both", Title: "Both",
		Body: "original", CID: "cid-b1",
		CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z",
	}
	if err := syncVault(c, dir, false, "manual", false, &out); err != nil {
		t.Fatal(err)
	}

	// Diverge both sides.
	p := filepath.Join(dir, "posts", "1700000000003-both.md")
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(raw), "original", "local version", 1)), 0o644)
	f.entries["1700000000003-both"].Body = "remote version"
	f.entries["1700000000003-both"].CID = "cid-b2"

	out.Reset()
	err := syncVault(c, dir, false, "manual", false, &out)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("manual conflict should error, got %v", err)
	}
	side, _ := os.ReadFile(filepath.Join(dir, "posts", "1700000000003-both.remote.md"))
	if !strings.Contains(string(side), "remote version") {
		t.Error("conflict should write the remote sibling")
	}
	if f.entries["1700000000003-both"].Body != "remote version" {
		t.Error("manual conflict must not push")
	}

	// prefer=local resolves by pushing.
	os.Remove(filepath.Join(dir, "posts", "1700000000003-both.remote.md"))
	out.Reset()
	if err := syncVault(c, dir, false, "local", false, &out); err != nil {
		t.Fatalf("prefer local: %v\n%s", err, out.String())
	}
	if f.entries["1700000000003-both"].Body != "local version" {
		t.Error("prefer=local should push the local body")
	}
}

func TestSyncRefusesIPFSBodies(t *testing.T) {
	_, c, dir := newSyncTest(t)
	mustWrite(t, dir, "posts/1700000000004-old-refs.md", `---
title: Old refs
slug: 1700000000004-old-refs
published: true
created: 2024-02-02 10:00
updated: 2024-02-02 10:00
tags: []
excerpt: ""
---

![](ipfs://bafkoldcidoldcidoldcidoldcidoldcidoldcidoldcidxx)
`)
	var out strings.Builder
	err := syncVault(c, dir, false, "manual", false, &out)
	if err == nil {
		t.Fatal("push with ipfs refs must be refused")
	}
	if !strings.Contains(out.String(), "migrate-refs") {
		t.Errorf("report should point at --migrate-refs:\n%s", out.String())
	}
}

func TestMigrateRefs(t *testing.T) {
	_, c, dir := newSyncTest(t)
	mustWrite(t, dir, "posts/1700000000005-migrate.md", `---
title: Migrate
slug: 1700000000005-migrate
published: true
created: 2024-02-02 10:00
updated: 2024-02-02 10:00
tags: []
excerpt: ""
---

![a](ipfs://bafkgoodcidgoodcidgoodcidgoodcidgoodcidgoodcid) and ipfs://notacidxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
`)
	// A second file with only verified refs — this one rewrites.
	mustWrite(t, dir, "posts/1700000000006-clean.md", `---
title: Clean
slug: 1700000000006-clean
published: true
created: 2024-02-02 10:00
updated: 2024-02-02 10:00
tags: []
excerpt: ""
---

![b](ipfs://bafkgoodcidgoodcidgoodcidgoodcidgoodcidgoodcid)
`)
	var out strings.Builder
	if err := migrateVaultRefs(c, dir, false, &out); err != nil {
		t.Fatal(err)
	}
	// Mixed file: one unverifiable ref poisons the file — nothing rewrites.
	raw, _ := os.ReadFile(filepath.Join(dir, "posts", "1700000000005-migrate.md"))
	if !strings.Contains(string(raw), "ipfs://bafkgood") || !strings.Contains(string(raw), "ipfs://notacid") {
		t.Errorf("mixed file must stay fully untouched:\n%s", raw)
	}
	// Clean file: rewritten.
	clean, _ := os.ReadFile(filepath.Join(dir, "posts", "1700000000006-clean.md"))
	if !strings.Contains(string(clean), "blob://bafkgood") || strings.Contains(string(clean), "ipfs://") {
		t.Errorf("clean file should be fully rewritten:\n%s", clean)
	}
}

func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFirstSyncSeedsEqualEntries(t *testing.T) {
	f, c, dir := newSyncTest(t)
	e := &Entry{
		Collection: "posts", Slug: "1700000000007-same", Title: "Same",
		Body: "identical body", Tags: []string{"t"}, Published: true,
		CID: "cid-s1", CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z",
	}
	f.entries[e.Slug] = e
	if err := writeVaultFile(filepath.Join(dir, "posts", e.Slug+".md"), e); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := syncVault(c, dir, false, "manual", false, &out); err != nil {
		t.Fatalf("equal first sync must not conflict: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 unchanged") {
		t.Errorf("expected quiet state seeding:\n%s", out.String())
	}
	if f.entries[e.Slug].CID != "cid-s1" {
		t.Error("equal entry must not be pushed")
	}
}
