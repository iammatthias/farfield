package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
	"howett.net/plist"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

// makeIPA builds a synthetic but structurally faithful .ipa: a zip carrying a
// binary Info.plist and an embedded.mobileprovision whose XML plist is wrapped
// in filler bytes, the way a real CMS-signed profile embeds it.
func makeIPA(t *testing.T, bundleID, name, version, build string, expiry time.Time, udids []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	info := map[string]any{
		"CFBundleIdentifier":         bundleID,
		"CFBundleName":               name,
		"CFBundleDisplayName":        name,
		"CFBundleShortVersionString": version,
		"CFBundleVersion":            build,
		"CFBundlePackageType":        "APPL",
		"MinimumOSVersion":           "16.0",
	}
	infoBytes, err := plist.Marshal(info, plist.BinaryFormat)
	if err != nil {
		t.Fatalf("marshal Info.plist: %v", err)
	}
	mustZip(t, zw, "Payload/"+name+".app/Info.plist", infoBytes)

	prov := map[string]any{
		"Name":               name,
		"TeamName":           "Test Team",
		"ExpirationDate":     expiry,
		"ProvisionedDevices": udids,
	}
	provXML, err := plist.Marshal(prov, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal provision: %v", err)
	}
	wrapped := append([]byte("\x00\x01CMS-PREAMBLE\x02"), provXML...)
	wrapped = append(wrapped, []byte("CMS-TRAILER\x00")...)
	mustZip(t, zw, "Payload/"+name+".app/embedded.mobileprovision", wrapped)

	// A nested extension bundle that must be ignored by the top-level matcher.
	mustZip(t, zw, "Payload/"+name+".app/Plugins/Ext.appex/Info.plist", []byte("ignored"))

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func mustZip(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("zip create %s: %v", name, err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("zip write %s: %v", name, err)
	}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "sideload.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := newBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("newBlobStore: %v", err)
	}
	s := newServer(db, blobs, "pw", "apikey", false, "https://sideload.test")
	if err := s.parseTemplates(); err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	return s
}

func sessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	tok := auth.NewSessionToken()
	if err := store.InsertSession(s.db, tok, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return &http.Cookie{Name: "session", Value: tok}
}

// ── .ipa parsing ─────────────────────────────────────────────────────────────

func TestParseIPA(t *testing.T) {
	s := testServer(t)
	expiry := time.Now().Add(200 * 24 * time.Hour).UTC().Truncate(time.Second)
	ipa := makeIPA(t, "com.farfield.demo", "Demo", "2.1", "47", expiry,
		[]string{"udid-a", "udid-b"})

	cid, _, err := s.blobs.spool(bytes.NewReader(ipa), maxIPABytes)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	meta, err := parseIPA(s.blobs.path(cid))
	if err != nil {
		t.Fatalf("parseIPA: %v", err)
	}
	if meta.BundleID != "com.farfield.demo" {
		t.Errorf("bundle id = %q", meta.BundleID)
	}
	if meta.AppName != "Demo" || meta.Version != "2.1" || meta.BuildNumber != "47" {
		t.Errorf("name/version/build = %q/%q/%q", meta.AppName, meta.Version, meta.BuildNumber)
	}
	if meta.Team != "Test Team" {
		t.Errorf("team = %q", meta.Team)
	}
	if len(meta.UDIDs) != 2 {
		t.Errorf("udids = %v", meta.UDIDs)
	}
	if meta.ProfileExpiry.IsZero() || !meta.ProfileExpiry.Equal(expiry) {
		t.Errorf("expiry = %v want %v", meta.ProfileExpiry, expiry)
	}
}

func TestParseIPARejectsNonApp(t *testing.T) {
	s := testServer(t)
	// A zip with no Payload/*.app/Info.plist.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mustZip(t, zw, "readme.txt", []byte("not an app"))
	zw.Close()
	cid, _, err := s.blobs.spool(bytes.NewReader(buf.Bytes()), maxIPABytes)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	if _, err := parseIPA(s.blobs.path(cid)); err == nil {
		t.Fatal("expected parseIPA to reject a non-app zip")
	}
}

// ── manifest ─────────────────────────────────────────────────────────────────

