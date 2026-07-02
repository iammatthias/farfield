package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeKeys implements KeyChecker with a fixed token→(app, scope) table.
type fakeKeys map[string]struct{ app, scope string }

func (f fakeKeys) Check(token, app string) (string, bool) {
	k, ok := f[token]
	if !ok || (k.app != "*" && k.app != app) {
		return "", false
	}
	return k.scope, true
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest("GET", "/api/x", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAdminKeysAugmentGates(t *testing.T) {
	a := &Auth{
		APIKey:  "env-write",
		ReadKey: "env-read",
		App:     "feed",
		Keys: fakeKeys{
			"ffk_w": {"feed", "write"},
			"ffk_r": {"feed", "read"},
			"ffk_u": {"feed", "upload"},
			"ffk_o": {"blobs", "write"},
			"ffk_s": {"*", "read"},
		},
	}

	// Env keys keep working unchanged.
	if !a.HasWriteKey(bearer("env-write")) || !a.HasReadKey(bearer("env-read")) {
		t.Fatal("env keys stopped working with a key store attached")
	}

	// DB write scope unlocks writes and reads; read scope only reads.
	if !a.HasWriteKey(bearer("ffk_w")) {
		t.Error("db write key rejected as write")
	}
	if !a.HasReadKey(bearer("ffk_w")) {
		t.Error("db write key rejected as read")
	}
	if a.HasWriteKey(bearer("ffk_r")) {
		t.Error("db read key accepted as write")
	}
	if !a.HasReadKey(bearer("ffk_r")) {
		t.Error("db read key rejected as read")
	}

	// Upload scope is not read: it opens only the endpoints that ask for it.
	if a.HasReadKey(bearer("ffk_u")) || a.HasWriteKey(bearer("ffk_u")) {
		t.Error("upload-scoped key leaked into read/write gates")
	}

	// A key for another app is rejected; a wildcard key is accepted.
	if a.HasReadKey(bearer("ffk_o")) {
		t.Error("key scoped to blobs accepted on feed")
	}
	if !a.HasReadKey(bearer("ffk_s")) {
		t.Error("wildcard read key rejected")
	}
}

func TestRequireAPIKeyWithOnlyKeyStore(t *testing.T) {
	// No env write key at all: DB write keys still work, others 401, and the
	// gate is not the 503 "unconfigured" case.
	a := &Auth{App: "feed", Keys: fakeKeys{"ffk_w": {"feed", "write"}}}
	next := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }

	w := httptest.NewRecorder()
	a.RequireAPIKey(next)(w, bearer("ffk_w"))
	if w.Code != http.StatusNoContent {
		t.Errorf("db write key → %d, want 204", w.Code)
	}

	w = httptest.NewRecorder()
	a.RequireAPIKey(next)(w, bearer("ffk_nope"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad key → %d, want 401", w.Code)
	}

	// Neither env key nor store: still fails closed with 503.
	w = httptest.NewRecorder()
	(&Auth{}).RequireAPIKey(next)(w, bearer("anything"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured → %d, want 503", w.Code)
	}
}
