package web

import (
	_ "embed"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
)

// The fleet 404 is a print: "Plate 404 — not in the catalogue", set on a
// halftone landscape in the farfield frontispiece style. Serve applies
// NotFoundPlate to every app's handler, so no app can forget it and every
// host answers a missing page the same way.
//
// The middleware only replaces the *default* not-found response — a GET or
// HEAD whose Accept names text/html and whose handler wrote a 404 with no
// Content-Type or the text/plain one http.NotFound sets. A JSON 404 from an
// API route, a custom HTML 404, and every non-browser client (curl sends
// Accept: */*) pass through byte-for-byte.

//go:embed assets/plate404.webp
var plate404 []byte

// plate404Path is where the middleware serves its own artwork from — a
// reserved path answered before the app's handler runs, so the page works in
// every app without any app embedding the image.
const plate404Path = "/static/plate-404.webp"

var (
	plate404Ver  = cid.Of(plate404)[:16]
	plate404Page = []byte(fmt.Sprintf(plate404HTML, plate404Path+"?v="+plate404Ver))
)

// NotFoundPlate serves the styled fleet 404. Serve wires it in for every app;
// it is exported for servers that bypass Serve and for tests.
func NotFoundPlate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == plate404Path &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) {
			servePlateArt(w, r)
			return
		}
		next.ServeHTTP(&plateWriter{ResponseWriter: w, r: r}, r)
	})
}

// servePlateArt writes the embedded artwork. The page links it with a ?v=
// content fingerprint, so the versioned URL caches as immutable; a bare fetch
// revalidates hourly.
func servePlateArt(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "image/webp")
	h.Set("Content-Length", strconv.Itoa(len(plate404)))
	h.Set("ETag", `"`+plate404Ver+`"`)
	if r.URL.Query().Has("v") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	if ETagMatch(r, plate404Ver) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(plate404)
	}
}

// plateWriter watches for the default 404 and swaps in the plate page. It
// sits outermost — outside Gzip — so on an intercept it must also drop the
// Content-Encoding a compressing inner layer may have promised, then swallow
// that layer's body bytes.
type plateWriter struct {
	http.ResponseWriter
	r        *http.Request
	wrote    bool // WriteHeader has been forwarded
	replaced bool // the plate page was written; discard the handler's body
}

// wantsPlate reports whether this response is the default not-found a browser
// is about to render: an HTML-accepting GET/HEAD whose handler set no
// Content-Type, or the text/plain one http.NotFound / http.Error set.
func (pw *plateWriter) wantsPlate() bool {
	if pw.r.Method != http.MethodGet && pw.r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(pw.r.Header.Get("Accept"), "text/html") {
		return false
	}
	ct := pw.Header().Get("Content-Type")
	return ct == "" || strings.HasPrefix(ct, "text/plain")
}

func (pw *plateWriter) WriteHeader(code int) {
	if pw.wrote {
		return
	}
	pw.wrote = true
	// No 404 leaves the fleet cacheable: the mux's default not-found carries
	// no headers at all, and an edge cache rule would happily hold it. Only
	// fill the gap — a handler that set its own Cache-Control knows better.
	if code == http.StatusNotFound && pw.Header().Get("Cache-Control") == "" {
		pw.Header().Set("Cache-Control", "no-store")
	}
	if code == http.StatusNotFound && pw.wantsPlate() {
		pw.replaced = true
		h := pw.Header()
		h.Del("Content-Encoding")
		h.Del("Content-Length")
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		pw.ResponseWriter.WriteHeader(http.StatusNotFound)
		if pw.r.Method != http.MethodHead {
			_, _ = pw.ResponseWriter.Write(plate404Page)
		}
		return
	}
	pw.ResponseWriter.WriteHeader(code)
}

func (pw *plateWriter) Write(b []byte) (int, error) {
	if !pw.wrote {
		pw.WriteHeader(http.StatusOK)
	}
	if pw.replaced {
		// The inner handler (or its gzip wrapper) is writing the body we
		// replaced; report it consumed so that layer finishes cleanly.
		return len(b), nil
	}
	return pw.ResponseWriter.Write(b)
}

