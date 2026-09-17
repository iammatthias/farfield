package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// Public device registration — the only unauthenticated write path in the
// service. A tester opens a link, installs an enrolment profile, and iOS posts
// back the UDID that the whitelist then admits.

// regBundle resolves the registration token to its bundle id, rate-limiting and
// rendering the gone page when invalid. ok=false means the response is written.
func (s *Server) regBundle(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := r.PathValue("token")
	if !tokenPattern.MatchString(token) {
		s.renderRegGone(w)
		return "", false
	}
	ip := web.ClientIP(r)
	if s.limiter.Blocked(ip) {
		s.renderRegGone(w)
		return "", false
	}
	bundle, err := registrationBundle(s.db, token)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	if bundle == "" {
		s.limiter.Fail(ip)
		s.renderRegGone(w)
		return "", false
	}
	return bundle, true
}

func (s *Server) renderRegGone(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	s.rd.Render(w, "gone.html", nil)
}

// appName returns an app's friendly name from its latest build.
func (s *Server) appName(bundle string) string {
	builds, _ := listBuildsByBundle(s.db, bundle)
	if len(builds) > 0 && builds[0].AppName != "" {
		return builds[0].AppName
	}
	return bundle
}

func (s *Server) handleRegisterLanding(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	s.rd.Render(w, "register.html", map[string]any{
		"Token":    r.PathValue("token"),
		"AppName":  s.appName(bundle),
		"BundleID": bundle,
		"Error":    r.URL.Query().Get("error"),
	})
}

func (s *Server) handleEnrollProfile(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	token := r.PathValue("token")
	callback := s.publicURL + "/register/" + token + "/capture"
	mc, err := buildEnrollProfile(token, s.appName(bundle), callback)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="enroll.mobileconfig"`)
	_, _ = w.Write(mc)
}

// handleEnrollCapture receives the signed device-attributes plist iOS posts
// after the user installs the enrolment profile, and whitelists the UDID.
func (s *Server) handleEnrollCapture(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	bundle, err := registrationBundle(s.db, token)
	if err != nil || bundle == "" {
		http.Error(w, "registration closed", http.StatusGone)
		return
	}
	ip := web.ClientIP(r)
	if s.limiter.Blocked(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	attrs, err := parseDeviceAttrs(body)
	if err != nil {
		s.limiter.Fail(ip)
		slog.Warn("enrol parse", "err", err)
		http.Error(w, "could not read device info", http.StatusBadRequest)
		return
	}
	udid, ok := normalizeUDID(attrs.UDID)
	if !ok {
		http.Error(w, "invalid udid", http.StatusBadRequest)
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: udid,
		Name: strings.TrimSpace(attrs.DeviceName), Product: attrs.Product, Source: "capture",
	}); err != nil {
		// Falling through to the done page told the visitor their device was
		// whitelisted when it was not: they only find out much later, when the
		// install fails with no clue why. The landing page renders .Error.
		slog.Error("enrol add device", "err", err)
		http.Redirect(w, r, "/register/"+token+"?error=Could+not+register+this+device.+Please+try+again.",
			http.StatusFound)
		return
	}
	slog.Info("device enrolled", "bundle", bundle, "product", attrs.Product)
	http.Redirect(w, r, "/register/"+token+"/done", http.StatusFound)
}

// handleRegisterSubmit takes a typed UDID from the public page's manual form —
// the fallback when a visitor already knows their identifier.
func (s *Server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	bundle, err := registrationBundle(s.db, token)
	if err != nil || bundle == "" {
		s.renderRegGone(w)
		return
	}
	ip := web.ClientIP(r)
	if s.limiter.Blocked(ip) {
		http.Redirect(w, r, "/register/"+token+"?error="+url.QueryEscape("Too many attempts. Wait a minute."), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	udid, ok := normalizeUDID(r.FormValue("udid"))
	if !ok {
		s.limiter.Fail(ip)
		http.Redirect(w, r, "/register/"+token+"?error="+url.QueryEscape("That doesn't look like a UDID."), http.StatusSeeOther)
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: udid,
		Name: strings.TrimSpace(r.FormValue("name")), Source: "capture",
	}); err != nil {
		// Same reason as the capture path: a done page after a failed insert
		// is a lie the visitor only discovers when the install fails.
		slog.Error("submit device", "err", err)
		http.Redirect(w, r, "/register/"+token+"?error="+
			url.QueryEscape("Could not register this device. Please try again."),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/register/"+token+"/done", http.StatusSeeOther)
}

func (s *Server) handleRegisterDone(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	s.rd.Render(w, "register_done.html", map[string]any{"AppName": s.appName(bundle)})
}
