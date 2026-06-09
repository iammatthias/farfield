package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
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
	db, err := openDB(filepath.Join(t.TempDir(), "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bs, err := OpenLocalDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		db:        db,
		store:     bs,
		auth:      &web.Auth{DB: db, Password: "pw", APIKey: "secret"},
		rd:        &web.Renderer{Templates: tmpl, AssetVer: "test"},
		maxUpload: defaultMaxUpload,
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

	// A live admin session so RequireSession lets the upload through.
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

func TestUploadFileEndpoint(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	data := buildEPUB(t, "Solo Mission", "Far Field", nil)

	// Without a session, the endpoint is not usable (requireSession redirects).
	req := httptest.NewRequest(http.MethodPost, "/upload/file?filename=solo.epub", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		t.Errorf("unauthenticated /upload/file returned 201, want a redirect/denial")
	}

	// With a session, one EPUB stores and returns the book JSON.
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/upload/file?filename=solo.epub", bytes.NewReader(data))
	req.AddCookie(auth.SessionCookie(token, false))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var book Book
	if err := json.Unmarshal(rec.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Title != "Solo Mission" || book.CID != cid.Of(data) {
		t.Errorf("book = %+v", book)
	}
}

func TestEnsureCollectionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")

	// Simulate a database created before the collection column existed.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE books (
		cid TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '',
		language TEXT NOT NULL DEFAULT '', identifier TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '', filename TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL, cover_cid TEXT NOT NULL DEFAULT '',
		cover_mime TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO books (cid, title, size, created_at) VALUES ('bafold', 'Old Book', 10, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// openDB must migrate it: add the column, read the old row cleanly.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB migrate: %v", err)
	}
	defer db.Close()

	b, err := getBook(db, "bafold")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Title != "Old Book" || b.Collection != "" {
		t.Fatalf("migrated book = %+v", b)
	}
	if _, err := updateBookCollection(db, "bafold", "Archive"); err != nil {
		t.Fatal(err)
	}
	if b, _ = getBook(db, "bafold"); b.Collection != "Archive" {
		t.Errorf("collection after update = %q, want Archive", b.Collection)
	}
}

func TestBulkCollection(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Upload two uncategorized books via the API.
	var cids []string
	for _, title := range []string{"Alpha", "Beta"} {
		data := buildEPUB(t, title, "Author", nil)
		cids = append(cids, cid.Of(data))
		req := httptest.NewRequest(http.MethodPost, "/api/books?filename="+title+".epub", bytes.NewReader(data))
		req.Header.Set("X-API-Key", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d", title, rec.Code)
		}
	}

	// Bulk-move both into a new "Reading" folder.
	form := url.Values{"collection": {"Reading"}, "next": {"/?collection=Reading"}, "cid": cids}
	req := httptest.NewRequest(http.MethodPost, "/books/collection", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(auth.SessionCookie(token, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?collection=Reading" {
		t.Fatalf("bulk move: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	named, _, err := collectionStats(s.db)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, c := range named {
		if c.Name == "Reading" {
			got = c.Count
		}
	}
	if got != 2 {
		t.Errorf("Reading folder count = %d, want 2 (%+v)", got, named)
	}

	// Without a session the bulk endpoint is denied (redirected to login).
	req = httptest.NewRequest(http.MethodPost, "/books/collection", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Location") == "/?collection=Reading" {
		t.Error("unauthenticated bulk move should not be processed")
	}
}

func TestFileManagerIndex(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	upload := func(title, coll string) {
		data := buildEPUB(t, title, "A", nil)
		u := "/api/books?filename=" + title + ".epub"
		if coll != "" {
			u += "&collection=" + url.QueryEscape(coll)
		}
		req := httptest.NewRequest(http.MethodPost, u, bytes.NewReader(data))
		req.Header.Set("X-API-Key", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d", title, rec.Code)
		}
	}
	upload("Dune", "Sci-Fi")
	upload("Loose", "")

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(auth.SessionCookie(token, false))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	// Root: a folder row for Sci-Fi and the loose book, but not the foldered one.
	root := get("/")
	for _, want := range []string{`class="folder-row"`, "/?collection=Sci-Fi", "Sci-Fi", "Loose"} {
		if !strings.Contains(root, want) {
			t.Errorf("root missing %q", want)
		}
	}
	if strings.Contains(root, "Dune") {
		t.Error("root should not list the foldered book Dune")
	}

	// Inside the folder: the foldered book + a breadcrumb back, not the loose book.
	inside := get("/?collection=Sci-Fi")
	if !strings.Contains(inside, "Dune") || !strings.Contains(inside, `href="/"`) {
		t.Error("folder view should show Dune and a back link")
	}
	if strings.Contains(inside, "Loose") {
		t.Error("folder view should not list the loose book")
	}
}

func TestCollections(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	upload := func(title, collection string) {
		data := buildEPUB(t, title, "Author", nil)
		u := "/api/books?filename=" + url.QueryEscape(title) + ".epub"
		if collection != "" {
			u += "&collection=" + url.QueryEscape(collection)
		}
		req := httptest.NewRequest(http.MethodPost, u, bytes.NewReader(data))
		req.Header.Set("X-API-Key", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %q: %d %s", title, rec.Code, rec.Body)
		}
	}
	upload("Dune", "Sci-Fi")
	upload("Neuromancer", "Sci-Fi")
	upload("Cookbook", "") // uncategorized

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("x", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	// Navigation root: All books, the Sci-Fi folder, and Uncategorized.
	nav := get("/opds")
	for _, want := range []string{
		"kind=navigation", "<title>All books</title>",
		"<title>Sci-Fi</title>", "/opds/collection?c=Sci-Fi",
		"<title>Uncategorized</title>",
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("nav feed missing %q\n%s", want, nav)
		}
	}

	// The Sci-Fi feed holds exactly its two books, not the uncategorized one.
	sci := get("/opds/collection?c=Sci-Fi")
	if !strings.Contains(sci, "<title>Dune</title>") || !strings.Contains(sci, "<title>Neuromancer</title>") {
		t.Errorf("Sci-Fi feed missing its books\n%s", sci)
	}
	if strings.Contains(sci, "<title>Cookbook</title>") {
		t.Errorf("Sci-Fi feed leaked the uncategorized book")
	}

	// Uncategorized feed holds only the loose book.
	unc := get("/opds/collection?c=")
	if !strings.Contains(unc, "<title>Cookbook</title>") || strings.Contains(unc, "<title>Dune</title>") {
		t.Errorf("uncategorized feed wrong\n%s", unc)
	}

	// All feed has everything.
	all := get("/opds/all")
	for _, want := range []string{"Dune", "Neuromancer", "Cookbook"} {
		if !strings.Contains(all, "<title>"+want+"</title>") {
			t.Errorf("all feed missing %s", want)
		}
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

	// 4a. The navigation root (with auth) lists folders, not books.
	req = httptest.NewRequest(http.MethodGet, "/opds", nil)
	req.SetBasicAuth("reader", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nav status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != navFeedType {
		t.Errorf("nav content-type = %q, want %q", ct, navFeedType)
	}
	if nav := rec.Body.String(); !strings.Contains(nav, "<title>All books</title>") || !strings.Contains(nav, "/opds/all") {
		t.Errorf("nav feed missing the All books folder\n%s", nav)
	}

	// 4b. The acquisition feed (/opds/all) lists the book with its links.
	req = httptest.NewRequest(http.MethodGet, "/opds/all", nil)
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
	if status.Service != "library" || !status.OK || status.Books != 1 {
		t.Errorf("status = %+v", status)
	}
}
