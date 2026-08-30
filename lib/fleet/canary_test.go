package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The external uptime canary watches the fleet from a GitHub runner, which is
// the one vantage point that survives the box dying. It used to carry its own
// hand-written list of four URLs, and the fleet grew to thirteen public
// services around it — so nine services had no off-site watcher at all, and
// nothing anywhere said so. That is the same drift this package was created to
// stop, arriving by the same road: a second copy of the list.
//
// The canary now derives its farfield targets from the registry at run time.
// These tests keep it that way, because the failure is silent: a canary
// watching a stale subset still reports green, and green is exactly what it
// would report if it were watching everything.

var (
	// A probe URL aimed at the fleet's own domain. Comments may say
	// "farfield.systems"; a hardcoded https:// target is the thing that rots.
	hardcodedFleetURLRe = regexp.MustCompile(`https://[a-z0-9.-]*farfield\.systems`)
	uptimeWorkflowPath  = filepath.Join(".github", "workflows", "uptime.yml")
)

func canaryWorkflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), uptimeWorkflowPath))
	if err != nil {
		t.Skipf("uptime workflow not readable: %v", err)
	}
	return string(raw)
}

// TestCanaryDerivesFleetTargets fails when the canary hardcodes a farfield URL
// instead of asking the registry, which is how it silently fell nine services
// behind the fleet.
func TestCanaryDerivesFleetTargets(t *testing.T) {
	body := canaryWorkflow(t)

	if found := hardcodedFleetURLRe.FindAllString(body, -1); len(found) > 0 {
		t.Errorf("%s hardcodes %d farfield URL(s) — %v — instead of deriving them "+
			"from the registry; a second copy of the list is a copy that drifts, and a "+
			"canary watching a stale subset reports green either way",
			uptimeWorkflowPath, len(found), found)
	}

	if !strings.Contains(body, "cmd/publictargets") {
		t.Errorf("%s does not invoke lib/fleet/cmd/publictargets — nothing is deriving "+
			"the fleet's target list, so a newly registered service gets no off-site watcher",
			uptimeWorkflowPath)
	}
}

// TestCanaryWatchesEveryPublicService is the same guarantee stated forwards:
// every registered public service must reach the canary through derivation.
// It asserts the generator is complete rather than the workflow's text, so it
// still holds if the derivation step is reshaped.
func TestCanaryWatchesEveryPublicService(t *testing.T) {
	derived := map[string]bool{}
	for _, s := range PublicServices() {
		if s.PublicStatusURL() == "" {
			t.Errorf("%s is public but has no public status URL", s.Name)
			continue
		}
		derived[s.Name] = true
	}

	for _, s := range Services() {
		if s.Public == "" {
			if derived[s.Name] {
				t.Errorf("%s has no public host but was offered to the canary as a public target", s.Name)
			}
			continue
		}
		if !derived[s.Name] {
			t.Errorf("%s is publicly reachable at %s but the canary would never probe it", s.Name, s.Public)
		}
	}

	if len(derived) == 0 {
		t.Fatal("no public services derived at all — the canary would probe nothing and pass")
	}
}

// TestPublicStatusURLShape pins the probe address, since a canary pointed at
// the wrong path fails open: a 404 body has no "ok":true either, but so does
// an outage, and the two must not look alike.
func TestPublicStatusURLShape(t *testing.T) {
	apex, ok := Lookup("apex")
	if !ok {
		t.Fatal("apex missing from the registry")
	}
	if got, want := apex.PublicStatusURL(), "https://farfield.systems/status"; got != want {
		t.Errorf("apex public status URL = %q, want %q", got, want)
	}

	backup, ok := Lookup("backup")
	if !ok {
		t.Fatal("backup missing from the registry")
	}
	if got := backup.PublicStatusURL(); got != "" {
		t.Errorf("tailnet-only backup has public status URL %q, want empty", got)
	}
}
