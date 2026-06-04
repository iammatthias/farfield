package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
)

// buildEPUB assembles a minimal but valid EPUB in memory: the stored mimetype
// entry, META-INF/container.xml pointing at the OPF, and an OPF carrying the
// given Dublin Core metadata. When cover is non-empty a cover-image item and
// its PNG bytes are included.
func buildEPUB(t *testing.T, title, author string, cover []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// The EPUB spec wants an uncompressed "mimetype" entry first.
	mt, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mt.Write([]byte("application/epub+zip")); err != nil {
		t.Fatal(err)
	}

	write := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}

	write("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`)

	coverItem := ""
	if len(cover) > 0 {
		coverItem = `<item id="cover-img" href="cover.png" media-type="image/png" properties="cover-image"/>`
	}
	write("OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>en</dc:language>
    <dc:identifier id="bookid">urn:uuid:test-1234</dc:identifier>
    <dc:description>A test book.</dc:description>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    %s
  </manifest>
  <spine/>
</package>`, title, author, coverItem))

	if len(cover) > 0 {
		w, err := zw.Create("OEBPS/cover.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(cover); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "opds.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bs, err := OpenLocalDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		db:        db,
		store:     bs,
		templates: tmpl,
		password:  "pw",
		apiKey:    "secret",
		maxUpload: defaultMaxUpload,
		assetVer:  "test",
	}
}

func TestParseEPUB(t *testing.T) {
	cover := []byte("\x89PNG\r\n\x1a\nFAKECOVERBYTES")
	data := buildEPUB(t, "The Go Programming Language", "Donovan and Kernighan", cover)

	meta, gotCover, mime, err := parseEPUB(data)
	if err != nil {
		t.Fatalf("parseEPUB: %v", err)
	}
	if meta.Title != "The Go Programming Language" {
		t.Errorf("title = %q", meta.Title)
	}
	if meta.Author != "Donovan and Kernighan" {
		t.Errorf("author = %q", meta.Author)
	}
	if meta.Language != "en" {
		t.Errorf("language = %q", meta.Language)
	}
	if meta.Identifier != "urn:uuid:test-1234" {
		t.Errorf("identifier = %q", meta.Identifier)
	}
	if !bytes.Equal(gotCover, cover) {
		t.Errorf("cover bytes = %q, want %q", gotCover, cover)
	}
	if mime != "image/png" {
		t.Errorf("cover mime = %q, want image/png", mime)
	}

	// Non-EPUB bytes must be rejected — that error is the upload validation.
	if _, _, _, err := parseEPUB([]byte("not an epub")); err == nil {
		t.Error("parseEPUB accepted non-EPUB bytes")
	}
}

func TestBulkUpload(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	// A live admin session so requireSession lets the upload through.
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A multipart form carrying three EPUBs under one "file" field, plus one
	// non-EPUB that must be rejected without sinking the batch.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i, title := range []string{"Mission One", "Mission Two", "Mission Three"} {
		fw, err := mw.CreateFormFile("file", fmt.Sprintf("book-%d.epub", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(buildEPUB(t, title, "Far Field", nil)); err != nil {
			t.Fatal(err)
		}
	}
	bad, _ := mw.CreateFormFile("file", "not-a-book.epub")
	bad.Write([]byte("definitely not an epub"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(auth.SessionCookie(token, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bulk upload status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	// The three valid EPUBs are stored; the bad one is reported, not fatal.
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/upload?error=") || !strings.Contains(loc, "not-a-book.epub") {
		t.Errorf("redirect = %q, want an error mentioning the bad file", loc)
	}
	if n, err := countBooks(s.db); err != nil {
		t.Fatal(err)
	} else if n != 3 {
		t.Errorf("books after bulk upload = %d, want 3", n)
	}
}

func TestCatalogFlow(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	cover := []byte("\x89PNG\r\n\x1a\nFAKECOVERBYTES")
	data := buildEPUB(t, "Mission Handbook", "Far Field", cover)
	wantCID := cid.Of(data)

	// 1. Upload through the API key path.
	req := httptest.NewRequest(http.MethodPost, "/api/books?filename=handbook.epub", bytes.NewReader(data))
	req.Header.Set("X-API-Key", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body)
	}
	var book Book
	if err := json.Unmarshal(rec.Body.Bytes(), &book); err != nil {
		t.Fatalf("decode book: %v", err)
	}
	if book.CID != wantCID {
		t.Errorf("book cid = %q, want %q", book.CID, wantCID)
	}
	if book.Title != "Mission Handbook" || book.Author != "Far Field" {
		t.Errorf("book metadata = %+v", book)
	}
	if book.CoverCID == "" {
		t.Error("book has no cover cid")
	}

	// 2. Upload without the API key is refused.
	req = httptest.NewRequest(http.MethodPost, "/api/books", bytes.NewReader(data))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload status = %d, want 401", rec.Code)
	}

	// 3. The catalog refuses an unauthenticated reader with a Basic challenge.
	req = httptest.NewRequest(http.MethodGet, "/opds", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("catalog status without auth = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}

	// 4. With Basic Auth the catalog lists the book with an acquisition link.
	req = httptest.NewRequest(http.MethodGet, "/opds", nil)
	req.SetBasicAuth("reader", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != feedType {
		t.Errorf("catalog content-type = %q, want %q", ct, feedType)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`xmlns:dc="http://purl.org/dc/elements/1.1/"`,
		"<title>Mission Handbook</title>",
		"<dc:language>en</dc:language>",
		`rel="http://opds-spec.org/acquisition"`,
		"/opds/download/" + book.CID,
		"/opds/cover/" + book.CoverCID,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog body missing %q\n--- body ---\n%s", want, body)
		}
	}

	// 5. Download with Basic Auth returns the exact bytes — the CID round-trips.
	req = httptest.NewRequest(http.MethodGet, "/opds/download/"+book.CID, nil)
	req.SetBasicAuth("reader", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != epubMime {
		t.Errorf("download content-type = %q, want %q", ct, epubMime)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Errorf("downloaded bytes differ from upload (%d vs %d)", rec.Body.Len(), len(data))
	}
	if cid.Of(rec.Body.Bytes()) != wantCID {
		t.Error("downloaded bytes do not re-hash to the original CID")
	}

	// 6. The cover is served (to an authenticated reader) with its own bytes.
	req = httptest.NewRequest(http.MethodGet, "/opds/cover/"+book.CoverCID, nil)
	req.SetBasicAuth("reader", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cover status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("cover content-type = %q, want image/png", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), cover) {
		t.Errorf("cover bytes differ from the embedded cover")
	}

	// 7. Status reports the catalog size, unauthenticated.
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var status struct {
		Service string `json:"service"`
		OK      bool   `json:"ok"`
		Books   int    `json:"books"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Service != "opds" || !status.OK || status.Books != 1 {
		t.Errorf("status = %+v", status)
	}
}
