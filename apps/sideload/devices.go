package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"howett.net/plist"
)

// udidRe matches the two iOS device-identifier shapes: the modern 25-char
// dashed form (00008110-001A2B3C4D5E6F70) and the legacy 40-hex form.
var udidRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16}$|^[0-9A-Fa-f]{40}$`)

// normalizeUDID trims and validates a UDID. The modern dashed form is
// upper-cased, the legacy hex form lower-cased, for stable de-duplication.
func normalizeUDID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !udidRe.MatchString(s) {
		return "", false
	}
	if strings.Contains(s, "-") {
		return strings.ToUpper(s), true
	}
	return strings.ToLower(s), true
}

// provisionedSet returns the lower-cased UDIDs already in a build's profile,
// for diffing the whitelist against what the latest build actually ships.
func provisionedSet(b *Build) map[string]bool {
	set := map[string]bool{}
	if b == nil {
		return set
	}
	for _, u := range strings.Split(b.UDIDs, "\n") {
		if u = strings.ToLower(strings.TrimSpace(u)); u != "" {
			set[u] = true
		}
	}
	return set
}

// deviceAttrs is the subset of the device-attribute plist iOS POSTs back during
// Profile Service enrolment.
type deviceAttrs struct {
	UDID       string `plist:"UDID"`
	Product    string `plist:"PRODUCT"`
	Version    string `plist:"VERSION"`
	DeviceName string `plist:"DEVICE_NAME"`
	Serial     string `plist:"SERIAL"`
}

// parseDeviceAttrs extracts the device attributes from the signed plist iOS
// posts to the enrolment callback.
func parseDeviceAttrs(body []byte) (*deviceAttrs, error) {
	xmlBytes, err := extractPlist(body)
	if err != nil {
		return nil, err
	}
	var a deviceAttrs
	if _, err := plist.Unmarshal(xmlBytes, &a); err != nil {
		return nil, err
	}
	if a.UDID == "" {
		return nil, fmt.Errorf("no UDID in device attributes")
	}
	return &a, nil
}

// ── Profile Service enrolment profile (.mobileconfig) ─────────────────────────

type profileServiceContent struct {
	URL              string   `plist:"URL"`
	DeviceAttributes []string `plist:"DeviceAttributes"`
}

type enrollProfile struct {
	PayloadContent      profileServiceContent `plist:"PayloadContent"`
	PayloadOrganization string                `plist:"PayloadOrganization"`
	PayloadDisplayName  string                `plist:"PayloadDisplayName"`
	PayloadDescription  string                `plist:"PayloadDescription"`
	PayloadVersion      int                   `plist:"PayloadVersion"`
	PayloadUUID         string                `plist:"PayloadUUID"`
	PayloadIdentifier   string                `plist:"PayloadIdentifier"`
	PayloadType         string                `plist:"PayloadType"`
}

// buildEnrollProfile renders the Profile Service .mobileconfig that asks iOS to
// post its UDID (and product/name) to callbackURL. Unsigned — iOS shows an
// "Unverified" note but still enrols, which is fine for a personal tool.
func buildEnrollProfile(token, appName, callbackURL string) ([]byte, error) {
	p := enrollProfile{
		PayloadContent: profileServiceContent{
			URL:              callbackURL,
			DeviceAttributes: []string{"UDID", "PRODUCT", "VERSION", "DEVICE_NAME", "SERIAL"},
		},
		PayloadOrganization: "farfield · sideload",
		PayloadDisplayName:  "Register device — " + appName,
		PayloadDescription:  "Sends this device's identifier so it can be added to the next build.",
		PayloadVersion:      1,
		PayloadUUID:         uuidFrom("enroll:" + token),
		PayloadIdentifier:   "systems.farfield.sideload.enroll." + token,
		PayloadType:         "Profile Service",
	}
	return plist.MarshalIndent(p, plist.XMLFormat, "\t")
}

// uuidFrom derives a stable RFC-4122-shaped UUID from a seed — the profile's
// PayloadUUID only needs to be stable and unique-ish.
func uuidFrom(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	x := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", x[0:8], x[8:12], x[12:16], x[16:20], x[20:32])
}

// exportDevices renders the whitelist for Apple's bulk device registration: one
// "UDID<TAB>Name" line per device.
func exportDevices(devices []Device) string {
	var b strings.Builder
	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = "device"
		}
		fmt.Fprintf(&b, "%s\t%s\n", d.UDID, name)
	}
	return b.String()
}
