package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/fleet"
)

// Target is one service the /status roll-up probes.
type Target struct {
	Name string
	URL  string
}

// Config addresses the fleet. Callers build it explicitly rather than having the
// package read the environment behind their back — switchboard holds its own
// minted per-app tokens under its own variable names, and the CLI holds a
// different set, so there is no single right answer for where a key comes from.
type Config struct {
	FeedURL, FeedKey                        string
	BookmarksURL, BookmarksKey, BookmarkCat string
	ScrapURL, ScrapKey                      string
	QRURL, QRKey, QRPublicURL               string
	PulseURL, PulseKey                      string
	Targets                                 []Target
}

// Clients is everything a command handler may call.
type Clients struct {
	Feed      *FeedClient
	Bookmarks *BookmarksClient
	Scrap     *ScrapClient
	QR        *QRClient
	Pulse     svc
	Targets   []Target
}

// New wires the clients.
func New(cfg Config) *Clients {
	return &Clients{
		Feed: &FeedClient{svc: newSvc(orDefault(cfg.FeedURL, "https://feed.farfield.systems"), cfg.FeedKey)},
		Bookmarks: &BookmarksClient{
			svc:             newSvc(orDefault(cfg.BookmarksURL, "https://bookmarks.farfield.systems"), cfg.BookmarksKey),
			DefaultCategory: orDefault(cfg.BookmarkCat, "unsorted"),
		},
		Scrap: &ScrapClient{svc: newSvc(orDefault(cfg.ScrapURL, "https://scrap.farfield.systems"), cfg.ScrapKey)},
		QR: &QRClient{
			svc:       newSvc(orDefault(cfg.QRURL, "https://qr.farfield.systems"), cfg.QRKey),
			PublicURL: strings.TrimRight(orDefault(cfg.QRPublicURL, "https://qr.farfield.systems"), "/"),
		},
		Pulse:   newSvc(orDefault(cfg.PulseURL, "https://pulse.farfield.systems"), cfg.PulseKey),
		Targets: cfg.Targets,
	}
}

// FromEnv builds a Config from FARFIELD_* variables, for callers that have no
// opinion — the CLI, and anything an agent shells out to.
func FromEnv() Config {
	return Config{
		FeedURL: os.Getenv("FEED_URL"), FeedKey: env("FARFIELD_FEED_KEY", "FEED_API_KEY"),
		BookmarksURL: os.Getenv("BOOKMARKS_URL"), BookmarksKey: env("FARFIELD_BOOKMARKS_KEY", "BOOKMARKS_API_KEY"),
		BookmarkCat: os.Getenv("FARFIELD_BOOKMARK_CATEGORY"),
		ScrapURL:    os.Getenv("SCRAP_URL"), ScrapKey: env("FARFIELD_SCRAP_KEY", "SCRAP_API_KEY"),
		QRURL: os.Getenv("QR_URL"), QRKey: env("FARFIELD_QR_KEY", "QR_API_KEY"),
		QRPublicURL: os.Getenv("QR_PUBLIC_URL"),
		PulseURL:    os.Getenv("PULSE_URL"), PulseKey: env("FARFIELD_PULSE_KEY", "PULSE_READ_KEY"),
		Targets: PublicTargets(),
	}
}

func env(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// PublicTargets probes each service at its public hostname — what a caller off
// the box can reach. Services with no public face are skipped rather than
// reported unreachable.
func PublicTargets() []Target {
	var out []Target
	for _, s := range fleet.Services() {
		if s.Public == "" {
			continue
		}
		out = append(out, Target{Name: s.Name, URL: s.PublicURL()})
	}
	return out
}

// ServiceURL is where a caller reaches one farfield service.
//
// The port comes from lib/fleet rather than a literal, because a literal is a
// second copy of the registry that nothing checks. The host is a parameter for
// the same reason HostTargets takes one: inside the compose network a service
// answers to its own name, and outside it answers on the address the container
// publishes — which is the docker0 gateway, not loopback. Getting that wrong
// fails as "connection refused" against a service that is perfectly healthy.
func ServiceURL(name, host string) string {
	svc, ok := fleet.Lookup(name)
	if !ok {
		return ""
	}
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, svc.Port)
}

