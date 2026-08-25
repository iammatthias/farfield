package capability

import (
	"strings"
	"testing"
)

// TestSplitRequiresLeadingSlash pins the routing rule the whole design rests on.
//
// The cases that used to route somewhere are the interesting ones: a plain
// thought became a feed post and a bare URL became a bookmark, both by guessing.
// Neither is a command now, and both must fall through to the agent — if this
// test ever goes green on those, capture-by-default has come back and a stray
// message can publish itself again.
func TestSplitRequiresLeadingSlash(t *testing.T) {
	commands := []struct{ in, name, rest string }{
		{"/qr https://example.com", "qr", "https://example.com"},
		{"/qr https://example.com my label", "qr", "https://example.com my label"},
		{"/status", "status", ""},
		{"  /STATUS  ", "status", ""},
		{"/feed walked the long way home #life", "feed", "walked the long way home #life"},
	}
	for _, tc := range commands {
		name, rest, ok := Split(tc.in)
		if !ok {
			t.Errorf("Split(%q): not recognized as a command", tc.in)
			continue
		}
		if name != tc.name || rest != tc.rest {
			t.Errorf("Split(%q) = (%q, %q), want (%q, %q)", tc.in, name, rest, tc.name, tc.rest)
		}
	}

	notCommands := []string{
		"walked the long way home",
		"https://example.com",
		"can you redeploy content",
		"",
		"   ",
		"/",
		"what does / mean",
	}
	for _, in := range notCommands {
		if _, _, ok := Split(in); ok {
			t.Errorf("Split(%q): treated as a command; it must reach the agent", in)
		}
	}
}

// TestBindPositionalAndRest covers the grammar someone types one-handed.
func TestBindPositionalAndRest(t *testing.T) {
	qr := &Spec{Name: "qr", Args: []Arg{{Name: "target"}, {Name: "label", Optional: true, Rest: true}}}

	in, err := qr.Bind("https://example.com a long label with spaces", nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := in.Arg("target"); got != "https://example.com" {
		t.Errorf("target = %q", got)
	}
	// Rest keeps the tail whole. Splitting it would have made multi-word labels
	// impossible without quoting, which is not a thing anyone does in Messages.
	if got := in.Arg("label"); got != "a long label with spaces" {
		t.Errorf("label = %q", got)
	}

	// An absent optional arg is empty, not an error.
	in, err = qr.Bind("https://example.com", nil)
	if err != nil {
		t.Fatalf("Bind without label: %v", err)
	}
	if in.Arg("label") != "" {
		t.Errorf("label = %q, want empty", in.Arg("label"))
	}

	// A missing required arg answers with the usage, because over a text message
	// that reply is the only documentation anyone reads.
	if _, err := qr.Bind("", nil); err == nil {
		t.Error("Bind with no target: want an error")
	} else if !strings.Contains(err.Error(), "/qr <target> [label...]") {
		t.Errorf("error %q does not quote the usage", err)
	}
}

// TestBindAcceptsAttachmentInsteadOfText: a bare photo is a complete /feed.
func TestBindAcceptsAttachmentInsteadOfText(t *testing.T) {
	feed := &Spec{Name: "feed", Args: []Arg{{Name: "text", Rest: true}}}

	if _, err := feed.Bind("", nil); err == nil {
		t.Error("empty /feed with no attachment: want an error")
	}
	in, err := feed.Bind("", []NamedFile{{Name: "photo.heic"}})
	if err != nil {
		t.Fatalf("/feed with only a photo: %v", err)
	}
	if len(in.Files) != 1 {
		t.Errorf("files = %d, want 1", len(in.Files))
	}
}

func TestUsage(t *testing.T) {
	cases := []struct {
		spec *Spec
		want string
	}{
		{&Spec{Name: "status"}, "/status"},
		{&Spec{Name: "bm", Args: []Arg{{Name: "url"}, {Name: "category", Optional: true, Rest: true}}},
			"/bm <url> [category...]"},
		{&Spec{Name: "feed", Args: []Arg{{Name: "text", Rest: true}}}, "/feed <text...>"},
	}
	for _, tc := range cases {
		if got := tc.spec.Usage(); got != tc.want {
			t.Errorf("Usage() = %q, want %q", got, tc.want)
		}
	}
}

// TestHelpIsGeneratedFromTheTable is the drift guard.
//
// The string this replaced was a hand-maintained const listing eleven commands
// with nothing checking it against the dispatch, and it had already drifted. Help
// is now derived, so the only way to document a command is to register it.
func TestHelpIsGeneratedFromTheTable(t *testing.T) {
	r := NewRegistry(Fleet()...)
	help := r.Help("farfield")
	for _, s := range r.Specs() {
		if !strings.Contains(help, s.Usage()) {
			t.Errorf("help omits %q", s.Usage())
		}
		if s.Summary == "" {
			t.Errorf("%s has no summary, so help would show a bare usage line", s.Name)
		}
		if !strings.Contains(help, s.Summary) {
			t.Errorf("help omits the summary for %q", s.Name)
		}
	}
}

// TestFleetSpecsAreWellFormed catches a spec added without an implementation —
// which would register a command that help advertises and dispatch panics on.
func TestFleetSpecsAreWellFormed(t *testing.T) {
	r := NewRegistry(Fleet()...)
	for _, s := range r.Specs() {
		if s.Run == nil {
			t.Errorf("%s has no Run", s.Name)
		}
		if _, ok := r.Lookup(s.Name); !ok {
			t.Errorf("%s does not resolve by its own name", s.Name)
		}
		for _, alias := range s.Aliases {
			if _, ok := r.Lookup(alias); !ok {
				t.Errorf("%s: alias %q does not resolve", s.Name, alias)
			}
		}
	}
}

// TestRegistryRejectsMisplacedRest: a Rest arg that is not last would silently
// swallow the arguments after it, so it fails loudly at construction instead.
func TestRegistryRejectsMisplacedRest(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("want a panic for a Rest arg that is not last")
		}
	}()
	NewRegistry(&Spec{Name: "bad", Args: []Arg{{Name: "a", Rest: true}, {Name: "b"}}})
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("want a panic for a duplicate command name")
		}
	}()
	NewRegistry(&Spec{Name: "x"}, &Spec{Name: "y", Aliases: []string{"x"}})
}
