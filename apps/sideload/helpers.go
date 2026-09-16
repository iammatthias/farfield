package main

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Small shared helpers: link building, TTL and install-count parsing, filename
// sanitising, and the Range arithmetic that decides when iOS has taken the
// final byte of a build.

// installLink builds the itms-services OTA install URL for a token. It is
// returned as template.URL because html/template otherwise filters the
// non-allowlisted itms-services: scheme to a dead "#ZgotmplZ".
func (s *Server) installLink(token string) template.URL {
	manifest := s.publicURL + "/i/" + token + "/manifest.plist"
	return template.URL("itms-services://?action=download-manifest&url=" + url.QueryEscape(manifest))
}

func goneText(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	_, _ = io.WriteString(w, "This install link is no longer valid.\n")
}

// deliversFinalByte reports whether serving the given Range (empty = full GET)
// delivers the last byte of a size-byte file — the signal that an install
// download has completed.
func deliversFinalByte(rangeHeader string, size int64) bool {
	if rangeHeader == "" {
		return true
	}
	const p = "bytes="
	if !strings.HasPrefix(rangeHeader, p) {
		return false
	}
	for _, spec := range strings.Split(rangeHeader[len(p):], ",") {
		spec = strings.TrimSpace(spec)
		dash := strings.IndexByte(spec, '-')
		if dash < 0 {
			continue
		}
		startStr := strings.TrimSpace(spec[:dash])
		endStr := strings.TrimSpace(spec[dash+1:])
		switch {
		case startStr == "": // bytes=-N suffix → last N bytes
			if n, err := strconv.ParseInt(endStr, 10, 64); err == nil && n > 0 {
				return true
			}
		case endStr == "": // bytes=N- open-ended → through the end
			return true
		default: // bytes=N-M
			if end, err := strconv.ParseInt(endStr, 10, 64); err == nil && end >= size-1 {
				return true
			}
		}
	}
	return false
}

// parseTTL maps a share TTL choice to a duration; unknown values fall back to
// the 30-minute default.
func parseTTL(choice string) time.Duration {
	switch choice {
	case "2h":
		return 2 * time.Hour
	case "24h":
		return 24 * time.Hour
	case "30m", "":
		return 30 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// parseMaxInstalls maps a max-installs choice to a count; 0 means unlimited.
// The default is single-use.
func parseMaxInstalls(choice string) int {
	switch choice {
	case "unlimited", "0":
		return 0
	case "3":
		return 3
	case "1", "":
		return 1
	default:
		if n, err := strconv.Atoi(choice); err == nil && n >= 0 {
			return n
		}
		return 1
	}
}

// sanitizeFilename reduces an upload name to a safe base for Content-Disposition.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return "app.ipa"
	}
	name = strings.NewReplacer(`"`, "", "\\", "", "\n", "", "\r", "").Replace(name)
	if !strings.HasSuffix(strings.ToLower(name), ".ipa") {
		name += ".ipa"
	}
	return name
}

// ExpiryView is the provisioning-profile expiry summary the install pages show.
type ExpiryView struct {
	Known   bool
	Date    string
	Days    int
	Expired bool
	Warn    bool // under the warning threshold
}

const expiryWarnDays = 14

func expiryView(b *Build) ExpiryView {
	if b.ProfileExpiry == "" {
		return ExpiryView{}
	}
	t, err := time.Parse(time.RFC3339, b.ProfileExpiry)
	if err != nil {
		return ExpiryView{}
	}
	days := int(time.Until(t).Hours() / 24)
	expired := !t.After(time.Now())
	return ExpiryView{
		Known:   true,
		Date:    t.Format("2006-01-02"),
		Days:    days,
		Expired: expired,
		Warn:    !expired && days < expiryWarnDays,
	}
}

// relAge renders an RFC 3339 timestamp as compact relative age.
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// sizeText renders a byte count for the meta line.
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
