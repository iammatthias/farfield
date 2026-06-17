package main

import "howett.net/plist"

// The OTA manifest iOS fetches from an itms-services:// link. It points the
// install daemon at the .ipa plus the two prompt icons and restates the
// identity metadata, which must match the .ipa for the install to proceed.
//
// Field order follows Apple's documented structure; go-plist emits the full
// XML plist (declaration + DOCTYPE) the daemon expects.

type manifestAsset struct {
	Kind string `plist:"kind"`
	URL  string `plist:"url"`
}

type manifestMetadata struct {
	BundleIdentifier string `plist:"bundle-identifier"`
	BundleVersion    string `plist:"bundle-version"`
	Kind             string `plist:"kind"`
	Title            string `plist:"title"`
}

type manifestItem struct {
	Assets   []manifestAsset  `plist:"assets"`
	Metadata manifestMetadata `plist:"metadata"`
}

type manifestPlist struct {
	Items []manifestItem `plist:"items"`
}

// manifestURLs are the absolute HTTPS URLs the manifest references, all scoped
// to one install-session token.
type manifestURLs struct {
	IPA     string
	Display string
	Full    string
}

// buildManifest renders the OTA manifest XML for a build.
func buildManifest(b *Build, urls manifestURLs) ([]byte, error) {
	title := b.AppName
	if title == "" {
		title = b.BundleID
	}
	version := b.BuildNumber
	if version == "" {
		version = b.Version
	}
	m := manifestPlist{Items: []manifestItem{{
		Assets: []manifestAsset{
			{Kind: "software-package", URL: urls.IPA},
			{Kind: "display-image", URL: urls.Display},
			{Kind: "full-size-image", URL: urls.Full},
		},
		Metadata: manifestMetadata{
			BundleIdentifier: b.BundleID,
			BundleVersion:    version,
			Kind:             "software",
			Title:            title,
		},
	}}}
	return plist.MarshalIndent(m, plist.XMLFormat, "\t")
}
