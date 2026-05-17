package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesIndex(t *testing.T) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	rec := httptest.NewRecorder()
	http.FileServerFS(site).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Error("index page contains no <svg> element")
	}
}
