// Command apiperf measures the fleet's read paths the way clients use them:
// latency (median and p95 of N requests), payload size raw and gzipped, and
// whether a revalidation with If-None-Match comes back 304. Point it at the
// dev fleet (make dev) or any profile's private addresses.
//
//	go run ./lib/capability/cmd/apiperf                  # dev fleet, 30 requests each
//	go run ./lib/capability/cmd/apiperf -n 100 -json     # machine-readable
//
// Keys come from FARFIELD_KEY_<APP> (default dev-<app>-key, the dev fleet's).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/fleet"
)

type probe struct{ App, Path string }

var probes = []probe{
	{"content", "/api/collections"},
	{"content", "/api/entries?status=all&limit=50&bodies=0"},
	{"content", "/api/entries?status=all&limit=50"},
	{"content", "/api/series"},
	{"feed", "/api/posts?limit=20"},
	{"blobs", "/blobs?page=1"},
	{"bookmarks", "/api/admin/bookmarks"},
	{"qr", "/api/admin/codes"},
	{"scrap", "/api/admin/pastes?limit=50"},
	{"library", "/api/admin/books"},
	{"sideload", "/api/builds"},
	{"sideload", "/api/admin/shares"},
	{"switchboard", "/api/admin/messages?limit=100"},
	{"backup", "/api/admin/snapshots"},
	{"daily", "/api/photos?page=1"},
	{"apex", "/api/profile"},
	{"content", "/static/editor/dict/en_US.txt"},
}

type result struct {
	App      string  `json:"app"`
	Path     string  `json:"path"`
	Status   int     `json:"status"`
	P50ms    float64 `json:"p50Ms"`
	P95ms    float64 `json:"p95Ms"`
	Bytes    int     `json:"bytes"`
	GzBytes  int     `json:"gzipBytes"`
	ETag     bool    `json:"etag"`
	Revalid  int     `json:"revalidateStatus"`
	CacheCtl string  `json:"cacheControl"`
}

func key(app string) string {
	if k := os.Getenv("FARFIELD_KEY_" + strings.ToUpper(app)); k != "" {
		return k
	}
	return "dev-" + app + "-key"
}

func main() {
	n := flag.Int("n", 30, "requests per endpoint")
	host := flag.String("host", "http://127.0.0.1", "base (each app's registry port is appended)")
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()

	// no transparent decompression: measure what crosses the wire
	hc := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(url, app string, gz bool, inm string) (*http.Response, []byte, time.Duration, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("X-API-Key", key(app))
		req.Header.Set("User-Agent", "farfield-apiperf/1.0")
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		t := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			return nil, nil, 0, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b, time.Since(t), nil
	}

	var out []result
	for _, p := range probes {
		s, ok := fleet.Lookup(p.App)
		if !ok {
			continue
		}
		url := fmt.Sprintf("%s:%d%s", *host, s.Port, p.Path)
		r := result{App: p.App, Path: p.Path}
		var times []float64
		for i := 0; i < *n; i++ {
			resp, b, d, err := do(url, p.App, true, "")
			if err != nil {
				r.Status = -1
				break
			}
			times = append(times, float64(d.Microseconds())/1000)
			if i == 0 {
				r.Status, r.GzBytes = resp.StatusCode, len(b)
				r.CacheCtl = resp.Header.Get("Cache-Control")
				if et := resp.Header.Get("ETag"); et != "" {
					r.ETag = true
					if rv, _, _, err := do(url, p.App, true, et); err == nil {
						r.Revalid = rv.StatusCode
					}
				}
			}
		}
		if _, b, _, err := do(url, p.App, false, ""); err == nil {
			r.Bytes = len(b)
		}
		if len(times) > 0 {
			sort.Float64s(times)
			r.P50ms = times[len(times)/2]
			r.P95ms = times[min(len(times)-1, int(float64(len(times))*0.95))]
		}
		out = append(out, r)
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	fmt.Printf("%-12s %-44s %4s %7s %7s %9s %9s %5s %4s\n", "app", "path", "code", "p50ms", "p95ms", "bytes", "gzip", "etag", "304")
	for _, r := range out {
		et := "-"
		if r.ETag {
			et = "yes"
		}
		rv := "-"
		if r.Revalid != 0 {
			rv = fmt.Sprint(r.Revalid)
		}
		fmt.Printf("%-12s %-44s %4d %7.2f %7.2f %9d %9d %5s %4s\n", r.App, r.Path, r.Status, r.P50ms, r.P95ms, r.Bytes, r.GzBytes, et, rv)
	}
}
