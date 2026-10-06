package web

import (
	"net/http"
	"net/url"
	"strings"
)

// The fleet answers on more than one parent domain: *.farfield.systems
// through the Cloudflare tunnel, and *.iam.casa on the tailnet. A cookie can
// only span one of them, so SESSION_COOKIE_DOMAIN is a comma-separated list
// (".farfield.systems,.iam.casa") and every request is placed under the one
// its host belongs to. A host under none of them (the tailscale serve ports
// on *.ts.net) gets a host-only cookie and its own app's /login — a fleet
// cookie set there would be rejected by the browser and the login would loop.

// canonicalDomain is the parent FleetBase builds URLs on.
const canonicalDomain = "farfield.systems"

// fleetDomains is SESSION_COOKIE_DOMAIN as bare, lowercased domains.
func fleetDomains() []string {
	_, raw := fleetSessionConfig()
	var out []string
	for _, d := range strings.Split(raw, ",") {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "."))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// fleetDomainOf returns the configured fleet domain host is, or sits under —
// "" when it is none of them. The match is on a label boundary, so
// evilfarfield.systems is not under farfield.systems.
func fleetDomainOf(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range fleetDomains() {
		if host == d || strings.HasSuffix(host, "."+d) {
			return d
		}
	}
	return ""
}

// sameFleetDomain reports whether host shares the request's fleet domain —
// the boundary the session cookie actually spans.
func sameFleetDomain(r *http.Request, host string) bool {
	d := fleetDomainOf(host)
	return d != "" && d == fleetDomainOf(requestHostname(r))
}

// cookieDomainFor is the Domain attribute a session cookie set on r carries:
// the request's fleet domain, or "" (host-only) off the fleet's domains.
func cookieDomainFor(r *http.Request) string {
	if d := fleetDomainOf(requestHostname(r)); d != "" {
		return "." + d
	}
	return ""
}

// RebaseFleetURL moves an absolute URL on one fleet domain onto the domain r
// arrived on, so a page reached on the tailnet links to its siblings on the
// tailnet: https://feed.farfield.systems/x seen from content.iam.casa becomes
// https://feed.iam.casa/x. Anything else — a relative URL, a host outside the
// fleet, a request outside the fleet — comes back unchanged.
func RebaseFleetURL(r *http.Request, raw string) string {
	to := fleetDomainOf(requestHostname(r))
	if to == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host, port := u.Hostname(), u.Port()
	from := fleetDomainOf(host)
	if from == "" || from == to {
		return raw
	}
	u.Host = strings.TrimSuffix(host, from) + to
	if port != "" {
		u.Host += ":" + port
	}
	return u.String()
}
