package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"gopkg.in/yaml.v3"
)

// content sync-vault <dir> — bidirectional sync between an Obsidian vault
// and the content service. <dir> is the vault's content directory: each
// subfolder is a collection, each .md file an entry with YAML frontmatter
// (the same layout import-vault reads).
//
// The sync is three-way: a state file (.farfield-sync.json inside <dir>)
// remembers each entry's remote CID and local hash from the last sync, so
// the tool knows which side changed. Local-only changes push, remote-only
// changes pull, both-changed is a conflict resolved by --prefer
// (manual|local|remote; manual writes a .remote.md sibling and reports).
// Deletions never propagate automatically — a vanished side is reported.
//
// Media rules: bodies reference media as blob://<cid> (and series://<slug>).
// Legacy ipfs://<cid> refs are the same CIDs by construction; --migrate-refs
// rewrites them in place after verifying each CID against the blobs service,
// and a push refuses bodies that still carry ipfs:// refs.
//
// Envs: CONTENT_URL + CONTENT_API_KEY (the write key — required),
// BLOBS_PUBLIC_URL for ref verification.
func runSyncVault(args []string) error {
	fs := flag.NewFlagSet("sync-vault", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report actions without changing anything")
	prefer := fs.String("prefer", "manual", "conflict policy: manual|local|remote")
	migrateRefs := fs.Bool("migrate-refs", false, "rewrite verified ipfs:// refs to blob:// in vault files first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: content sync-vault [flags] <content-dir>")
	}
	dir := fs.Arg(0)
	if *prefer != "manual" && *prefer != "local" && *prefer != "remote" {
		return fmt.Errorf("--prefer must be manual, local, or remote")
	}

	client := &syncClient{
		base:      strings.TrimRight(store.Env("CONTENT_URL", "http://127.0.0.1:8787"), "/"),
		blobsBase: strings.TrimRight(store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"), "/"),
		key:       store.Env("CONTENT_API_KEY", ""),
		http:      &http.Client{Timeout: 30 * time.Second},
	}
	if client.key == "" {
		return fmt.Errorf("CONTENT_API_KEY is required (the write key)")
	}

	return syncVault(client, dir, *dryRun, *prefer, *migrateRefs, os.Stdout)
}

type syncClient struct {
	base, blobsBase, key string
	http                 *http.Client
}

func (c *syncClient) do(method, url string, body any) (*http.Response, error) {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	// The Cloudflare edge in front of the fleet rejects bot-shaped agents.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible) farfield-sync/1.0")
	req.Header.Set("X-API-Key", c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// ── state ──────────────────────────────────────────────────────────────────

const syncStateFile = ".farfield-sync.json"

type syncState struct {
	Version int                   `json:"version"`
	Entries map[string]syncRecord `json:"entries"`
}

type syncRecord struct {
	RemoteCID string `json:"remoteCid"`
	LocalHash string `json:"localHash"`
	SyncedAt  string `json:"syncedAt"`
}

func loadSyncState(dir string) (*syncState, error) {
	st := &syncState{Version: 1, Entries: map[string]syncRecord{}}
	b, err := os.ReadFile(filepath.Join(dir, syncStateFile))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", syncStateFile, err)
	}
	if st.Entries == nil {
		st.Entries = map[string]syncRecord{}
	}
	return st, nil
}

func (st *syncState) save(dir string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, syncStateFile), append(b, '\n'), 0o644)
}

