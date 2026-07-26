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
