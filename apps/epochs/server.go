package main

import (
	"embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var tmplFS embed.FS

// Indices of the epochs the page names directly. The original addressed them
// positionally — _(6), _(5), _(4) in the headline; _(7) down to _(3) as the
// milestone columns — so the names can change on-chain without touching this.
const (
	idxBloom      = 3
	idxEpisode    = 4
	idxPhase      = 5
	idxSeason     = 6
	idxRevolution = 7
)

// milestoneCols are the epoch columns of the Milestones table, in the order
// the original printed them: Revolution, Season, Phase, Episode, Bloom.
var milestoneCols = []int{idxRevolution, idxSeason, idxPhase, idxEpisode, idxBloom}

// Diagram geometry, transcribed from the original SVG. Twelve rows of eleven
// circles: r=8, cy=10, cx=8+20n+2, each row offset 20px down.
const (
	diagramHeight  = 220
	diagramRowStep = 20
	circleRadius   = 8
	circleCY       = 10
)

// Circle is one node of the diagram; Filled marks the epoch's current value.
type Circle struct {
	CX     int
	Filled bool
}

// DiagramRow is one epoch's row of eleven circles.
type DiagramRow struct {
	Label   string
	Y       int
	Circles []Circle
}

// Diagram builds the concentric-epoch figure. Rows run Aeon-first, matching
// the original's Object.values(epochs).reverse().
//
// Note the height: 12 rows at 20px each is 240, but the original's <svg> was
// 220 tall and SVG clips by default, so the final row — Block, the one that
// changes every twelve seconds — was cut off. That is reproduced here rather
// than corrected: it is what the page looked like. Raising diagramHeight to
// 240 reveals the twelfth row.
func Diagram(r Reading) []DiagramRow {
	rows := make([]DiagramRow, 0, Count)
	for i, e := range r.Rows { // r.Rows is already reversed
		circles := make([]Circle, 0, 11)
		for n := 0; n < 11; n++ {
			circles = append(circles, Circle{
				CX: 8 + diagramRowStep*n + 2,
				// fill={f-1===n ? "black" : "none"} — values are 1-indexed.
				Filled: e.Value-1 == uint64(n),
			})
		}
		rows = append(rows, DiagramRow{Label: e.Label, Y: i * diagramRowStep, Circles: circles})
	}
	return rows
}

// commas renders an integer the way toLocaleString("en") does.
func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// templateFuncs are the helpers the page needs. Keeping the arithmetic in Go
// leaves the template as markup.
var templateFuncs = template.FuncMap{
	"commas": commas,
	"cols":   func() []int { return milestoneCols },
}

// Server renders the page and answers the live-update endpoint.
type Server struct {
	state *State

	// publicURL is where this deployment is reachable, used for canonical and
	// og:url. It is this app's own address, never the original's — a canonical
	// pointing at a dead domain would be worse than none.
	publicURL string

	// fontsDir, when set, is a directory holding the original woff2 files.
	// New Spirit and Söhne Mono are licensed and cannot be redistributed, so
	// they are opt-in: point EPOCHS_FONTS_DIR at a directory containing them
	// and the page serves and declares them. Left unset, the @font-face rules
	// are omitted entirely rather than emitted to 404 on every load, and the
	// system serif/mono fallbacks carry the design.
	fontsDir string

	rend *web.Renderer
}

// NewServer parses the templates and returns the handler set. fontsDir may be
// empty.
func NewServer(state *State, publicURL, fontsDir string) (*Server, error) {
	pages, err := web.ParseTemplates(tmplFS, templateFuncs)
	if err != nil {
		return nil, err
	}
	return &Server{
		state:     state,
		publicURL: strings.TrimSuffix(publicURL, "/"),
		fontsDir:  fontsDir,
		// The app defines its own "base" in templates/base.html, so the shared
		// farfield shell is overridden wholesale — this page is a reproduction,
		// not a fleet UI. App/Mark still feed the title and favicon.
		rend: &web.Renderer{
			Templates: pages,
			App:       "epochs",
			Mark:      "ep",
			Funcs:     templateFuncs,
		},
	}, nil
}

// Routes wires the handlers.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/current", s.current)
	mux.HandleFunc("GET /status", s.status)
	if s.fontsDir != "" {
		mux.Handle("GET /fonts/", http.StripPrefix("/fonts/", s.fonts()))
	}
	return mux
}

