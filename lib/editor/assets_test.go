package editor

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func getAsset(t *testing.T, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Handler("/static/editor/").ServeHTTP(rec, req)
	return rec
}

// The text assets go out precompressed to a gzip client and plain to anyone
// else, both decoding to the embedded bytes, both marked Vary.
func TestHandlerServesPrecompressedText(t *testing.T) {
	for name, want := range map[string][]byte{
		"host.js":        []byte(HostJS),
		"host.css":       []byte(HostCSS),
		"mount.js":       []byte(MountJS),
		"dict/en_US.txt": Dictionary,
	} {
		gz := getAsset(t, "/static/editor/"+name, map[string]string{"Accept-Encoding": "gzip, deflate, br"})
		if gz.Code != http.StatusOK || gz.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: got %d, Content-Encoding %q; want 200 gzip", name, gz.Code, gz.Header().Get("Content-Encoding"))
		}
		if gz.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: Vary = %q", name, gz.Header().Get("Vary"))
		}
		if gz.Header().Get("Content-Length") != strconv.Itoa(gz.Body.Len()) {
			t.Errorf("%s: Content-Length %q for %d bytes", name, gz.Header().Get("Content-Length"), gz.Body.Len())
		}
		zr, err := gzip.NewReader(gz.Body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := io.ReadAll(zr)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: gzip body decodes to %d bytes, want %d", name, len(got), len(want))
		}

		plain := getAsset(t, "/static/editor/"+name, nil)
		if plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), want) {
			t.Errorf("%s: plain response encoded or altered", name)
		}
		if plain.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: plain Vary = %q", name, plain.Header().Get("Vary"))
		}
		if plain.Header().Get("ETag") == gz.Header().Get("ETag") {
			t.Errorf("%s: both encodings share strong ETag %s", name, gz.Header().Get("ETag"))
		}
	}
}

// Every asset revalidates against the editor version, in either encoding, and
// the ?v= rule for Cache-Control is unchanged.
func TestHandlerRevalidates(t *testing.T) {
	v := Version()
	for _, name := range []string{"host.js", "dict/en_US.txt", "editor.wasm", "fonts/" + FontSlots[0]} {
		first := getAsset(t, "/static/editor/"+name, map[string]string{"Accept-Encoding": "gzip"})
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", name)
		}
		for _, inm := range []string{etag, "W/" + etag, `"` + v + `"`} {
			rec := getAsset(t, "/static/editor/"+name, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": inm})
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
				t.Errorf("%s If-None-Match %s: got %d with %d bytes, want 304", name, inm, rec.Code, rec.Body.Len())
			}
		}
		stale := getAsset(t, "/static/editor/"+name, map[string]string{"If-None-Match": `"0000000000000000"`})
		if stale.Code != http.StatusOK {
			t.Errorf("%s: stale tag got %d, want 200", name, stale.Code)
		}
	}

	if cc := getAsset(t, "/static/editor/host.js?v="+v, nil).Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("current ?v= Cache-Control = %q", cc)
	}
	if cc := getAsset(t, "/static/editor/host.js?v=old", nil).Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("other ?v= Cache-Control = %q", cc)
	}
	if cc := getAsset(t, "/static/editor/host.js", map[string]string{"If-None-Match": `"` + v + `"`}).Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("304 Cache-Control = %q", cc)
	}
}
