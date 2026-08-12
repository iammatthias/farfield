package fleet

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRegistryShape: names are unique, ports are unique, every public host
// is either the apex domain or a subdomain of it.
func TestRegistryShape(t *testing.T) {
	names := map[string]bool{}
	ports := map[int]bool{}
	for _, s := range Services() {
		if names[s.Name] {
			t.Errorf("duplicate name %q", s.Name)
		}
		if ports[s.Port] {
			t.Errorf("duplicate port %d", s.Port)
		}
		names[s.Name], ports[s.Port] = true, true
		if s.Public != "" && s.Public != "farfield.systems" &&
			!strings.HasSuffix(s.Public, ".farfield.systems") {
			t.Errorf("%s: public host %q is not under farfield.systems", s.Name, s.Public)
		}
	}
	if _, ok := Lookup("pulse"); !ok {
		t.Error("Lookup(pulse) missed")
	}
}

// TestComposeMatchesRegistry parses docker-compose.yml and fails when a
// compose service is missing from the registry, a registry service is
// missing from compose, or an internal port disagrees. This is the test that
// turns the registry into a source of truth instead of a fifth copy.
func TestComposeMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Skipf("compose file not readable: %v", err)
	}
	// A service block starts at two-space indentation ("  name:"); its port
	// mapping line ends ":<internal>" inside the ports list.
	svcRe := regexp.MustCompile(`(?m)^  ([a-z-]+):\s*$`)
	portRe := regexp.MustCompile(`:(\d+)"`)

	compose := map[string]int{}
	blocks := svcRe.FindAllStringSubmatchIndex(string(raw), -1)
	for i, b := range blocks {
		name := string(raw[b[2]:b[3]])
		end := len(raw)
		if i+1 < len(blocks) {
			end = blocks[i+1][0]
		}
		body := string(raw[b[1]:end])
		if m := portRe.FindStringSubmatch(body); m != nil {
			p, _ := strconv.Atoi(m[1])
			compose[name] = p
		}
	}
	if len(compose) < 10 {
		t.Fatalf("compose parse found only %d services — parser broken?", len(compose))
	}

	for _, s := range Services() {
		got, ok := compose[s.Name]
		if !ok {
			t.Errorf("registry service %q missing from docker-compose.yml", s.Name)
			continue
		}
		if got != s.Port {
			t.Errorf("%s: registry port %d, compose port %d", s.Name, s.Port, got)
		}
		delete(compose, s.Name)
	}
	for name := range compose {
		t.Errorf("compose service %q missing from the fleet registry", name)
	}
}

// TestDevfleetMatchesRegistry parses scripts/devfleet.sh's FLEET line the
// same way.
func TestDevfleetMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/devfleet.sh")
	if err != nil {
		t.Skipf("devfleet.sh not readable: %v", err)
	}
	m := regexp.MustCompile(`FLEET="([^"]+)"`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("devfleet.sh has no FLEET= line")
	}
	dev := map[string]int{}
	for _, pair := range strings.Fields(m[1]) {
		name, portStr, ok := strings.Cut(pair, ":")
		if !ok {
			t.Fatalf("unparseable FLEET pair %q", pair)
		}
		p, _ := strconv.Atoi(portStr)
		dev[name] = p
	}

	for _, s := range Services() {
		got, ok := dev[s.Name]
		if !ok {
			t.Errorf("registry service %q missing from devfleet.sh", s.Name)
			continue
		}
		if got != s.Port {
			t.Errorf("%s: registry port %d, devfleet port %d", s.Name, s.Port, got)
		}
		delete(dev, s.Name)
	}
	for name := range dev {
		t.Errorf("devfleet service %q missing from the fleet registry", name)
	}
}
