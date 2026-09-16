package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
)

// The device whitelist as the author manages it: add, import, export, delete,
// and the switch that opens or closes public registration for one app.

// deviceView pairs a whitelisted device with whether the latest build's
// profile already provisions it.
type deviceView struct {
	Device
	Provisioned bool
}

// ensureOwnerDevice keeps the configured owner UDID in an app's whitelist, so
// the author's own device is always provisioned in the next build. Idempotent.
func (s *Server) ensureOwnerDevice(bundle string) {
	if s.ownerUDID == "" {
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: s.ownerUDID,
		Name: "me", Source: "owner",
	}); err != nil {
		slog.Warn("ensure owner device", "bundle", bundle, "err", err)
	}
}

// appDevices ensures the owner device is present, then lists an app's devices.
func (s *Server) appDevices(bundle string) ([]Device, error) {
	s.ensureOwnerDevice(bundle)
	return listDevices(s.db, bundle)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	latest := builds[0]
	devs, err := s.appDevices(bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	prov := provisionedSet(&latest)
	views := make([]deviceView, len(devs))
	pending := 0
	for i, d := range devs {
		p := prov[strings.ToLower(d.UDID)]
		views[i] = deviceView{Device: d, Provisioned: p}
		if !p {
			pending++
		}
	}
	reg, err := getRegistration(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	registerURL := ""
	if reg != nil {
		registerURL = s.publicURL + "/register/" + reg.Token
	}
	s.rd.Render(w, "devices.html", map[string]any{
		"BundleID":     bundle,
		"AppName":      latest.AppName,
		"Latest":       latest,
		"Devices":      views,
		"Total":        len(devs),
		"Pending":      pending,
		"ProfileCount": len(prov),
		"Registration": reg,
		"RegisterURL":  registerURL,
		"Export":       exportDevices(devs),
		"Error":        r.URL.Query().Get("error"),
	})
}

func (s *Server) devicesError(w http.ResponseWriter, r *http.Request, bundle, msg string) {
	http.Redirect(w, r, "/app/"+bundle+"/devices?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleDeviceAdd(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	udid, ok := normalizeUDID(r.FormValue("udid"))
	if !ok {
		s.devicesError(w, r, bundle, "That doesn't look like a UDID (expected 25- or 40-char hex).")
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID:       store.ShortID(),
		BundleID: bundle,
		UDID:     udid,
		Name:     strings.TrimSpace(r.FormValue("name")),
		Note:     strings.TrimSpace(r.FormValue("note")),
		Source:   "manual",
	}); err != nil {
		slog.Error("add device", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

// handleDeviceImport seeds the whitelist from the latest build's profile — the
// devices already provisioned — so an existing app starts tracked.
func (s *Server) handleDeviceImport(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil || len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	n := 0
	for _, raw := range strings.Split(builds[0].UDIDs, "\n") {
		udid, ok := normalizeUDID(raw)
		if !ok {
			continue
		}
		if isNew, err := addOrUpdateDevice(s.db, &Device{
			ID: store.ShortID(), BundleID: bundle, UDID: udid, Source: "manual",
		}); err == nil && isNew {
			n++
		}
	}
	slog.Info("imported profile devices", "bundle", bundle, "added", n)
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if err := deleteDevice(s.db, bundle, r.PathValue("did")); err != nil {
		slog.Error("delete device", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleDevicesExport(w http.ResponseWriter, r *http.Request) {
	devs, err := s.appDevices(r.PathValue("bundle"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="devices.txt"`)
	_, _ = io.WriteString(w, exportDevices(devs))
}

func (s *Server) handleRegEnable(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if builds, err := listBuildsByBundle(s.db, bundle); err != nil || len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	if _, err := enableRegistration(s.db, bundle); err != nil {
		slog.Error("enable registration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleRegDisable(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if err := disableRegistration(s.db, bundle); err != nil {
		slog.Error("disable registration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}
