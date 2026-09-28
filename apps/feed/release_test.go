package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// fakeBlobs stores uploads (failing on the Nth) and records release deletes.
type fakeBlobs struct {
	mu       sync.Mutex
	uploads  int
	failOn   int
	released []string
}

func (f *fakeBlobs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		f.uploads++
		if f.uploads == f.failOn {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// Base32 only (a–z, 2–7), like a real CID.
		fmt.Fprintf(w, `{"cid":"bafkreiupload%s%c"}`, strings.Repeat("q", 20), rune('a'+f.uploads))
	case http.MethodDelete:
		if r.URL.Query().Get("unlessReferenced") == "" {
			t := "unguarded"
			f.released = append(f.released, t)
		}
		f.released = append(f.released, strings.TrimPrefix(r.URL.Path, "/blobs/"))
		w.Write([]byte(`{"deleted":true}`))
	}
}

func (f *fakeBlobs) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := append([]string{}, f.released...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

func releaseServer(t *testing.T, fb *fakeBlobs) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(fb)
	t.Cleanup(up.Close)
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, auth: &web.Auth{DB: db, APIKey: "write"}, blobsURL: up.URL, blobsKey: "bk"}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv
}

func mediaPost(t *testing.T, srv *httptest.Server, files int) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("body", "caption")
	for i := 0; i < files; i++ {
		fw, _ := mw.CreateFormFile("file", fmt.Sprintf("p%d.jpg", i))
		fw.Write([]byte("jpeg bytes"))
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/posts/media", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-API-Key", "write")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Slug string `json:"slug"`
	}
	_ = jsonDecode(resp, &out)
	return resp, out.Slug
}

// A media post that fails partway releases the photos it already stored.
func TestFailedMediaPostReleasesItsPhotos(t *testing.T) {
	fb := &fakeBlobs{failOn: 3}
	srv := releaseServer(t, fb)
	resp, _ := mediaPost(t, srv, 3)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	got := fb.wait(t, 2)
	if len(got) != 2 || strings.Contains(strings.Join(got, ","), "unguarded") {
		t.Errorf("released = %v, want the 2 stored photos, guarded", got)
	}
}

// DELETE ?media=release lets the post's photos go; a plain DELETE never does.
func TestDeleteReleasesMediaOnlyWhenAsked(t *testing.T) {
	fb := &fakeBlobs{}
	srv := releaseServer(t, fb)
	_, slug := mediaPost(t, srv, 2)
	_, keep := mediaPost(t, srv, 1)

	del := func(slug, q string) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/posts/"+slug+q, nil)
		req.Header.Set("X-API-Key", "write")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("delete %s%s: %v %v", slug, q, err, resp.StatusCode)
		}
	}
	del(keep, "")
	time.Sleep(100 * time.Millisecond)
	if got := fb.wait(t, 0); len(got) != 0 {
		t.Fatalf("plain delete released %v", got)
	}
	del(slug, "?media=release")
	if got := fb.wait(t, 2); len(got) != 2 {
		t.Errorf("released = %v, want the post's 2 photos", got)
	}
}

func jsonDecode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
