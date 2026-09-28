package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/iammatthias/farfield/lib/bytestore"
	"github.com/iammatthias/farfield/lib/web"
)

// ?unlessReferenced=1 deletes an orphan, keeps a referenced blob, and keeps
// everything when a reference source cannot be read.
func TestGuardedDelete(t *testing.T) {
	content, feed := fakeContent(t), fakeFeed(t)
	defer content.Close()
	defer feed.Close()
	s := newHygieneServer(t, content.URL, feed.URL)
	bs, err := bytestore.OpenLocalDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.store = bs
	s.auth = &web.Auth{DB: s.db, APIKey: "k"}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	del := func(cid string) int {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/blobs/"+cid+"?unlessReferenced=1", nil)
		req.Header.Set("X-API-Key", "k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := del(cidA); code != http.StatusConflict {
		t.Errorf("referenced blob: %d, want 409", code)
	}
	if m, _ := getMeta(s.db, cidA); m == nil {
		t.Error("referenced blob was deleted")
	}
	if code := del(cidB); code != http.StatusOK {
		t.Errorf("orphan: %d, want 200", code)
	}
	if m, _ := getMeta(s.db, cidB); m != nil {
		t.Error("orphan survived")
	}

	// A source that cannot be read means nothing is provably unreferenced.
	content.Close()
	if code := del(cidC); code != http.StatusConflict {
		t.Errorf("with content down: %d, want 409 (kept)", code)
	}
	if m, _ := getMeta(s.db, cidC); m == nil {
		t.Error("deleted while blind to a source")
	}
}

func TestPurgeCloudflare(t *testing.T) {
	var mu sync.Mutex
	var got struct {
		Files []string `json:"files"`
	}
	var auth, path string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"success":true}`))
	}))
	defer stub.Close()

	err := purgeCloudflare(t.Context(), stub.URL, "zone1", "tok", []string{"https://blobs.example/blobs/x"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || path != "/zones/zone1/purge_cache" || len(got.Files) != 1 {
		t.Errorf("auth=%q path=%q files=%v", auth, path, got.Files)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"success":false,"errors":[{"message":"no permission"}]}`))
	}))
	defer fail.Close()
	if err := purgeCloudflare(t.Context(), fail.URL, "z", "t", []string{"u"}); err == nil || !strings.Contains(err.Error(), "no permission") {
		t.Errorf("err = %v", err)
	}
}