// HostTargets probes each service at the address the containers publish on.
// This is the view from the box itself, for a caller that is not in the compose
// network and so cannot resolve service names.
//
// The address is a parameter rather than loopback because it is not loopback in
// production: caddy reaches apps through the docker0 gateway, so the fleet
// publishes on FARFIELD_BIND_IP (172.17.0.1 on the homelab) and binding
// 127.0.0.1 there would make every probe fail while the fleet was perfectly
// healthy.
func HostTargets(bindIP string) []Target {
	if strings.TrimSpace(bindIP) == "" {
		bindIP = "127.0.0.1"
	}
	var out []Target
	for _, s := range fleet.Services() {
		out = append(out, Target{Name: s.Name, URL: fmt.Sprintf("http://%s:%d", bindIP, s.Port)})
	}
	return out
}

// ── the fleet command table ────────────────────────────────────────────────

// Fleet returns the commands every surface shares.
//
// Commands that need state a surface owns are not here: /undo and /append mean
// "the last thing I did", which only exists where there is a message log, so
// switchboard registers those itself on top of this set.
func Fleet() []*Spec {
	return []*Spec{
		{
			Name: "feed", Aliases: []string{"post"},
			Summary: "post to the feed; trailing #hashtags become tags",
			Args:    []Arg{{Name: "text", Rest: true}},
			Run:     runFeed,
		},
		{
			Name: "bm", Aliases: []string{"bookmark", "link"},
			Summary: "save a link",
			Args:    []Arg{{Name: "url"}, {Name: "category", Optional: true, Rest: true}},
			Run:     runBookmark,
		},
		{
			Name: "scrap", Aliases: []string{"paste"},
			Summary: "paste text, get an unlisted link back",
			Args:    []Arg{{Name: "text", Rest: true}},
			Run:     runScrap,
		},
		{
			Name:    "qr",
			Summary: "make a QR code and return the picture",
			Args:    []Arg{{Name: "target"}, {Name: "label", Optional: true, Rest: true}},
			Run:     runQR,
		},
		{
			Name:    "status",
			Summary: "fleet health",
			Run:     runStatus,
		},
		{
			Name:    "pulse",
			Summary: "traffic and open incidents",
			Run:     runPulse,
		},
	}
}

func runFeed(ctx context.Context, c *Clients, in Invocation) (Result, error) {
	body, tags := ExtractTags(in.Arg("text"))
	if body == "" && len(in.Files) == 0 {
		return Result{}, fmt.Errorf("nothing to post")
	}
	slug, err := c.Feed.CreatePost(ctx, body, tags, in.Files)
	if err != nil {
		return Result{}, err
	}
	text := "posted · " + slug
	if n := len(in.Files); n > 0 {
		text = fmt.Sprintf("posted · %s · %d photo%s", slug, n, plural(n))
	}
	return Result{Ref: slug, Text: text}, nil
}

func runBookmark(ctx context.Context, c *Clients, in Invocation) (Result, error) {
	link := in.Arg("url")
	if !IsURL(link) {
		return Result{}, fmt.Errorf("that is not a URL")
	}
	id, err := c.Bookmarks.Create(ctx, link, in.Arg("category"))
	if err != nil {
		return Result{}, err
	}
	// bookmarks fetches OpenGraph metadata after answering, so the title is not
	// available yet — say what happened rather than echo an empty title.
	return Result{Ref: id, Text: "bookmarked · " + id}, nil
}

func runScrap(ctx context.Context, c *Clients, in Invocation) (Result, error) {
	text := in.Arg("text")
	if strings.TrimSpace(text) == "" {
		return Result{}, fmt.Errorf("nothing to paste")
	}
	pasteURL, id, err := c.Scrap.Create(ctx, text)
	if err != nil {
		return Result{}, err
	}
	return Result{Ref: id, Text: pasteURL}, nil
}

