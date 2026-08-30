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
	// Host marks a service that runs on the host as a systemd unit rather
	// than in the compose stack. It is still a farfield service and still
	// answers on Port — it simply is not a container, so the drift tests
	// expect it to be absent from docker-compose.yml rather than present.
	//
	// switchboard is the first: it hands messages to an agent, which means
	// exec'ing a binary and reaching herdr, neither of which a distroless
	// container with one volume can do.
	Host bool
}

// services is the registry, in port order. Two absences are deliberate and
// both are the same story: epochs moved to a standalone Cloudflare Worker, and
// bard and dead-presidents moved to the pure-internet monorepo. They were
// always projects that happened to be hosted here rather than parts of the
// content backend, and 8795/8796 are left unassigned so the ports still read
// as theirs to anyone comparing this list against a running box.
var services = []Service{
	{Name: "content", Port: 8787, Public: "content.farfield.systems"},
	{Name: "feed", Port: 8788, Public: "feed.farfield.systems"},
	{Name: "blobs", Port: 8789, Public: "blobs.farfield.systems"},
	{Name: "apex", Port: 8790, Public: "farfield.systems"},
	{Name: "backup", Port: 8791, Public: ""},
	{Name: "daily", Port: 8792, Public: "daily.farfield.systems"},
	{Name: "bookmarks", Port: 8793, Public: "bookmarks.farfield.systems"},
	{Name: "qr", Port: 8794, Public: "qr.farfield.systems"},
	{Name: "library", Port: 8797, Public: "library.farfield.systems"},
	{Name: "pulse", Port: 8798, Public: "pulse.farfield.systems"},
	{Name: "scrap", Port: 8799, Public: "scrap.farfield.systems"},
	{Name: "sideload", Port: 8800, Public: "sideload.farfield.systems"},
	{Name: "keys", Port: 8801, Public: "keys.farfield.systems"},
	{Name: "switchboard", Port: 8802, Public: "switchboard.farfield.systems", Host: true},
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

// StatusURL is the service's /status probe address for a caller inside the
// fleet.
//
// A container answers to its compose service name. A Host service does not —
// it has no compose DNS entry at all — so it is probed at the address the host
// publishes on, which a container can route to. Getting this wrong does not
// error: the prober simply reports a healthy service as down forever, which is
// how switchboard's move showed up as "13/14 services up".
//
// hostAddr is the fleet's bind address (FARFIELD_BIND_IP — the docker bridge
// gateway in production, loopback in local development). It is ignored for
// services that are containers.
func (s Service) StatusURL(hostAddr string) string {
	if s.Host {
		if hostAddr == "" {
			hostAddr = "127.0.0.1"
		}
		return fmt.Sprintf("http://%s:%d/status", hostAddr, s.Port)
	}
	return fmt.Sprintf("http://%s:%d/status", s.Name, s.Port)
}

// PublicURL is the service's public root, or "" for a tailnet-only service.
func (s Service) PublicURL() string {
	if s.Public == "" {
		return ""
	}
	return "https://" + s.Public + "/"
}

// PublicStatusURL is the service's /status probe address as the public
// internet reaches it — the outside view, where StatusURL is the inside one.
// It is "" for a tailnet-only service, which by definition has no outside.
//
// The external uptime canary derives its whole farfield target list from this
// rather than keeping its own copy, so a service added to the registry is
// watched from off-site on the next run with nothing else to remember. That
// list had been hand-maintained and covered four of the thirteen public
// services, which is the same drift this package exists to prevent.
func (s Service) PublicStatusURL() string {
	if s.Public == "" {
		return ""
	}
	return "https://" + s.Public + "/status"
}

// PublicServices returns every service with a public face, in port order.
func PublicServices() []Service {
	out := []Service{}
	for _, s := range services {
		if s.Public != "" {
			out = append(out, s)
		}
	}
	return out
}
