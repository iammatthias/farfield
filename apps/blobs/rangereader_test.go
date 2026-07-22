package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// countingFetcher hands out readers over a byte slice and counts how many
// ranged fetches a rangeReader actually issues.
type countingFetcher struct {
	data  []byte
	calls int
}

func (c *countingFetcher) fetch(off int64) (io.ReadCloser, error) {
	c.calls++
	if off > int64(len(c.data)) {
		off = int64(len(c.data))
	}
	return io.NopCloser(bytes.NewReader(c.data[off:])), nil
}

// newTestRangeReader mirrors R2.GetStream: the initial fetch is already open
// when the reader is handed out.
func newTestRangeReader(data []byte) (*rangeReader, *countingFetcher) {
	f := &countingFetcher{data: data}
	body, _ := f.fetch(0)
	return &rangeReader{fetch: f.fetch, size: int64(len(data)), body: body}, f
}

func testBytes(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

func TestRangeReaderSequentialReusesInitialBody(t *testing.T) {
	data := testBytes(1000)
	rr, f := newTestRangeReader(data)
	defer rr.Close()
	got, err := io.ReadAll(rr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("read %d bytes, want %d matching", len(got), len(data))
	}
	if f.calls != 1 {
		t.Errorf("sequential read issued %d fetches, want 1", f.calls)
	}
}

// ServeContent probes the size by seeking to the end and back before it
// reads; that dance must not abandon the already-open initial stream.
func TestRangeReaderSeekDanceReusesInitialBody(t *testing.T) {
	data := testBytes(1000)
	rr, f := newTestRangeReader(data)
	defer rr.Close()
	if n, err := rr.Seek(0, io.SeekEnd); err != nil || n != 1000 {
		t.Fatalf("Seek end = %d, %v; want 1000, nil", n, err)
	}
	if n, err := rr.Seek(0, io.SeekStart); err != nil || n != 0 {
		t.Fatalf("Seek start = %d, %v; want 0, nil", n, err)
	}
	got, err := io.ReadAll(rr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("post-dance read mismatch")
	}
	if f.calls != 1 {
		t.Errorf("seek dance issued %d fetches, want 1", f.calls)
	}
}

func TestRangeReaderMidSeekRefetches(t *testing.T) {
	data := testBytes(1000)
	rr, f := newTestRangeReader(data)
	defer rr.Close()
	if _, err := rr.Seek(600, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(rr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data[600:]) {
		t.Errorf("read from offset 600 mismatch")
	}
	if f.calls != 2 {
		t.Errorf("mid-file seek issued %d fetches, want 2 (initial + ranged)", f.calls)
	}
}

func TestRangeReaderServeContent(t *testing.T) {
	data := testBytes(4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr, _ := newTestRangeReader(data)
		defer rr.Close()
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "", time.Time{}, rr)
	}))
	defer srv.Close()

	get := func(rangeHdr string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET (%q): %v", rangeHdr, err)
		}
		return resp
	}

	// Plain GET — the whole object.
	resp := get("")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Errorf("plain GET = %d, %d bytes; want 200 with full body", resp.StatusCode, len(body))
	}

	// A mid-file range — what a scrubbing player sends.
	resp = get("bytes=1000-1999")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("ranged GET = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, data[1000:2000]) {
		t.Errorf("ranged GET returned wrong slice (%d bytes)", len(body))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 1000-1999/4096" {
		t.Errorf("Content-Range = %q, want bytes 1000-1999/4096", cr)
	}

	// Safari's playability probe — two bytes, expects a 206.
	resp = get("bytes=0-1")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 2 {
		t.Errorf("probe = %d with %d bytes, want 206 with 2", resp.StatusCode, len(body))
	}
}

// rangedStubStore is an R2-shaped ByteStore: its streams are rangeReaders,
// never *os.File — proving the byte handler serves 206s through the reader
// rather than through local-file luck.
type rangedStubStore struct{ data map[string][]byte }

func (s *rangedStubStore) Put(key string, data []byte, _ string) error {
	s.data[key] = data
	return nil
}
func (s *rangedStubStore) PutFile(key, path, ct string) error { return fmt.Errorf("unused") }
func (s *rangedStubStore) Get(key string) ([]byte, error)     { return s.data[key], nil }
func (s *rangedStubStore) Delete(key string) error            { delete(s.data, key); return nil }
func (s *rangedStubStore) List() ([]ObjectInfo, error)        { return nil, nil }
func (s *rangedStubStore) GetStream(key string) (io.ReadCloser, int64, error) {
	data, ok := s.data[key]
	if !ok {
		return nil, 0, nil
	}
	f := &countingFetcher{data: data}
	body, _ := f.fetch(0)
	return &rangeReader{fetch: f.fetch, size: int64(len(data)), body: body}, int64(len(data)), nil
}

func TestByteHandlerServesRangesFromUnseekableStore(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "blobs.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	data := testBytes(8192)
	cid := BlobCID(data)
	if err := upsertMeta(db, &Meta{CID: cid, Size: int64(len(data)), Mime: "video/mp4",
		CreatedAt: "2026-07-22T00:00:00Z"}); err != nil {
		t.Fatalf("upsertMeta: %v", err)
	}

	s := &Server{
		db:    db,
		store: &rangedStubStore{data: map[string][]byte{cid: data}},
		auth:  &web.Auth{DB: db},
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/blobs/"+cid, nil)
	req.Header.Set("Range", "bytes=4096-4127")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, data[4096:4128]) {
		t.Errorf("ranged GET returned wrong bytes")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 4096-4127/8192" {
		t.Errorf("Content-Range = %q", cr)
	}
}
