package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/bytestore"
	"github.com/iammatthias/farfield/lib/r2"
	"github.com/iammatthias/farfield/lib/web"
)

// The rangeReader's own behavior — fetch counting, the seek dance, ServeContent
// over a non-seekable backend — is tested in lib/r2, where it lives. What
// belongs here is the byte handler: that blobs answers Range requests through
// whatever the store hands it, rather than through local-file luck.

func testBytes(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

// rangedStubStore is an R2-shaped ByteStore: its streams are range-fetching
// readers, never *os.File.
type rangedStubStore struct{ data map[string][]byte }

func (s *rangedStubStore) Put(key string, data []byte, _ string) error {
	s.data[key] = data
	return nil
}
func (s *rangedStubStore) PutFile(key, path, ct string) error { return fmt.Errorf("unused") }
func (s *rangedStubStore) Get(key string) ([]byte, error)     { return s.data[key], nil }
func (s *rangedStubStore) Delete(key string) error            { delete(s.data, key); return nil }
func (s *rangedStubStore) List() ([]bytestore.ObjectInfo, error) {
	return nil, nil
}
func (s *rangedStubStore) PutSeeker(key string, rs io.ReadSeeker, _ string) error {
	b, err := io.ReadAll(rs)
	if err != nil {
		return err
	}
	s.data[key] = b
	return nil
}
func (s *rangedStubStore) GetStream(key string) (io.ReadCloser, int64, error) {
	data, ok := s.data[key]
	if !ok {
		return nil, 0, nil
	}
	fetch := func(off int64) (io.ReadCloser, error) {
		if off > int64(len(data)) {
			off = int64(len(data))
		}
		return io.NopCloser(bytes.NewReader(data[off:])), nil
	}
	body, _ := fetch(0)
	return r2.NewRangeReader(int64(len(data)), body, fetch), int64(len(data)), nil
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
