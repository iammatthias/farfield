package main

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/iammatthias/farfield/lib/qrenc"
)

// Deleting an app or a build, and serving one build's page. Removal is
// refcounted: the store dedupes by content, so the same bytes under two
// bundles is one file with two rows pointing at it.

// handleAppDelete removes an entire app — every version, its tokens, its
// rich-page metadata, and all of their files.
func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	cids, shots, n, err := deleteApp(s.db, bundle)
	if err != nil {
		slog.Error("delete app", "bundle", bundle, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.removeAppFiles(cids, shots)
	slog.Info("app deleted", "bundle", bundle, "versions", n)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// removeAppFiles drops a deleted app's .ipa blobs and screenshot images.
//
// Every removal is refcounted first. The store dedupes globally by content, so
// the same image uploaded under two bundles is two rows pointing at one file:
// deleting app A used to take the bytes out from under app B's gallery, which
// then 404s forever with no remote object left to refill from. The single
// screenshot delete already guarded this; whole-app delete did not.
func (s *Server) removeAppFiles(buildCIDs []string, shots []Screenshot) {
	for _, c := range buildCIDs {
		if others, err := buildsWithCID(s.db, c); err != nil {
			// Never delete on an unreadable refcount — a lost blob cannot be
			// recovered, a kept one is only wasted disk.
			slog.Warn("skipping blob removal: refcount unreadable", "cid", c, "err", err)
			continue
		} else if others > 0 {
			continue
		}
		if err := s.blobs.remove(c, ".ipa"); err != nil {
			slog.Warn("could not remove blob", "cid", c, "err", err)
		}
	}
	for _, sh := range shots {
		if others, err := screenshotsWithCID(s.db, sh.CID); err != nil {
			slog.Warn("skipping screenshot removal: refcount unreadable", "cid", sh.CID, "err", err)
			continue
		} else if others > 0 {
			continue
		}
		if err := s.blobs.remove(sh.CID, sh.Ext); err != nil {
			slog.Warn("could not remove screenshot", "cid", sh.CID, "err", err)
		}
	}
}

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	b, err := getBuild(s.db, id)
	if err != nil {
		slog.Error("get build", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	tok, err := selfToken(s.db, b.ID)
	if err != nil {
		slog.Error("self token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pageURL := s.publicURL + "/b/" + b.ID
	qrSVG, _, err := qrenc.EncodeSVG([]byte(pageURL), qrenc.ECMedium)
	if err != nil {
		slog.Warn("qr encode", "err", err)
		qrSVG = ""
	}
	s.rd.Render(w, "build.html", map[string]any{
		"Build":      b,
		"Expiry":     expiryView(b),
		"InstallURL": s.installLink(tok.Token),
		"PageURL":    pageURL,
		"QR":         template.HTML(qrSVG),
	})
}

// handleDelete removes one version. It returns to the app's version list when
// other versions remain, else to the index.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dest := "/"
	if b != nil {
		if _, err := deleteBuild(s.db, id); err != nil {
			slog.Error("delete build", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.blobs.remove(b.CID, ".ipa"); err != nil {
			slog.Warn("could not remove blob", "cid", b.CID, "err", err)
		}
		if remaining, err := listBuildsByBundle(s.db, b.BundleID); err == nil && len(remaining) > 0 {
			dest = "/app/" + b.BundleID
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
