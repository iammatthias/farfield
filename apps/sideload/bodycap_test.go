package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

// TestUploadRoutesAreNotBodyCapped pins the routes that legitimately stream.
//
// The fleet-wide 2 MiB cap is right for a form or a JSON document and wrong
// for an .ipa: maxIPABytes is 2 GiB. POST /api/builds is the agent API's
// upload — the path the sideload skill uses — and it does not start with
// /upload, so an incomplete prefix list would have capped a 2 GiB archive at
// two megabytes and broken every scripted build push.
func TestUploadRoutesAreNotBodyCapped(t *testing.T) {
	skip := web.PathPrefixSkipper("/upload", "/app", "/api/builds")
	for _, path := range []string{
		"/upload",                             // admin form upload
		"/api/builds",                         // agent API .ipa upload
		"/app/systems.farfield.x/screenshots", // screenshot upload
	} {
		req, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(""))
		if !skip(req) {
			t.Errorf("POST %s would be body-capped, but it streams", path)
		}
	}
	// Everything else should be capped.
	for _, path := range []string{"/b/abc/notes", "/shares/tok/revoke", "/register/tok/submit"} {
		req, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(""))
		if skip(req) {
			t.Errorf("POST %s escapes the body cap but carries only a small form", path)
		}
	}
}
