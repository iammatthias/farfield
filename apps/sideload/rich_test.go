package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 6, 4))
	for x := 0; x < 6; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{20, 20, 20, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestImageInfo(t *testing.T) {
	mime, ext, w, h, err := imageInfo(testPNG(t))
	if err != nil {
		t.Fatalf("imageInfo: %v", err)
	}
	if mime != "image/png" || ext != ".png" || w != 6 || h != 4 {
		t.Errorf("imageInfo = %q %q %dx%d", mime, ext, w, h)
	}
	if _, _, _, _, err := imageInfo([]byte("not an image")); err == nil {
		t.Error("expected non-image to be rejected")
	}
}

func TestRenderMarkdown(t *testing.T) {
	if got := renderMarkdown(""); got != "" {
		t.Errorf("empty markdown = %q, want empty", got)
	}
	got := string(renderMarkdown("**bold** and a list:\n\n- one\n- two"))
	if !strings.Contains(got, "<strong>bold</strong>") || !strings.Contains(got, "<li>one</li>") {
		t.Errorf("markdown render = %q", got)
	}
	// Raw HTML is neutralised (goldmark's safe default).
	if strings.Contains(string(renderMarkdown("<script>alert(1)</script>")), "<script>") {
		t.Error("raw HTML should not pass through")
	}
}

func TestAppMetaCRUD(t *testing.T) {
	s := testServer(t)
	if m, _ := getAppMeta(s.db, "com.x"); m != nil {
		t.Fatal("expected no meta initially")
	}
	if err := upsertAppMeta(s.db, &AppMeta{BundleID: "com.x", Tagline: "Tiny", Description: "# Hi"}); err != nil {
		t.Fatal(err)
	}
	m, err := getAppMeta(s.db, "com.x")
	if err != nil || m == nil || m.Tagline != "Tiny" || m.Description != "# Hi" {
		t.Fatalf("meta = %+v err %v", m, err)
	}
	// Clearing both fields removes the row (back to the plain view).
	if err := upsertAppMeta(s.db, &AppMeta{BundleID: "com.x"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := getAppMeta(s.db, "com.x"); m != nil {
		t.Error("clearing meta should delete the row")
	}
}

func TestScreenshotOrderingAndDedup(t *testing.T) {
	s := testServer(t)
	mk := func(id, cid string) {
		if err := addScreenshot(s.db, &Screenshot{ID: id, BundleID: "com.x", CID: cid, Ext: ".png"}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "cidA")
	mk("b", "cidB")
	mk("c", "cidA") // same bytes as a — shared file

	order := func() string {
		shots, _ := listScreenshots(s.db, "com.x")
		ids := make([]string, len(shots))
		for i, sh := range shots {
			ids[i] = sh.ID
		}
		return strings.Join(ids, ",")
	}
	if order() != "a,b,c" {
		t.Fatalf("initial order = %q", order())
	}
	if err := moveScreenshot(s.db, "c", "up"); err != nil {
		t.Fatal(err)
	}
	if order() != "a,c,b" {
		t.Errorf("after move c up = %q, want a,c,b", order())
	}
	// Two rows share cidA, so deleting one must not orphan the other's bytes.
	if n, _ := screenshotsWithCID(s.db, "cidA"); n != 2 {
		t.Errorf("cidA refcount = %d, want 2", n)
	}
	if _, err := deleteScreenshot(s.db, "a"); err != nil {
		t.Fatal(err)
	}
	if n, _ := screenshotsWithCID(s.db, "cidA"); n != 1 {
		t.Errorf("cidA refcount after delete = %d, want 1", n)
	}
}

func TestRichPageFlow(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cookie := sessionCookie(t, s)

	// An app must exist (one build) for its page to render.
	ipa := makeIPA(t, "com.rich.app", "Rich", "1.0", "1", time.Now().Add(200*24*time.Hour), nil)
	b, err := s.ingest(bytes.NewReader(ipa), "rich.ipa", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Backwards compatible: the page renders before any rich content is set.
	if _, code := getBody(t, srv.Client(), srv.URL+"/app/com.rich.app", cookie); code != http.StatusOK {
		t.Fatalf("plain app page = %d", code)
	}

	// Save metadata.
	form := strings.NewReader("tagline=Pocket+ledger&description=" + "%23+About%0A%0ATracks+things.")
	req, _ := http.NewRequest("POST", srv.URL+"/app/com.rich.app/meta", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	if resp, _ := noRedirect.Do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("meta save = %d", resp.StatusCode)
	}

	// Upload a screenshot (multipart).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("image", "home.png")
	fw.Write(testPNG(t))
	mw.WriteField("caption", "Home screen")
	mw.Close()
	ureq, _ := http.NewRequest("POST", srv.URL+"/app/com.rich.app/screenshots", &buf)
	ureq.Header.Set("Content-Type", mw.FormDataContentType())
	ureq.AddCookie(cookie)
	if resp, _ := noRedirect.Do(ureq); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("screenshot upload = %d", resp.StatusCode)
	}

	shots, _ := listScreenshots(s.db, "com.rich.app")
	if len(shots) != 1 {
		t.Fatalf("screenshots = %d, want 1", len(shots))
	}

	// The image serves publicly with the right type.
	imgBody, code := getRaw(t, srv.Client(), srv.URL+"/shots/"+shots[0].ID, nil)
	if code != http.StatusOK {
		t.Fatalf("/shots = %d", code)
	}
	if !bytes.Equal(imgBody, testPNG(t)) {
		t.Error("served screenshot bytes differ")
	}

	// The rich app page now shows the tagline, the rendered description, and the screenshot.
	body, code := getBody(t, srv.Client(), srv.URL+"/app/com.rich.app", cookie)
	if code != http.StatusOK {
		t.Fatalf("rich app page = %d", code)
	}
	for _, want := range []string{"Pocket ledger", "Tracks things.", "/shots/" + shots[0].ID} {
		if !strings.Contains(body, want) {
			t.Errorf("rich app page missing %q", want)
		}
	}

	// Changelog: set notes on the build, confirm they render on the app page.
	nf := strings.NewReader("notes=" + "First+release")
	nreq, _ := http.NewRequest("POST", srv.URL+"/b/"+b.ID+"/notes", nf)
	nreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nreq.AddCookie(cookie)
	if resp, _ := noRedirect.Do(nreq); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("notes save = %d", resp.StatusCode)
	}
	if body, _ := getBody(t, srv.Client(), srv.URL+"/app/com.rich.app", cookie); !strings.Contains(body, "First release") {
		t.Error("changelog not shown on app page")
	}
}