// Flush keeps streaming handlers working through the wrapper.
func (pw *plateWriter) Flush() {
	if pw.replaced {
		return
	}
	if f, ok := pw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (pw *plateWriter) Unwrap() http.ResponseWriter { return pw.ResponseWriter }

// plate404HTML is the page, set in the Farfield brand system: the plate
// carries the emotion, the type stays precise — a mono observation label, a
// Newsreader headline, one Horizon-orange action. On tall screens the
// artwork holds the bottom at its own scale and a flat field extends its sky,
// so the scene stays distant instead of crop-zoomed. One %s: the versioned
// artwork URL.
const plate404HTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#213854">
<meta name="robots" content="noindex">
<title>404 — not in the catalogue</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@500&family=Inter:wght@400;500&family=Newsreader:opsz,wght@6..72,400&display=swap">
<style>
  *,*::before,*::after{margin:0;padding:0;box-sizing:border-box}
  :root{
    --ff-space:#0e222d;
    --ff-paper:#f3e5d1;
    --ff-horizon:#e59f67;
    --ff-sky:#213854; /* sampled from the plate's upper field */
  }
  html,body{height:100%%;width:100%%}
  body{background:var(--ff-sky);overflow:hidden}
  .plate{
    position:fixed;inset:0;width:100%%;height:100%%;
    object-fit:cover;
    /* Keep the road and the headland observatory when wide screens crop. */
    object-position:60%% 50%%;
  }
  @media (max-aspect-ratio: 9/10){
    body{overflow:auto}
    .plate{
      top:auto;bottom:0;
      height:auto;max-height:68vh;
      object-fit:contain;object-position:bottom;
      -webkit-mask-image:linear-gradient(to bottom,transparent 0,#000 12%%);
      mask-image:linear-gradient(to bottom,transparent 0,#000 12%%);
    }
  }
  .copy{
    /* The right sky is the plate's darkest, quietest field — the galaxy owns
       the left. Type goes where the ink is settled. */
    position:fixed;
    top:clamp(4rem, 16vh, 12rem);
    left:clamp(1.5rem, 60vw, 66vw);
    right:clamp(1.25rem, 5vw, 5rem);
    max-width:34ch;
    display:flex;
    flex-direction:column;
    align-items:flex-start;
    gap:1.3rem;
    color:var(--ff-paper);
    text-shadow:0 1px 2px rgba(14,34,45,.45);
  }
  @media (max-aspect-ratio: 9/10){
    .copy{
      position:static;
      max-width:40ch;
      padding:calc(env(safe-area-inset-top) + 3.5rem) 7vw 0;
    }
  }
  .note{
    font-family:"IBM Plex Mono",ui-monospace,monospace;
    font-size:.72rem;
    font-weight:500;
    letter-spacing:.18em;
    text-transform:uppercase;
    color:#9babb3;
  }
  h1{
    font-family:"Newsreader",Georgia,serif;
    font-weight:400;
    font-size:clamp(2.4rem, 6.5vw, 4.2rem);
    line-height:.98;
    letter-spacing:-.025em;
    text-wrap:balance;
  }
  .lede{
    font-family:"Inter",system-ui,sans-serif;
    font-weight:400;
    font-size:clamp(1rem, 2vw, 1.15rem);
    line-height:1.5;
    color:rgba(243,229,209,.85);
    max-width:32ch;
  }
  .button-primary{
    font-family:"Inter",system-ui,sans-serif;
    font-weight:500;
    font-size:.95rem;
    color:var(--ff-space);
    background:var(--ff-horizon);
    border:1px solid var(--ff-horizon);
    border-radius:4px;
    padding:.6rem 1.2rem;
    text-decoration:none;
    text-shadow:none;
    transition:background 140ms ease-out,border-color 140ms ease-out;
  }
  .button-primary:hover{background:#eeb183;border-color:#eeb183}
  .button-primary:focus-visible{outline:2px solid var(--ff-horizon);outline-offset:2px}
</style>
</head>
<body>
<img class="plate" src="%s" alt="A halftone-print landscape: a figure walks a pale road along a coastal headland toward a hilltop observatory, under a sky filled by a spiral galaxy, two moons and stars.">
<main class="copy">
  <p class="note">Observation 404</p>
  <h1>Not in the catalogue</h1>
  <p class="lede">Nothing is plotted at this address.</p>
  <a class="button-primary" href="/">Return&nbsp;&rarr;</a>
</main>
</body>
</html>
`
