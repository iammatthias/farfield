package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// The install session: token-gated and cookie-free, because iOS fetches the
// manifest and the .ipa from a process that carries no browser session.

// loadToken resolves a path token with per-IP enumeration rate-limiting.
// Returns the token, or nil with a status the caller should respond.
func (s *Server) loadToken(r *http.Request) (*Token, int) {
	raw := r.PathValue("token")
	if !tokenPattern.MatchString(raw) {
		return nil, http.StatusNotFound
	}
	ip := web.ClientIP(r)
	if s.limiter.Blocked(ip) {
		return nil, http.StatusTooManyRequests
	}
	t, err := getToken(s.db, raw)
	if err != nil {
		slog.Error("get token", "err", err)
		return nil, http.StatusInternalServerError
	}
	if t == nil {
		s.limiter.Fail(ip)
		return nil, http.StatusNotFound
	}
	return t, http.StatusOK
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || !tok.canStart() {
		goneText(w)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		goneText(w)
		return
	}
	base := s.publicURL + "/i/" + tok.Token
	xml, err := buildManifest(b, manifestURLs{
		IPA:     base + "/app.ipa",
		Display: base + "/display.png",
		Full:    base + "/full.png",
	})
	if err != nil {
		slog.Error("build manifest", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(xml)
}

func (s *Server) handleIPA(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || !tok.canServeBytes() {
		goneText(w)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		goneText(w)
		return
	}
	f, size, err := s.blobs.open(b.CID, ".ipa")
	if err != nil {
		slog.Error("open blob", "cid", b.CID, "err", err)
		goneText(w)
		return
	}
	defer f.Close()

	name := b.Filename
	if name == "" {
		name = "app.ipa"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(name)+`"`)

	wasStartable := tok.canStart()
	deliversLast := deliversFinalByte(r.Header.Get("Range"), size)

	var modtime time.Time
	if t, perr := time.Parse(time.RFC3339, b.CreatedAt); perr == nil {
		modtime = t
	}
	http.ServeContent(w, r, name, modtime, f)

	// Count the install only when this response delivered the archive's final
	// byte and the token was still startable when it began — so a multi-range
	// download counts once, on the request that finishes it.
	if r.Method == http.MethodGet && wasStartable && deliversLast {
		if err := recordInstall(s.db, tok, r.UserAgent(), web.ClientIP(r)); err != nil {
			slog.Warn("record install", "err", err)
		} else {
			slog.Info("install delivered", "build", b.ID, "kind", tok.Kind,
				"used", tok.UsedInstalls, "state", tok.State)
		}
	}
}

// handleIcon serves a generated identicon for the install prompt at the given
// pixel size.
func (s *Server) handleIcon(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, code := s.loadToken(r)
		if code != http.StatusOK || !tok.canServeBytes() {
			goneText(w)
			return
		}
		b, err := getBuild(s.db, tok.BuildID)
		if err != nil || b == nil {
			goneText(w)
			return
		}
		png, err := iconPNG(b.BundleID, size)
		if err != nil {
			slog.Error("icon", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(png)
	}
}