func runQR(ctx context.Context, c *Clients, in Invocation) (Result, error) {
	target := in.Arg("target")
	if strings.TrimSpace(target) == "" {
		return Result{}, fmt.Errorf("give me something to encode")
	}
	id, err := c.QR.Create(ctx, target, in.Arg("label"))
	if err != nil {
		return Result{}, err
	}
	// The picture is the whole point of asking for a QR code, so return it and
	// nothing else — a URL beside it is noise you would have to open. The link is
	// the fallback, not the default: it appears only when the raster could not be
	// fetched, so a failure still leaves something usable.
	res := Result{Ref: id}
	png, err := c.QR.PNG(ctx, id)
	if err != nil {
		res.Text = c.QR.PublicURL + "/qr/" + id + ".png"
		return res, nil
	}
	res.Image, res.ImageName = png, id+".png"
	return res, nil
}

func runStatus(ctx context.Context, c *Clients, _ Invocation) (Result, error) {
	return Result{Text: c.FleetStatus(ctx)}, nil
}

func runPulse(ctx context.Context, c *Clients, _ Invocation) (Result, error) {
	summary, err := c.PulseSummary(ctx)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: summary}, nil
}

// ── status probes ──────────────────────────────────────────────────────────

// statusClient is separate from the service clients: status probes should give
// up fast so one hung service cannot stall the whole report.
var statusClient = &http.Client{Timeout: 5 * time.Second}

// FleetStatus probes every target's public /status concurrently and renders a
// compact report. It needs no credentials: /status is public on every farfield
// app precisely so it can be polled.
func (c *Clients) FleetStatus(ctx context.Context) string {
	if len(c.Targets) == 0 {
		return "no targets configured"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type result struct {
		name string
		ok   bool
		note string
	}
	results := make([]result, len(c.Targets))
	var wg sync.WaitGroup
	for i, t := range c.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = result{name: t.Name}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimRight(t.URL, "/")+"/status", nil)
			if err != nil {
				results[i].note = "bad url"
				return
			}
			req.Header.Set("User-Agent", UserAgent)
			resp, err := statusClient.Do(req)
			if err != nil {
				results[i].note = "unreachable"
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
			results[i].ok = resp.StatusCode == http.StatusOK
			if !results[i].ok {
				results[i].note = resp.Status
			}
		}()
	}
	wg.Wait()

	var down []string
	up := 0
	for _, r := range results {
		if r.ok {
			up++
			continue
		}
		down = append(down, r.name+" ("+r.note+")")
	}
	sort.Strings(down)
	if len(down) == 0 {
		return fmt.Sprintf("all %d services up", up)
	}
	return fmt.Sprintf("%d/%d up\ndown: %s", up, len(results), strings.Join(down, ", "))
}

// PulseSummary renders today's traffic and any open incidents.
func (c *Clients) PulseSummary(ctx context.Context) (string, error) {
	out, err := c.Pulse.do(ctx, http.MethodGet, "/api/overview", "", nil)
	if err != nil {
		return "", err
	}
	var ov struct {
		Targets []struct {
			Name string `json:"name"`
			Up24 string `json:"up24h"`
			Last *struct {
				OK        bool  `json:"ok"`
				LatencyMS int64 `json:"latencyMs"`
			} `json:"last"`
			Incident *struct {
				OpenedAt string `json:"openedAt"`
				LastErr  string `json:"lastErr"`
			} `json:"incident"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(out, &ov); err != nil {
		return "", err
	}

	up, down := 0, 0
	var lines []string
	for _, t := range ov.Targets {
		if t.Last != nil && t.Last.OK {
			up++
		} else {
			down++
		}
		// Only open incidents get a line — a healthy fleet should answer in one.
		if t.Incident != nil {
			lines = append(lines, fmt.Sprintf("! %s down since %s — %s",
				t.Name, t.Incident.OpenedAt, FirstLine([]byte(t.Incident.LastErr))))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d targets · %d up · %d down", len(ov.Targets), up, down)
	sort.Strings(lines)
	for _, line := range lines {
		b.WriteString("\n" + line)
	}
	return b.String(), nil
}

// InternalTargets probes each service by its compose service name, which is how
// they resolve to one another inside the stack network.
func InternalTargets() []Target {
	var out []Target
	for _, s := range fleet.Services() {
		out = append(out, Target{Name: s.Name, URL: fmt.Sprintf("http://%s:%d", s.Name, s.Port)})
	}
	return out
}
