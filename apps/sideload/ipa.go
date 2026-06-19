package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path"
	"regexp"
	"time"

	"howett.net/plist"
)

// ipaMeta is what we extract from a signed ad-hoc .ipa: the app's identity from
// Info.plist and the distribution facts from the embedded provisioning profile.
type ipaMeta struct {
	BundleID      string
	AppName       string    // display name, falling back to bundle name
	Version       string    // CFBundleShortVersionString — the marketing version
	BuildNumber   string    // CFBundleVersion — the build number
	Team          string    // provisioning TeamName
	ProfileExpiry time.Time // zero when unreadable
	UDIDs         []string  // devices enrolled in the profile
}

// infoPlist is the subset of an app's Info.plist sideload reads.
type infoPlist struct {
	BundleID    string `plist:"CFBundleIdentifier"`
	Version     string `plist:"CFBundleShortVersionString"`
	BuildNumber string `plist:"CFBundleVersion"`
	DisplayName string `plist:"CFBundleDisplayName"`
	BundleName  string `plist:"CFBundleName"`
}

// provision is the subset of an embedded.mobileprovision plist sideload reads.
type provision struct {
	TeamName           string    `plist:"TeamName"`
	ExpirationDate     time.Time `plist:"ExpirationDate"`
	ProvisionedDevices []string  `plist:"ProvisionedDevices"`
}

// The main app bundle lives one level under Payload; nested .app bundles
// (extensions, watch apps) sit deeper and are ignored.
var (
	reInfo    = regexp.MustCompile(`^Payload/[^/]+\.app/Info\.plist$`)
	reProfile = regexp.MustCompile(`^Payload/[^/]+\.app/embedded\.mobileprovision$`)
)

// parseIPA reads a stored .ipa from disk and extracts its metadata. The
// provisioning profile is best-effort — a build with an unreadable profile
// still records its Info.plist identity.
func parseIPA(ipaPath string) (*ipaMeta, error) {
	zr, err := zip.OpenReader(ipaPath)
	if err != nil {
		return nil, fmt.Errorf("not a readable .ipa (zip): %w", err)
	}
	defer zr.Close()

	var infoFile, profileFile *zip.File
	for _, f := range zr.File {
		switch {
		case infoFile == nil && reInfo.MatchString(f.Name):
			infoFile = f
		case profileFile == nil && reProfile.MatchString(f.Name):
			profileFile = f
		}
	}
	if infoFile == nil {
		return nil, fmt.Errorf("no Payload/*.app/Info.plist — not an iOS app archive")
	}

	infoBytes, err := readZipEntry(infoFile)
	if err != nil {
		return nil, fmt.Errorf("read Info.plist: %w", err)
	}
	var info infoPlist
	if _, err := plist.Unmarshal(infoBytes, &info); err != nil {
		return nil, fmt.Errorf("parse Info.plist: %w", err)
	}
	if info.BundleID == "" {
		return nil, fmt.Errorf("Info.plist has no CFBundleIdentifier")
	}

	meta := &ipaMeta{
		BundleID:    info.BundleID,
		AppName:     appName(info, infoFile.Name),
		Version:     info.Version,
		BuildNumber: info.BuildNumber,
	}

	if profileFile != nil {
		if pb, err := readZipEntry(profileFile); err == nil {
			if prov, err := parseProvision(pb); err == nil {
				meta.Team = prov.TeamName
				meta.ProfileExpiry = prov.ExpirationDate
				meta.UDIDs = prov.ProvisionedDevices
			}
		}
	}
	return meta, nil
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Info.plist and mobileprovision are kilobytes; cap defensively anyway.
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}

// appName picks the friendliest available name: display name, then bundle name,
// then the .app directory stem.
func appName(info infoPlist, infoPath string) string {
	if info.DisplayName != "" {
		return info.DisplayName
	}
	if info.BundleName != "" {
		return info.BundleName
	}
	// Payload/<Name>.app/Info.plist → <Name>
	dir := path.Dir(infoPath)             // Payload/<Name>.app
	base := path.Base(dir)                // <Name>.app
	if ext := path.Ext(base); ext != "" { // strip .app
		base = base[:len(base)-len(ext)]
	}
	return base
}

// parseProvision extracts the XML plist embedded in the CMS-wrapped
// mobileprovision blob. The signed content is the literal plist text, so we
// slice it out between its <plist> tags rather than parsing the PKCS#7 envelope
// — reliable across the Xcode versions that produce these.
func parseProvision(der []byte) (*provision, error) {
	xmlBytes, err := extractPlist(der)
	if err != nil {
		return nil, err
	}
	var p provision
	if _, err := plist.Unmarshal(xmlBytes, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// extractPlist slices the XML plist out of a CMS/PKCS#7-wrapped blob — both the
// embedded provisioning profile and the device attributes iOS POSTs during
// enrolment carry their plist as literal text inside the signed envelope.
func extractPlist(der []byte) ([]byte, error) {
	start := bytes.Index(der, []byte("<plist"))
	end := bytes.LastIndex(der, []byte("</plist>"))
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no plist payload found")
	}
	return der[start : end+len("</plist>")], nil
}
