package theme

import (
	"strings"
	"testing"
)

// TestFontsAreSelfContained pins the guarantee apex broke: the fleet ships its
// own faces, so nothing may reference an external font host.
func TestFontsAreSelfContained(t *testing.T) {
	if n := strings.Count(Fonts, "@font-face"); n != strings.Count(CSS, "@font-face") {
		t.Errorf("extracted %d @font-face rules, stylesheet has %d",
			n, strings.Count(CSS, "@font-face"))
	}
	if !strings.Contains(Fonts, "data:") {
		t.Error("extracted faces carry no data: URI — the faces are not vendored")
	}
	// "src: url(http" is the only shape that fetches off-box; a data: URI
	// contains slashes of its own, so a bare "//" test would false-positive.
	for _, ext := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "url(http", "url('http", "url(\"http"} {
		if strings.Contains(Fonts, ext) {
			t.Errorf("extracted faces fetch from off-box (%q)", ext)
		}
	}
	if strings.Contains(Fonts, "}") && !strings.HasSuffix(strings.TrimSpace(Fonts), "}") {
		t.Error("extraction truncated mid-rule")
	}
}

// TestStampAssets substitutes the version a static shell cannot template.
func TestStampAssets(t *testing.T) {
	in := []byte(`<link href="/static/styles.css?v=__THEME_VERSION__">`)
	got := string(StampAssets(in))
	if strings.Contains(got, assetVersionToken) {
		t.Errorf("placeholder survived: %s", got)
	}
	if !strings.Contains(got, Version) {
		t.Errorf("version %q not substituted: %s", Version, got)
	}
}
