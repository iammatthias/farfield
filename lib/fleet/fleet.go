// Package fleet is the single source of truth for what farfield is: every
// service, the compose-internal port its HTTP server answers on, and the
// public host it is reachable at. Before this registry existed the same list
// was hand-maintained in four places — the apex status prober, the pulse
// uptime targets, docker-compose.yml, and scripts/devfleet.sh — and they
// drifted (pulse was watching 12 of 15 services when the audit caught it).
//
// Consumers: apex probes Services over the compose network for its status
// page and asserts its docs registry covers every name; pulse seeds uptime
// targets for public services it has never seen; drift tests in this package
// parse docker-compose.yml and devfleet.sh and fail CI when either disagrees
// with the registry. Adding a service means adding it here first — the tests
// then point at everything else that needs to know.
//
// Standard library only.
package fleet

import "fmt"

// Service is one farfield app.
type Service struct {
	// Name is the compose service name, the binary name, and the app's
	// database stem — farfield uses one name per app everywhere.
	Name string
	// Port is the compose-internal port. Every app also publishes it
	// loopback-only on the host, and devfleet uses the same number.
	Port int
	// Public is the hostname the Cloudflare tunnel exposes, "" when the
	// service is tailnet-only and has no public face.
	Public string
}

// services is the registry, in port order. epochs is deliberately absent —
// it moved to a standalone Cloudflare Worker.
var services = []Service{
	{Name: "content", Port: 8787, Public: "content.farfield.systems"},
	{Name: "feed", Port: 8788, Public: "feed.farfield.systems"},
	{Name: "blobs", Port: 8789, Public: "blobs.farfield.systems"},
	{Name: "apex", Port: 8790, Public: "farfield.systems"},
	{Name: "backup", Port: 8791, Public: ""},
	{Name: "daily", Port: 8792, Public: "daily.farfield.systems"},
	{Name: "bookmarks", Port: 8793, Public: "bookmarks.farfield.systems"},
	{Name: "qr", Port: 8794, Public: "qr.farfield.systems"},
	{Name: "bard", Port: 8795, Public: "bard.farfield.systems"},
	{Name: "dead-presidents", Port: 8796, Public: "dead-presidents.farfield.systems"},
	{Name: "library", Port: 8797, Public: "library.farfield.systems"},
	{Name: "pulse", Port: 8798, Public: "pulse.farfield.systems"},
	{Name: "scrap", Port: 8799, Public: "scrap.farfield.systems"},
	{Name: "sideload", Port: 8800, Public: "sideload.farfield.systems"},
	{Name: "keys", Port: 8801, Public: "keys.farfield.systems"},
}

// Services returns every service, in port order, as a fresh slice the caller
// may reorder freely.
func Services() []Service {
	out := make([]Service, len(services))
	copy(out, services)
	return out
}

// Lookup returns the named service.
func Lookup(name string) (Service, bool) {
	for _, s := range services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// InternalStatusURL is the service's /status probe address on the compose
// network, where service names resolve as hostnames.
func (s Service) InternalStatusURL() string {
	return fmt.Sprintf("http://%s:%d/status", s.Name, s.Port)
}

// PublicURL is the service's public root, or "" for a tailnet-only service.
func (s Service) PublicURL() string {
	if s.Public == "" {
		return ""
	}
	return "https://" + s.Public + "/"
}
