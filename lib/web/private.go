package web

import "net/http"

// AdminPrefix is where every app mounts its private admin API — the routes a
// native client on the tailnet uses for what the session console can see but
// the public API deliberately cannot (private records, admin notes, previews
// of disabled codes).
const AdminPrefix = "/api/admin/"

// PrivateAPI guards an admin route. The checks run in this order, and the
// order is the point:
//
//  1. Arrived through Cloudflare → 404 {"error":"not found"}. Every request
//     the tunnel forwards carries Cf-Ray and Cf-Connecting-IP — Cloudflare
//     adds them at the edge and a client cannot strip them — while tailnet
//     traffic (tailscale serve straight to the app) never has either. So the
//     admin API is simply not there from the internet, whatever key is
//     presented: a leaked write key is not enough on its own. This runs first
//     so no later answer (503, 401) confirms to an internet caller that the
//     route exists.
//  2. No write key configured (no env key and no key store) → 503. Fail
//     closed: an app that never set a key has no admin API, rather than an
//     open one.
//  3. No valid write credential — the env key or an admin-issued write-scoped
//     ffk_ key; a read key is not enough → 401.
//
// Every response, success included, is Cache-Control: no-store — these
// payloads carry private records, and WriteRecord's no-cache would otherwise
// let an intermediary keep a copy.
func (a *Auth) PrivateAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w = &noStoreWriter{ResponseWriter: w}
		if viaCloudflare(r) {
			WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if a.APIKey == "" && a.Keys == nil {
			WriteError(w, http.StatusServiceUnavailable, "admin API disabled: no key configured")
			return
		}
		if !a.HasWriteKey(r) {
			WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// AdminNotFound answers an /api/admin/ path no route claims. Apps mount it
// behind PrivateAPI on the bare prefix, so an unknown admin path — or a known
// one with the wrong method — looks exactly like a known one from the
// internet (the same JSON 404), and the mux's plain-text 404/405 never hints
// at which admin routes exist.
func AdminNotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, "not found")
}

// viaCloudflare reports whether the request came through the Cloudflare
// tunnel rather than private ingress.
func viaCloudflare(r *http.Request) bool {
	return r.Header.Get("Cf-Ray") != "" || r.Header.Get("Cf-Connecting-Ip") != ""
}

// noStoreWriter pins Cache-Control: no-store at the moment the status goes
// out, overriding whatever the handler set — WriteRecord's no-cache included.
type noStoreWriter struct {
	http.ResponseWriter
	wrote bool
}

func (nw *noStoreWriter) WriteHeader(code int) {
	if !nw.wrote {
		nw.wrote = true
		nw.Header().Set("Cache-Control", "no-store")
	}
	nw.ResponseWriter.WriteHeader(code)
}

func (nw *noStoreWriter) Write(b []byte) (int, error) {
	if !nw.wrote {
		nw.WriteHeader(http.StatusOK)
	}
	return nw.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so a streamed response still streams.
func (nw *noStoreWriter) Flush() {
	if !nw.wrote {
		nw.WriteHeader(http.StatusOK)
	}
	if f, ok := nw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (nw *noStoreWriter) Unwrap() http.ResponseWriter { return nw.ResponseWriter }