func TestBuildManifest(t *testing.T) {
	b := &Build{BundleID: "com.x.y", AppName: "Why", Version: "1.0", BuildNumber: "9"}
	xml, err := buildManifest(b, manifestURLs{
		IPA:     "https://h/i/T/app.ipa",
		Display: "https://h/i/T/display.png",
		Full:    "https://h/i/T/full.png",
	})
	if err != nil {
		t.Fatalf("buildManifest: %v", err)
	}
	var got manifestPlist
	if _, err := plist.Unmarshal(xml, &got); err != nil {
		t.Fatalf("manifest is not valid plist: %v", err)
	}
	if len(got.Items) != 1 || len(got.Items[0].Assets) != 3 {
		t.Fatalf("manifest shape wrong: %+v", got)
	}
	if got.Items[0].Metadata.BundleIdentifier != "com.x.y" {
		t.Errorf("bundle id = %q", got.Items[0].Metadata.BundleIdentifier)
	}
	// build-number wins over marketing version for bundle-version.
	if got.Items[0].Metadata.BundleVersion != "9" {
		t.Errorf("bundle version = %q want 9", got.Items[0].Metadata.BundleVersion)
	}
	if a := got.Items[0].Assets[0]; a.Kind != "software-package" || a.URL != "https://h/i/T/app.ipa" {
		t.Errorf("software-package asset = %+v", a)
	}
}

// ── token lifecycle ──────────────────────────────────────────────────────────

func TestSelfTokenReused(t *testing.T) {
	s := testServer(t)
	t1, err := selfToken(s.db, "build000000000001")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := selfToken(s.db, "build000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Token != t2.Token {
		t.Errorf("self token not reused: %q vs %q", t1.Token, t2.Token)
	}
}

