package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/fleet"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates/status.html
var statusFS embed.FS

// The probe list comes from lib/fleet — the single source of truth for what
// farfield is. apex probes each sibling's /status directly on the compose
// network, container to container, so the page reports what is actually
// running rather than what the tunnel happens to pass. apex itself is never
// probed: if this handler is running, apex is up.

// observation is one probed service, rendered as a row.
type observation struct {
	Name      string
	Public    string
	OK        bool
	LatencyMS int64
}

// fleetReport is the status page's template context.
type fleetReport struct {
	Observations []observation
	Up, Total    int
	ProbedAt     string
}

// probeClient keeps probes brisk: a hung sibling reads as down in two
// seconds, it never stalls the page.
var probeClient = &http.Client{Timeout: 2 * time.Second}

// probeFleet checks every service concurrently and returns rows sorted by
// name. A service is up iff its /status answers 200 inside the timeout.
func probeFleet() fleetReport {
	services := fleet.Services()
	obs := make([]observation, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		if svc.Name == "apex" { // the prober itself
			obs[i] = observation{Name: svc.Name, Public: svc.Public, OK: true}
			continue
		}
		wg.Add(1)
		go func(i int, svc fleet.Service) {
			defer wg.Done()
			start := time.Now()
			resp, err := probeClient.Get(svc.InternalStatusURL())
			o := observation{Name: svc.Name, Public: svc.Public,
				LatencyMS: time.Since(start).Milliseconds()}
			if err == nil {
				resp.Body.Close()
				o.OK = resp.StatusCode == http.StatusOK
			}
			obs[i] = o
		}(i, svc)
	}
	wg.Wait()
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].Name < obs[j].Name })

	r := fleetReport{Observations: obs, Total: len(obs), ProbedAt: store.NowRFC3339()}
	for _, o := range obs {
		if o.OK {
			r.Up++
		}
	}
	return r
}

// statusPage renders the branded fleet observation for browsers, caching the
// probe sweep briefly so a burst of views costs one fan-out, not many. The
// JSON contract on the same path is untouched — Docker healthchecks, pulse
// probes, and curl all negotiate right past this.
type statusPage struct {
	tmpl *template.Template

	mu       sync.Mutex
	cached   fleetReport
	cachedAt time.Time
}

const statusCacheTTL = 15 * time.Second

func newStatusPage() (*statusPage, error) {
	// Stamp the shared asset version into the source once, before parsing —
	// cheaper than rewriting every rendered response, and it keeps the
	// template itself free of a value only the binary knows.
	raw, err := statusFS.ReadFile("templates/status.html")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("status.html").Parse(string(theme.StampAssets(raw)))
	if err != nil {
		return nil, err
	}
	return &statusPage{tmpl: tmpl}, nil
}

// report returns the cached sweep, refreshing it when stale. The lock is held
// across the probe on purpose: concurrent viewers queue behind one two-second
// sweep instead of each fanning out fifteen probes of their own.
func (sp *statusPage) report() fleetReport {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if time.Since(sp.cachedAt) > statusCacheTTL {
		sp.cached, sp.cachedAt = probeFleet(), time.Now()
	}
	return sp.cached
}

func (sp *statusPage) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		web.WriteJSON(w, http.StatusOK, map[string]any{"service": "apex", "ok": true})
		return
	}
	var buf bytes.Buffer
	if err := sp.tmpl.Execute(&buf, sp.report()); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not render status")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}
