package keys

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the tests
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "keys.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMintAndCheck(t *testing.T) {
	s := openTest(t)
	token, k, err := s.Mint("intern", "library", ScopeUpload, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(token, "ffk_") {
		t.Errorf("token %q lacks ffk_ prefix", token)
	}
	if !strings.HasPrefix(token, k.Hint) {
		t.Errorf("hint %q is not a prefix of the token", k.Hint)
	}

	scope, ok := s.Check(token, "library")
	if !ok || scope != ScopeUpload {
		t.Fatalf("Check(library) = %q, %v; want upload, true", scope, ok)
	}
	if _, ok := s.Check(token, "feed"); ok {
		t.Error("key scoped to library was accepted for feed")
	}
	if _, ok := s.Check("ffk_nonsense", "library"); ok {
		t.Error("unknown token was accepted")
	}
	if _, ok := s.Check("", "library"); ok {
		t.Error("empty token was accepted")
	}
}

func TestWildcardApp(t *testing.T) {
	s := openTest(t)
	token, _, err := s.Mint("everywhere", AppAny, ScopeRead, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, app := range []string{"feed", "content", "blobs"} {
		if scope, ok := s.Check(token, app); !ok || scope != ScopeRead {
			t.Errorf("wildcard key rejected for %s", app)
		}
	}
}

func TestRevoke(t *testing.T) {
	s := openTest(t)
	token, k, _ := s.Mint("temp", "feed", ScopeWrite, time.Time{})
	if _, ok := s.Check(token, "feed"); !ok {
		t.Fatal("fresh key rejected")
	}
	if ok, err := s.Revoke(k.ID); err != nil || !ok {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if _, ok := s.Check(token, "feed"); ok {
		t.Error("revoked key still accepted")
	}
	// Second revoke is a no-op, unknown id reports false.
	if ok, _ := s.Revoke(k.ID); ok {
		t.Error("re-revoke reported a change")
	}
	if ok, _ := s.Revoke("missing"); ok {
		t.Error("revoking unknown id reported a change")
	}
}

func TestExpiry(t *testing.T) {
	s := openTest(t)
	past, _, _ := s.Mint("expired", "feed", ScopeRead, time.Now().Add(-time.Hour))
	if _, ok := s.Check(past, "feed"); ok {
		t.Error("expired key accepted")
	}
	future, _, _ := s.Mint("fresh", "feed", ScopeRead, time.Now().Add(time.Hour))
	if _, ok := s.Check(future, "feed"); !ok {
		t.Error("unexpired key rejected")
	}
}

func TestListAndDelete(t *testing.T) {
	s := openTest(t)
	_, a, _ := s.Mint("a", "feed", ScopeRead, time.Time{})
	_, b, _ := s.Mint("b", "*", ScopeWrite, time.Time{})
	ks, err := s.List()
	if err != nil || len(ks) != 2 {
		t.Fatalf("List = %d keys, %v; want 2", len(ks), err)
	}
	for _, k := range ks {
		if k.Hint == "" || len(k.Hint) < 5 {
			t.Errorf("key %s has no hint", k.ID)
		}
	}
	if ok, _ := s.Delete(a.ID); !ok {
		t.Error("Delete existing = false")
	}
	ks, _ = s.List()
	if len(ks) != 1 || ks[0].ID != b.ID {
		t.Errorf("after delete, list = %+v", ks)
	}
}

func TestMintValidation(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.Mint("", "feed", ScopeRead, time.Time{}); err == nil {
		t.Error("empty name accepted")
	}
	if _, _, err := s.Mint("x", "", ScopeRead, time.Time{}); err == nil {
		t.Error("empty app accepted")
	}
	if _, _, err := s.Mint("x", "feed", "admin", time.Time{}); err == nil {
		t.Error("unknown scope accepted")
	}
}

// Check runs on every authenticated request across the fleet, and each app
// opens the same database file. Stamping last_used_at on every hit turned a
// read path into a fleet-wide write path; it is throttled now.
func TestCheckThrottlesLastUsedWrites(t *testing.T) {
	s := openTest(t)
	token, k, err := s.Mint("hot path", "content", ScopeRead, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := s.Check(token, "content"); !ok {
		t.Fatal("freshly minted key was refused")
	}
	first := lastUsed(t, s, k.ID)
	if first == "" {
		t.Fatal("the first Check did not stamp last_used_at")
	}

	// Blank the column behind the store's back. Further Checks inside the
	// interval must not rewrite it — proving they issued no UPDATE at all.
	if _, err := s.db.Exec(`UPDATE api_keys SET last_used_at = NULL WHERE id = ?`, k.ID); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, ok := s.Check(token, "content"); !ok {
			t.Fatal("valid key refused")
		}
	}
	if got := lastUsed(t, s, k.ID); got != "" {
		t.Errorf("last_used_at was rewritten within the throttle window (%q)", got)
	}

	// Once the recorded stamp ages past the interval, the next hit writes again.
	s.stampMu.Lock()
	s.stamped[k.ID] = time.Now().Add(-stampInterval - time.Minute)
	s.stampMu.Unlock()
	if _, ok := s.Check(token, "content"); !ok {
		t.Fatal("valid key refused")
	}
	if lastUsed(t, s, k.ID) == "" {
		t.Error("last_used_at was not refreshed after the throttle window")
	}
}

func lastUsed(t *testing.T, s *Store, id string) string {
	t.Helper()
	var v sql.NullString
	if err := s.db.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v.String
}

func usageRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM key_usage`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Usage is counted in memory and written in batches: hundreds of checks cost
// no write until a flush, and a flush merges into the stored counts.
func TestUsageRollupFlush(t *testing.T) {
	s := openTest(t)
	token, k, err := s.Mint("hot path", "content", ScopeRead, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if _, ok := s.CheckRequest(token, "content", "get", "/api/entries/x?draft=1"); !ok {
			t.Fatal("valid key refused")
		}
	}
	if _, ok := s.CheckRequest(token, "content", "GET", "/api/entries"); !ok {
		t.Fatal("valid key refused")
	}
	if n := usageRows(t, s); n != 0 {
		t.Fatalf("usage written per request: %d rows before any flush", n)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := usageRows(t, s); n != 2 {
		t.Fatalf("after flush, %d usage rows; want 2 (one per route)", n)
	}
	var count int64
	var path string
	if err := s.db.QueryRow(`SELECT count, path FROM key_usage
		WHERE key_id = ? AND path LIKE '/api/entries/%'`, k.ID).Scan(&count, &path); err != nil {
		t.Fatal(err)
	}
	if count != 200 || path != "/api/entries/x" {
		t.Errorf("rollup = %d on %q; want 200 on /api/entries/x (no query)", count, path)
	}

	// A second batch merges into the same row rather than adding one.
	for range 5 {
		s.CheckRequest(token, "content", "GET", "/api/entries/x")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := usageRows(t, s); n != 2 {
		t.Errorf("second flush added rows: %d, want 2", n)
	}
	u, err := s.Usage(k.ID, 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.OK != 206 || u.Refused != 0 {
		t.Errorf("totals = %d ok, %d refused; want 206, 0", u.OK, u.Refused)
	}
	if len(u.Days) != 30 || u.Days[29].OK != 206 {
		t.Errorf("days = %d, today = %+v; want 30 days with 206 today", len(u.Days), u.Days[len(u.Days)-1])
	}
	if len(u.ByApp) != 1 || u.ByApp[0].App != "content" {
		t.Errorf("by app = %+v", u.ByApp)
	}

	// An empty flush writes nothing and is not an error.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

// A dead key still being presented is the signal worth keeping; a token that
// names no key leaves no trace.
func TestUsageCountsRefusals(t *testing.T) {
	s := openTest(t)
	token, k, _ := s.Mint("old", "feed", ScopeWrite, time.Time{})
	expired, ek, _ := s.Mint("lapsed", "feed", ScopeRead, time.Now().Add(-time.Hour))
	if _, err := s.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, ok := s.CheckRequest(token, "feed", "POST", "/api/posts"); ok {
			t.Fatal("revoked key accepted")
		}
	}
	s.CheckRequest(expired, "feed", "GET", "/api/posts")
	s.CheckRequest("ffk_unknowntoken", "feed", "GET", "/api/posts")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	u, _ := s.Usage(k.ID, 30, 10)
	if u.Refused != 3 || u.OK != 0 || len(u.Recent) != 1 || u.Recent[0].Outcome != OutcomeRevoked {
		t.Errorf("revoked usage = %+v", u)
	}
	u, _ = s.Usage(ek.ID, 30, 10)
	if u.Refused != 1 || u.Recent[0].Outcome != OutcomeExpired {
		t.Errorf("expired usage = %+v", u)
	}
	if n := usageRows(t, s); n != 2 {
		t.Errorf("usage rows = %d; want 2 (the unknown token is not recorded)", n)
	}
}

func TestUsagePruneAndDelete(t *testing.T) {
	s := openTest(t)
	token, k, _ := s.Mint("a", "feed", ScopeRead, time.Time{})
	old := time.Now().UTC().Add(-usageRetention - 48*time.Hour).Format(dayLayout)
	if _, err := s.db.Exec(`INSERT INTO key_usage
		(key_id, app, day, method, path, outcome, count, last_at)
		VALUES (?, 'feed', ?, 'GET', '/x', 'ok', 9, ?)`, k.ID, old, old+"T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s.CheckRequest(token, "feed", "GET", "/api/posts")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM key_usage WHERE day = ?`, old).Scan(&n)
	if n != 0 {
		t.Error("rollup older than the retention window survived a flush")
	}
	if usageRows(t, s) != 1 {
		t.Error("today's rollup missing")
	}

	// Deleting the key takes its usage with it, and a pending rollup for a
	// key deleted before the flush is dropped, not orphaned.
	s.CheckRequest(token, "feed", "GET", "/api/posts")
	if _, err := s.Delete(k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := usageRows(t, s); n != 0 {
		t.Errorf("usage rows after delete = %d, want 0", n)
	}
}

// Close writes what is still pending.
func TestCloseFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := s.Mint("a", "feed", ScopeRead, time.Time{})
	s.CheckRequest(token, "feed", "GET", "/api/posts")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := usageRows(t, s); n != 1 {
		t.Errorf("usage rows after Close = %d, want 1", n)
	}
}