// entryHash fingerprints the fields the sync cares about. Body whitespace is
// normalized so editors that trim trailing space don't cause phantom diffs.
func entryHash(e *Entry) string {
	h := sha256.New()
	for _, part := range []string{
		e.Title, e.Excerpt, strings.Join(e.Tags, ","),
		fmt.Sprint(e.Published), strings.TrimSpace(e.Body),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── decisions ──────────────────────────────────────────────────────────────

type syncAction int

const (
	actNone syncAction = iota
	actPush
	actPull
	actCreate     // local-only, never synced → create remotely
	actPullNew    // remote-only, never synced → write into the vault
	actConflict   // both sides changed since last sync
	actRemoteGone // synced before, remote vanished (trashed) — report only
	actLocalGone  // synced before, local file vanished — report only
)

// decide is the three-way merge table.
func decide(hasLocal, hasRemote, known, localChanged, remoteChanged bool) syncAction {
	switch {
	case hasLocal && hasRemote:
		switch {
		case localChanged && remoteChanged:
			return actConflict
		case localChanged:
			return actPush
		case remoteChanged:
			return actPull
		default:
			return actNone
		}
	case hasLocal && !hasRemote:
		if known {
			return actRemoteGone
		}
		return actCreate
	case !hasLocal && hasRemote:
		if known {
			return actLocalGone
		}
		return actPullNew
	default:
		return actNone
	}
}

// ── vault files ────────────────────────────────────────────────────────────

type vaultFile struct {
	path  string
	entry *Entry
}

// loadVault walks <dir>'s collection subfolders. Files directly in <dir>
// (notes like VOICE_DNA.md) are not entries and are ignored.
func loadVault(dir string) (map[string]vaultFile, error) {
	out := map[string]vaultFile{}
	colls, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, c := range colls {
		if !c.IsDir() || strings.HasPrefix(c.Name(), ".") {
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, c.Name(), "*.md"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			// Conflict siblings are review artifacts, not entries.
			if strings.HasSuffix(f, ".remote.md") {
				continue
			}
			e, err := entryFromFile(f, c.Name())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if prev, dup := out[e.Slug]; dup {
				return nil, fmt.Errorf("duplicate slug %q: %s and %s", e.Slug, prev.path, f)
			}
			out[e.Slug] = vaultFile{path: f, entry: e}
		}
	}
	return out, nil
}

// writeVaultFile renders an entry in the vault's frontmatter style. Values
// go through yaml marshalling so titles with colons stay valid.
func writeVaultFile(path string, e *Entry) error {
	var b strings.Builder
	b.WriteString("---\n")
	writeYAML(&b, "title", e.Title)
	writeYAML(&b, "slug", e.Slug)
	b.WriteString("published: " + fmt.Sprint(e.Published) + "\n")
	writeYAML(&b, "created", vaultTime(e.CreatedAt))
	writeYAML(&b, "updated", vaultTime(e.UpdatedAt))
	if len(e.Tags) == 0 {
		b.WriteString("tags: []\n")
	} else {
		b.WriteString("tags:\n")
		for _, t := range e.Tags {
			b.WriteString("  - " + yamlScalar(t) + "\n")
		}
	}
	writeYAML(&b, "excerpt", e.Excerpt)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(e.Body))
	b.WriteString("\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeYAML(b *strings.Builder, key, val string) {
	b.WriteString(key + ": " + yamlScalar(val) + "\n")
}

func yamlScalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.TrimSuffix(string(out), "\n")
}

// vaultTime renders an RFC3339 stamp the way the vault writes dates. The
// importer parses these as UTC, so UTC keeps the round trip stable.
func vaultTime(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// ── ref migration ──────────────────────────────────────────────────────────

var ipfsRefRe = regexp.MustCompile(`ipfs://([A-Za-z0-9]{46,})`)

// migrateVaultRefs rewrites ipfs://<cid> to blob://<cid> across the vault,
// but only for CIDs the blobs service actually holds — the migration that
// produced the blob store kept CIDs identical, so this is a verified rename.
func migrateVaultRefs(c *syncClient, dir string, dryRun bool, out *strings.Builder) error {
	verified := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.md"))
	if err != nil {
		return err
	}
	changed, unverified := 0, map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		s := string(raw)
		if !strings.Contains(s, "ipfs://") {
			continue
		}
		ok := true
		for _, m := range ipfsRefRe.FindAllStringSubmatch(s, -1) {
			cid := m[1]
			if v, seen := verified[cid]; seen {
				ok = ok && v
				continue
			}
			resp, err := c.do(http.MethodGet, c.blobsBase+"/blobs/"+cid+"/meta", nil)
			v := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				resp.Body.Close()
			}
			verified[cid] = v
			if !v {
				unverified[cid] = true
				ok = false
			}
		}
		if !ok {
			fmt.Fprintf(out, "  refs   %s — has ipfs refs the blob store lacks; left untouched\n", filepath.Base(f))
			continue
		}
		next := ipfsRefRe.ReplaceAllString(s, "blob://$1")
		if next != s {
			changed++
			if !dryRun {
				if err := os.WriteFile(f, []byte(next), 0o644); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "  refs   %s → blob://\n", filepath.Base(f))
		}
	}
	fmt.Fprintf(out, "ref migration: %d files rewritten, %d unverified cids\n", changed, len(unverified))
	return nil
}

// ── the sync ───────────────────────────────────────────────────────────────

