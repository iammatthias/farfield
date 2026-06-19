package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func TestNormalizeUDID(t *testing.T) {
	cases := []struct {
		in, out string
		ok      bool
	}{
		{"00008110-001A2B3C4D5E6F70", "00008110-001A2B3C4D5E6F70", true},
		{"  00008110-001a2b3c4d5e6f70 ", "00008110-001A2B3C4D5E6F70", true}, // trims + upper
		{strings.Repeat("a", 40), strings.Repeat("a", 40), true},            // legacy 40-hex
		{strings.Repeat("A", 40), strings.Repeat("a", 40), true},            // lowered
		{"not-a-udid", "", false},
		{"00008110-XYZ", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeUDID(c.in)
		if ok != c.ok || got != c.out {
			t.Errorf("normalizeUDID(%q) = %q,%v want %q,%v", c.in, got, ok, c.out, c.ok)
		}
	}
}

func TestProvisionedSet(t *testing.T) {
	set := provisionedSet(&Build{UDIDs: "00008110-AAAA\n00008110-BBBB\n"})
	if !set["00008110-aaaa"] || !set["00008110-bbbb"] || set["other"] {
		t.Errorf("provisionedSet = %v", set)
	}
}

func TestEnrollProfile(t *testing.T) {
	mc, err := buildEnrollProfile("TOKEN123", "Demo App", "https://h/register/TOKEN123/capture")
	if err != nil {
		t.Fatalf("buildEnrollProfile: %v", err)
	}
	var p enrollProfile
	if _, err := plist.Unmarshal(mc, &p); err != nil {
		t.Fatalf("profile not valid plist: %v", err)
	}
	if p.PayloadType != "Profile Service" {
		t.Errorf("PayloadType = %q", p.PayloadType)
	}
	if p.PayloadContent.URL != "https://h/register/TOKEN123/capture" {
		t.Errorf("callback URL = %q", p.PayloadContent.URL)
	}
	if len(p.PayloadContent.DeviceAttributes) == 0 || p.PayloadUUID == "" {
		t.Errorf("missing attributes/uuid: %+v", p)
	}
}

func deviceAttrsBody(t *testing.T, udid, product, name string) []byte {
	t.Helper()
	x, err := plist.Marshal(map[string]any{
		"UDID": udid, "PRODUCT": product, "DEVICE_NAME": name,
		"VERSION": "21A1", "SERIAL": "F2LXXXXXX",
	}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte("\x30\x82SIGNED-CMS-PREAMBLE\x00"), x...)
	return append(body, []byte("CMS-TRAILER\x00")...)
}

func TestParseDeviceAttrs(t *testing.T) {
	a, err := parseDeviceAttrs(deviceAttrsBody(t, "00008110-001A2B3C4D5E6F70", "iPhone16,1", "Sam's iPhone"))
	if err != nil {
		t.Fatalf("parseDeviceAttrs: %v", err)
	}
	if a.UDID != "00008110-001A2B3C4D5E6F70" || a.Product != "iPhone16,1" || a.DeviceName != "Sam's iPhone" {
		t.Errorf("attrs = %+v", a)
	}
	if _, err := parseDeviceAttrs([]byte("garbage")); err == nil {
		t.Error("expected error on non-plist body")
	}
}

func TestDeviceAndRegistrationCRUD(t *testing.T) {
	s := testServer(t)
	isNew, err := addOrUpdateDevice(s.db, &Device{ID: "d1", BundleID: "com.x", UDID: "U1", Source: "manual"})
	if err != nil || !isNew {
		t.Fatalf("first add: new=%v err=%v", isNew, err)
	}
	// Same UDID again with a name fills it in, not a new row.
	isNew, _ = addOrUpdateDevice(s.db, &Device{ID: "d2", BundleID: "com.x", UDID: "U1", Name: "Sam", Source: "manual"})
	if isNew {
		t.Error("duplicate UDID should not be new")
	}
	devs, _ := listDevices(s.db, "com.x")
	if len(devs) != 1 || devs[0].Name != "Sam" {
		t.Fatalf("devices = %+v", devs)
	}
	if err := deleteDevice(s.db, "com.x", devs[0].ID); err != nil {
		t.Fatal(err)
	}
	if devs, _ := listDevices(s.db, "com.x"); len(devs) != 0 {
		t.Error("device not deleted")
	}

	tok, err := enableRegistration(s.db, "com.x")
	if err != nil || tok == "" {
		t.Fatalf("enable: %q %v", tok, err)
	}
	if tok2, _ := enableRegistration(s.db, "com.x"); tok2 != tok {
		t.Error("enable should be idempotent")
	}
	if b, _ := registrationBundle(s.db, tok); b != "com.x" {
		t.Errorf("registrationBundle = %q", b)
	}
	if err := disableRegistration(s.db, "com.x"); err != nil {
		t.Fatal(err)
	}
	if reg, _ := getRegistration(s.db, "com.x"); reg != nil {
		t.Error("registration should be gone")
	}
}

func TestDeviceWhitelistHTTP(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cookie := sessionCookie(t, s)

	// An app with one provisioned device in its profile.
	provisioned := "00008110-0011223344556677"
	ipa := makeIPA(t, "com.dev.app", "DevApp", "1.0", "1", time.Now().Add(200*24*time.Hour), []string{provisioned})
	b, err := s.ingest(bytes.NewReader(ipa), "a.ipa", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := b.BundleID

	postForm := func(path string, vals url.Values) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Add a pending device (not in the profile).
	pending := "00008110-00AABBCCDDEEFF01"
	if code := postForm("/app/"+bundle+"/devices", url.Values{"udid": {pending}, "name": {"Sam"}}); code != http.StatusSeeOther {
		t.Fatalf("add device = %d", code)
	}
	// Import the profile's provisioned device.
	if code := postForm("/app/"+bundle+"/devices/import", nil); code != http.StatusSeeOther {
		t.Fatalf("import = %d", code)
	}
	devs, _ := listDevices(s.db, bundle)
	if len(devs) != 2 {
		t.Fatalf("device count = %d, want 2", len(devs))
	}

	// Bad UDID is rejected (redirect carries an error).
	if code := postForm("/app/"+bundle+"/devices", url.Values{"udid": {"nope"}}); code != http.StatusSeeOther {
		t.Errorf("bad udid status = %d", code)
	}
	if n, _ := listDevices(s.db, bundle); len(n) != 2 {
		t.Error("bad udid should not add a device")
	}

	// The devices page shows pending + in-build status.
	body, _ := getBody(t, srv.Client(), srv.URL+"/app/"+bundle+"/devices", cookie)
	if !strings.Contains(body, "pending") || !strings.Contains(body, "in build") {
		t.Error("devices page missing status pills")
	}

	// Export is tab-separated and contains both UDIDs.
	exp, code := getBody(t, srv.Client(), srv.URL+"/app/"+bundle+"/devices.txt", cookie)
	if code != http.StatusOK || !strings.Contains(exp, pending) || !strings.Contains(exp, "\t") {
		t.Errorf("export = %q (code %d)", exp, code)
	}

	// Enable public registration and exercise the capture flow.
	if code := postForm("/app/"+bundle+"/register/enable", nil); code != http.StatusSeeOther {
		t.Fatalf("enable registration = %d", code)
	}
	reg, _ := getRegistration(s.db, bundle)
	if reg == nil {
		t.Fatal("registration not enabled")
	}
	// Public landing renders.
	if lb, code := getBody(t, srv.Client(), srv.URL+"/register/"+reg.Token, nil); code != http.StatusOK || !strings.Contains(lb, "Add this device") {
		t.Errorf("register landing code=%d", code)
	}
	// The .mobileconfig serves with the Apple config content type.
	mreq, _ := http.NewRequest("GET", srv.URL+"/register/"+reg.Token+"/enroll.mobileconfig", nil)
	mresp, _ := srv.Client().Do(mreq)
	if ct := mresp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-apple-aspen-config") {
		t.Errorf("mobileconfig content-type = %q", ct)
	}
	mresp.Body.Close()

	// iOS posts the signed device attributes → device is captured.
	captured := "00008110-00FACE00FACE0001"
	creq, _ := http.NewRequest("POST", srv.URL+"/register/"+reg.Token+"/capture",
		bytes.NewReader(deviceAttrsBody(t, captured, "iPhone16,2", "Pat's iPhone")))
	cresp, err := noRedirect.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusFound {
		t.Fatalf("capture = %d, want 302", cresp.StatusCode)
	}
	var found *Device
	for i, d := range mustList(t, s, bundle) {
		if strings.EqualFold(d.UDID, captured) {
			found = &mustList(t, s, bundle)[i]
		}
	}
	if found == nil || found.Source != "capture" || found.Product != "iPhone16,2" {
		t.Errorf("captured device = %+v", found)
	}
}

func TestOwnerDeviceAlwaysIncluded(t *testing.T) {
	s := testServer(t)
	s.ownerUDID = "00008150-001C25C82220401C"

	// A build with a profile that does NOT include the owner.
	ipa := makeIPA(t, "com.owner.app", "OwnerApp", "1.0", "1", time.Now().Add(200*24*time.Hour), nil)
	if _, err := s.ingest(bytes.NewReader(ipa), "a.ipa", "", ""); err != nil {
		t.Fatal(err)
	}
	// Ingest pinned the owner into the whitelist.
	devs := mustList(t, s, "com.owner.app")
	if len(devs) != 1 || devs[0].UDID != s.ownerUDID || devs[0].Source != "owner" {
		t.Fatalf("owner not pinned on ingest: %+v", devs)
	}
	// appDevices re-ensures it even if removed (it is "always" present).
	_ = deleteDevice(s.db, "com.owner.app", devs[0].ID)
	again, _ := s.appDevices("com.owner.app")
	if len(again) != 1 || again[0].UDID != s.ownerUDID {
		t.Errorf("owner not re-ensured: %+v", again)
	}
	// Export always contains it.
	if !strings.Contains(exportDevices(again), s.ownerUDID) {
		t.Error("export missing owner UDID")
	}
}

func mustList(t *testing.T, s *Server, bundle string) []Device {
	t.Helper()
	devs, err := listDevices(s.db, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return devs
}