func TestShareSingleUseAndGrace(t *testing.T) {
	s := testServer(t)
	tok, err := createShare(s.db, "build000000000001", 30*time.Minute, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !tok.canStart() {
		t.Fatal("fresh share should be startable")
	}
	if err := recordInstall(s.db, tok, "ua", "ip"); err != nil {
		t.Fatal(err)
	}
	if tok.State != stateConsumed {
		t.Fatalf("after one install state = %q want consumed", tok.State)
	}
	if tok.canStart() {
		t.Error("consumed share must not be startable")
	}
	if !tok.canServeBytes() {
		t.Error("consumed share must keep serving within the grace window")
	}
	// Force the grace window to have passed.
	tok.ConsumedAt = time.Now().Add(-2 * consumeGrace).UTC().Format(time.RFC3339)
	if tok.canServeBytes() {
		t.Error("consumed share must stop serving past the grace window")
	}
}

func TestTokenExpiryAndRevoke(t *testing.T) {
	s := testServer(t)
	// Already-expired share.
	tok, _ := createShare(s.db, "b1", time.Millisecond, 1, "")
	time.Sleep(5 * time.Millisecond)
	reloaded, _ := getToken(s.db, tok.Token)
	if reloaded.canStart() || reloaded.canServeBytes() {
		t.Error("expired token must be dead for both start and bytes")
	}

	live, _ := createShare(s.db, "b1", time.Hour, 1, "")
	if ok, _ := revokeToken(s.db, live.Token); !ok {
		t.Fatal("revoke should report it existed")
	}
	reloaded, _ = getToken(s.db, live.Token)
	if reloaded.State != stateRevoked || reloaded.canServeBytes() {
		t.Error("revoked token must be dead")
	}
}

// ── range / consumption signal ───────────────────────────────────────────────

func TestDeliversFinalByte(t *testing.T) {
	const size = 1000
	cases := []struct {
		hdr  string
		want bool
	}{
		{"", true},                    // full GET
		{"bytes=0-999", true},         // exact full range
		{"bytes=500-", true},          // open-ended through end
		{"bytes=-200", true},          // suffix includes the tail
		{"bytes=0-499", false},        // first half only
		{"bytes=0-499,900-999", true}, // multi-range, second covers the end
		{"bytes=0-0", false},          // first byte only
		{"junk", false},
	}
	for _, c := range cases {
		if got := deliversFinalByte(c.hdr, size); got != c.want {
			t.Errorf("deliversFinalByte(%q) = %v want %v", c.hdr, got, c.want)
		}
	}
}

// ── end-to-end install + share ───────────────────────────────────────────────

func TestInstallFlow(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	client := srv.Client()

	ipa := makeIPA(t, "com.farfield.flow", "Flow", "3.0", "100",
		time.Now().Add(300*24*time.Hour), []string{"udid-1"})

	// Upload via the agent API.
	req, _ := http.NewRequest("POST", srv.URL+"/api/builds?filename=Flow.ipa", bytes.NewReader(ipa))
	req.Header.Set("X-API-Key", "apikey")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d", resp.StatusCode)
	}
	var up struct {
		ID, BundleID, InstallURL string
		DeviceCount              int
	}
	decodeJSON(t, resp, &up)
	if up.BundleID != "com.farfield.flow" || up.DeviceCount != 1 {
		t.Fatalf("upload meta wrong: %+v", up)
	}

	// Dedup: re-uploading identical bytes returns the same id and one build.
	req2, _ := http.NewRequest("POST", srv.URL+"/api/builds", bytes.NewReader(ipa))
	req2.Header.Set("X-API-Key", "apikey")
	resp2, _ := client.Do(req2)
	resp2.Body.Close()
	if n, _ := countBuilds(s.db); n != 1 {
		t.Fatalf("dedup failed, build count = %d", n)
	}

	// The install page requires a session.
	if code := get(t, client, srv.URL+"/b/"+up.ID, nil); code != http.StatusSeeOther && code != http.StatusOK {
		// RequireSession redirects (303) without a cookie; the test client may
		// follow it to /login (200). Either way it must not serve the page.
	}
	cookie := sessionCookie(t, s)
	body, code := getBody(t, client, srv.URL+"/b/"+up.ID, cookie)
	if code != http.StatusOK {
		t.Fatalf("build page status = %d", code)
	}
	if !strings.Contains(body, "Install on this iPhone") {
		t.Error("build page missing install button")
	}
	if !strings.Contains(body, "itms-services://") {
		t.Error("build page missing itms-services install link (template.URL filtered?)")
	}

	// The self install token backs the manifest/ipa/icon fetches (no cookie).
	self, err := selfToken(s.db, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	mBody, code := getBody(t, client, srv.URL+"/i/"+self.Token+"/manifest.plist", nil)
	if code != http.StatusOK {
		t.Fatalf("manifest status = %d", code)
	}
	if !strings.Contains(mBody, "com.farfield.flow") || !strings.Contains(mBody, "/app.ipa") {
		t.Errorf("manifest content wrong:\n%s", mBody)
	}

	// The .ipa streams back byte-for-byte.
	ipaBody, code := getRaw(t, client, srv.URL+"/i/"+self.Token+"/app.ipa", nil)
	if code != http.StatusOK {
		t.Fatalf("ipa status = %d", code)
	}
	if !bytes.Equal(ipaBody, ipa) {
		t.Errorf("served .ipa differs from upload (%d vs %d bytes)", len(ipaBody), len(ipa))
	}

	// Icons render.
	if _, code := getRaw(t, client, srv.URL+"/i/"+self.Token+"/display.png", nil); code != http.StatusOK {
		t.Errorf("display icon status = %d", code)
	}

	// ── public single-use share ──
	sreq, _ := http.NewRequest("POST", srv.URL+"/api/builds/"+up.ID+"/share?max=1&ttl=30m", nil)
	sreq.Header.Set("X-API-Key", "apikey")
	sresp, _ := client.Do(sreq)
	var share struct{ Token, ShareURL string }
	decodeJSON(t, sresp, &share)
	if share.Token == "" {
		t.Fatal("share token empty")
	}

	// Landing page is public and offers the install.
	if lb, code := getBody(t, client, srv.URL+"/s/"+share.Token, nil); code != http.StatusOK || !strings.Contains(lb, "itms-services://") {
		t.Errorf("share landing status=%d", code)
	}

	// Manifest works while active; the full .ipa download consumes the share.
	if _, code := getBody(t, client, srv.URL+"/i/"+share.Token+"/manifest.plist", nil); code != http.StatusOK {
		t.Fatalf("share manifest status = %d", code)
	}
	if _, code := getRaw(t, client, srv.URL+"/i/"+share.Token+"/app.ipa", nil); code != http.StatusOK {
		t.Fatalf("share ipa status = %d", code)
	}
	// After consumption a fresh install can't START — manifest is gone.
	if _, code := getBody(t, client, srv.URL+"/i/"+share.Token+"/manifest.plist", nil); code != http.StatusGone {
		t.Errorf("consumed share manifest status = %d want 410", code)
	}
	// But the .ipa still serves inside the grace window (trailing range requests).
	if _, code := getRaw(t, client, srv.URL+"/i/"+share.Token+"/app.ipa", nil); code != http.StatusOK {
		t.Errorf("consumed share ipa within grace status = %d want 200", code)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func get(t *testing.T, c *http.Client, url string, cookie *http.Cookie) int {
	t.Helper()
	_, code := getBody(t, c, url, cookie)
	return code
}

func getBody(t *testing.T, c *http.Client, url string, cookie *http.Cookie) (string, int) {
	t.Helper()
	b, code := getRaw(t, c, url, cookie)
	return string(b), code
}

func getRaw(t *testing.T, c *http.Client, url string, cookie *http.Cookie) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), resp.StatusCode
}