func syncVault(c *syncClient, dir string, dryRun bool, prefer string, migrateRefs bool, w interface{ Write([]byte) (int, error) }) error {
	var report strings.Builder

	if migrateRefs {
		if err := migrateVaultRefs(c, dir, dryRun, &report); err != nil {
			return err
		}
	}

	local, err := loadVault(dir)
	if err != nil {
		return err
	}
	remote, err := fetchRemote(c)
	if err != nil {
		return err
	}
	st, err := loadSyncState(dir)
	if err != nil {
		return err
	}

	slugs := map[string]bool{}
	for s := range local {
		slugs[s] = true
	}
	for s := range remote {
		slugs[s] = true
	}
	for s := range st.Entries {
		slugs[s] = true
	}
	ordered := make([]string, 0, len(slugs))
	for s := range slugs {
		ordered = append(ordered, s)
	}
	sort.Strings(ordered)

	counts := map[string]int{}
	conflicts := 0
	for _, slug := range ordered {
		lf, hasLocal := local[slug]
		re, hasRemote := remote[slug]
		rec, known := st.Entries[slug]

		localChanged := hasLocal && (!known || entryHash(lf.entry) != rec.LocalHash)
		remoteChanged := hasRemote && (!known || re.CID != rec.RemoteCID)

		// First encounter of an entry that exists on both sides with equal
		// content: seed the state quietly instead of manufacturing a
		// conflict (and a pointless updatedAt-churning push).
		if hasLocal && hasRemote && !known && entryHash(lf.entry) == entryHash(&re) {
			st.Entries[slug] = syncRecord{
				RemoteCID: re.CID, LocalHash: entryHash(lf.entry),
				SyncedAt: time.Now().UTC().Format(time.RFC3339),
			}
			counts["unchanged"]++
			continue
		}

		act := decide(hasLocal, hasRemote, known, localChanged, remoteChanged)
		if act == actConflict {
			switch prefer {
			case "local":
				act = actPush
			case "remote":
				act = actPull
			}
		}

		switch act {
		case actNone:
			counts["unchanged"]++
		case actPush, actCreate:
			// Only a CID-bearing ref blocks the push — prose that merely
			// mentions the ipfs:// scheme is fine.
			if ipfsRefRe.MatchString(lf.entry.Body) {
				fmt.Fprintf(&report, "  SKIP   %-46s body still has ipfs:// refs — run --migrate-refs\n", slug)
				conflicts++
				continue
			}
			verb := "push"
			if act == actCreate {
				verb = "create"
			}
			fmt.Fprintf(&report, "  %-6s %s\n", verb, slug)
			counts[verb]++
			if dryRun {
				continue
			}
			saved, err := pushEntry(c, lf.entry, act == actCreate)
			if err != nil {
				return fmt.Errorf("%s %s: %w", verb, slug, err)
			}
			st.Entries[slug] = syncRecord{
				RemoteCID: saved.CID, LocalHash: entryHash(lf.entry),
				SyncedAt: time.Now().UTC().Format(time.RFC3339),
			}
			// The server stamps updatedAt — reflect it in the vault file so
			// the frontmatter stays truthful.
			lf.entry.UpdatedAt = saved.UpdatedAt
			lf.entry.CreatedAt = firstNonEmpty(saved.CreatedAt, lf.entry.CreatedAt)
			if err := writeVaultFile(lf.path, lf.entry); err != nil {
				return err
			}
			rec = st.Entries[slug]
			rec.LocalHash = entryHash(lf.entry)
			st.Entries[slug] = rec
		case actPull, actPullNew:
			verb := "pull"
			path := ""
			if act == actPullNew {
				verb = "pull-new"
				path = filepath.Join(dir, re.Collection, slug+".md")
			} else {
				path = lf.path
			}
			fmt.Fprintf(&report, "  %-6s %s\n", verb, slug)
			counts[verb]++
			if dryRun {
				continue
			}
			if err := writeVaultFile(path, &re); err != nil {
				return err
			}
			st.Entries[slug] = syncRecord{
				RemoteCID: re.CID, LocalHash: entryHash(&re),
				SyncedAt: time.Now().UTC().Format(time.RFC3339),
			}
		case actConflict:
			conflicts++
			counts["conflict"]++
			fmt.Fprintf(&report, "  CONFLICT %-44s both sides changed — wrote %s.remote.md\n", slug, slug)
			if dryRun {
				continue
			}
			side := strings.TrimSuffix(lf.path, ".md") + ".remote.md"
			if err := writeVaultFile(side, &re); err != nil {
				return err
			}
		case actRemoteGone:
			counts["remote-gone"]++
			fmt.Fprintf(&report, "  NOTE   %-46s remote entry is gone (trashed?) — local kept; delete the file or restore remotely\n", slug)
		case actLocalGone:
			counts["local-gone"]++
			fmt.Fprintf(&report, "  NOTE   %-46s local file is gone — remote kept; trash it remotely or re-pull by clearing state\n", slug)
		}
	}

	if !dryRun {
		if err := st.save(dir); err != nil {
			return err
		}
	}

	fmt.Fprintf(&report, "sync: %d unchanged · %d push · %d create · %d pull · %d pull-new · %d conflicts\n",
		counts["unchanged"], counts["push"], counts["create"], counts["pull"], counts["pull-new"], counts["conflict"])
	if dryRun {
		report.WriteString("(dry run — nothing written)\n")
	}
	_, err = w.Write([]byte(report.String()))
	if err == nil && conflicts > 0 && prefer == "manual" {
		err = fmt.Errorf("%d conflict(s) need attention", conflicts)
	}
	return err
}

func fetchRemote(c *syncClient) (map[string]Entry, error) {
	resp, err := c.do(http.MethodGet, c.base+"/api/entries?status=all", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list entries: %s", resp.Status)
	}
	var out struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	m := make(map[string]Entry, len(out.Entries))
	for _, e := range out.Entries {
		m[e.Slug] = e
	}
	return m, nil
}

func pushEntry(c *syncClient, e *Entry, create bool) (*Entry, error) {
	method, url := http.MethodPut, c.base+"/api/entries/"+e.Slug
	if create {
		method, url = http.MethodPost, c.base+"/api/entries"
	}
	resp, err := c.do(method, url, map[string]any{
		"collection": e.Collection, "slug": e.Slug, "title": e.Title,
		"excerpt": e.Excerpt, "body": e.Body, "tags": e.Tags,
		"published": e.Published, "createdAt": e.CreatedAt,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var saved Entry
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		return nil, err
	}
	return &saved, nil
}