func TestRotate(t *testing.T) {
	s := openTest(t)
	exp := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	oldToken, old, _ := s.Mint("ci", "blobs", ScopeUpload, exp)
	token, k, err := s.Rotate(old.ID)
	if err != nil || k == nil {
		t.Fatalf("Rotate = %v, %v", k, err)
	}
	if k.ID == old.ID || k.Name != "ci" || k.App != "blobs" || k.Scope != ScopeUpload ||
		k.ExpiresAt != exp.Format(time.RFC3339) {
		t.Errorf("rotated key = %+v", k)
	}
	if scope, ok := s.Check(token, "blobs"); !ok || scope != ScopeUpload {
		t.Error("rotated token does not check")
	}
	if _, ok := s.Check(oldToken, "blobs"); ok {
		t.Error("old token still accepted after rotate")
	}
	if _, _, err := s.Rotate(old.ID); err != ErrNotActive {
		t.Errorf("rotating a revoked key = %v, want ErrNotActive", err)
	}
	if _, k, err := s.Rotate("missing"); k != nil || err != nil {
		t.Errorf("rotating unknown id = %v, %v", k, err)
	}
}

func TestRenameAndExpiry(t *testing.T) {
	s := openTest(t)
	token, k, _ := s.Mint("a", "feed", ScopeRead, time.Now().Add(-time.Hour))
	if ok, err := s.Rename(k.ID, "  b  "); !ok || err != nil {
		t.Fatalf("Rename = %v, %v", ok, err)
	}
	if _, err := s.Rename(k.ID, " "); err == nil {
		t.Error("blank name accepted")
	}
	if _, ok := s.Check(token, "feed"); ok {
		t.Fatal("expired key accepted")
	}
	if ok, err := s.SetExpiry(k.ID, time.Time{}); !ok || err != nil {
		t.Fatalf("SetExpiry = %v, %v", ok, err)
	}
	got, _ := s.Get(k.ID)
	if got.Name != "b" || got.ExpiresAt != "" {
		t.Errorf("after rename + clear expiry: %+v", got)
	}
	if _, ok := s.Check(token, "feed"); !ok {
		t.Error("clearing the expiry did not revive the key")
	}
	if g, _ := s.Get("missing"); g != nil {
		t.Error("Get(unknown) returned a key")
	}
}