// fonts serves the licensed webfonts from disk. http.Dir already refuses path
// traversal; the extension check narrows it further so a misconfigured
// EPOCHS_FONTS_DIR cannot turn into a general file server.
func (s *Server) fonts() http.Handler {
	files := http.FileServer(http.Dir(s.fontsDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".woff2") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
}

// firstPaintWait is how long a request will wait for the very first reading
// before falling back to the loading state. It only ever applies on a cold
// start with no cached snapshot: one round trip to an RPC endpoint is quicker
// than a visitor noticing a "Loading" page and reloading it.
const firstPaintWait = 2500 * time.Millisecond

// index renders the page. Before the first successful poll it waits briefly,
// then falls back to the original's loading state rather than a page of zeroes.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	reading, ok := s.state.Wait(r.Context(), firstPaintWait)
	if !ok {
		// The page is about to change, so let clients revalidate rather than
		// cache the empty state.
		w.Header().Set("Cache-Control", "no-store")
		s.rend.Render(w, "index.html", map[string]any{
			"Ready":     false,
			"Fonts":     s.fontsDir != "",
			"PublicURL": s.publicURL,
		})
		return
	}

	// One block of freshness: the page is regenerated on the poll interval and
	// there is nothing user-specific in it.
	w.Header().Set("Cache-Control", "public, max-age=12")
	s.rend.Render(w, "index.html", map[string]any{
		"Ready":      true,
		"Fonts":      s.fontsDir != "",
		"PublicURL":  s.publicURL,
		"Reading":    reading,
		"Block":      commas(reading.Block),
		"Diagram":    Diagram(reading),
		"Height":     diagramHeight,
		"Radius":     circleRadius,
		"CY":         circleCY,
		"System":     SystemTable(reading.Labels),
		"Milestones": MilestoneRows(reading),
		"Cols":       milestoneCols,
		"Contract":   Contract,
		"BlockTime":  BlockTime,
	})
}

// currentPayload is the shape /api/current returns — enough for the page's
// poller to update in place without a reload.
type currentPayload struct {
	Block   uint64        `json:"block"`
	Display string        `json:"display"`
	Epochs  [Count]uint64 `json:"epochs"`
	Labels  [Count]string `json:"labels"`
	Live    bool          `json:"live"`
}

// current serves the live reading as JSON. It is public and read-only, like
// the contract behind it.
func (s *Server) current(w http.ResponseWriter, r *http.Request) {
	reading, ok := s.state.Current()
	if !ok {
		web.WriteError(w, http.StatusServiceUnavailable, "no reading yet")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	web.WriteJSON(w, http.StatusOK, currentPayload{
		Block:   reading.Block,
		Display: commas(reading.Block),
		Epochs:  reading.Epochs,
		Labels:  reading.Labels,
		Live:    reading.Live,
	})
}

// status backs the container healthcheck. It reports ok as long as the process
// is serving: a wedged upstream RPC is visible in the payload but must not take
// the container down, because the page still renders the last known reading.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	reading, ok := s.state.Current()
	payload := map[string]any{
		"service":  "epochs",
		"ok":       true,
		"contract": Contract,
		"reading":  ok,
		"live":     ok && reading.Live,
		// Which provider is actually answering. With failover this is the
		// only way to tell that a configured first choice is being skipped.
		"rpc": s.state.Endpoint(),
	}
	if ok {
		payload["block"] = reading.Block
	}
	if err := s.state.Err(); err != nil {
		payload["rpc_error"] = err.Error()
	}
	web.WriteJSON(w, http.StatusOK, payload)
}
