package capability

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Arg is one positional parameter of a command.
//
// The grammar is deliberately loose, because its first audience is someone
// typing one-handed on a phone. There are no flags and no required quoting: args
// are whitespace-separated, and the last one usually carries Rest so the tail of
// the message arrives intact, spaces and newlines and all. `/bm <url> [category]`
// and `/qr <target> [label]` are the same shape as the old hand-rolled parser,
// which is the point — the syntax people already use had to survive being
// described as data.
type Arg struct {
	Name     string
	Optional bool
	// Rest captures everything remaining rather than one token. Only meaningful
	// on the final arg; Registry.Add rejects it anywhere else.
	Rest bool
}

// Invocation is one parsed call: the bound arguments plus anything that rode
// along with the message.
type Invocation struct {
	// Raw is the untouched argument text, for commands that want it whole.
	Raw string
	// Args holds each declared Arg by name. A missing optional arg is "".
	Args map[string]string
	// Files are attachments — photos from a text, or files named on the CLI.
	Files []NamedFile
	// Actor identifies who is calling (a normalized handle, or "cli"). Commands
	// that act on "the last thing I did" scope by it so one person's history can
	// never resolve to somebody else's.
	Actor string
}

// Arg returns a bound argument, or "" when it was optional and absent.
func (in Invocation) Arg(name string) string { return in.Args[name] }

// Result is one command's outcome. Text is what a human is told; Image carries a
// raster when the picture *is* the answer, as it is for a QR code.
type Result struct {
	Ref       string
	Text      string
	Image     []byte
	ImageName string
}

// Spec declares one command. Run is the single implementation every surface
// calls — the CLI, switchboard's dispatch, and anything else that grows later.
type Spec struct {
	Name    string
	Aliases []string
	Summary string
	Args    []Arg
	Run     func(context.Context, *Clients, Invocation) (Result, error)
}

// Usage renders the command the way help should show it: `/qr <target> [label]`.
func (s *Spec) Usage() string {
	var b strings.Builder
	b.WriteString("/")
	b.WriteString(s.Name)
	for _, a := range s.Args {
		ellipsis := ""
		if a.Rest {
			ellipsis = "..."
		}
		if a.Optional {
			fmt.Fprintf(&b, " [%s%s]", a.Name, ellipsis)
		} else {
			fmt.Fprintf(&b, " <%s%s>", a.Name, ellipsis)
		}
	}
	return b.String()
}

// Bind parses argument text against the spec.
//
// A missing required argument is an error rather than an empty string, and the
// error quotes the usage — over a text message that reply is the only
// documentation anyone is going to read.
func (s *Spec) Bind(rest string, files []NamedFile) (Invocation, error) {
	in := Invocation{Raw: strings.TrimSpace(rest), Args: map[string]string{}}
	remaining := in.Raw

	for _, a := range s.Args {
		var value string
		if a.Rest {
			value, remaining = remaining, ""
		} else {
			value, remaining = cutField(remaining)
		}
		if value == "" && !a.Optional {
			// Attachments can stand in for a missing body: a bare photo is a
			// complete /feed, and demanding text for it would be pedantic.
			if len(files) > 0 && a.Rest {
				in.Args[a.Name] = ""
				continue
			}
			return in, fmt.Errorf("usage: %s", s.Usage())
		}
		in.Args[a.Name] = value
	}
	in.Files = files
	return in, nil
}

// cutField splits off the first whitespace-delimited token.
func cutField(s string) (first, rest string) {
	s = strings.TrimSpace(s)
	first, rest, _ = strings.Cut(s, " ")
	return strings.TrimSpace(first), strings.TrimSpace(rest)
}

// Registry is the set of commands a surface exposes.
//
// It is not a single global: switchboard adds commands that only make sense with
// a message log behind them (/undo, /append), and the CLI does not carry those.
// The shared fleet commands come from Fleet(); each surface layers its own on
// top and gets help generated over whatever it actually has.
type Registry struct {
	specs  []*Spec
	byName map[string]*Spec
}

// NewRegistry builds a registry. It panics on a malformed or duplicate spec,
// which is a programmer error caught at startup rather than on a phone.
func NewRegistry(specs ...*Spec) *Registry {
	r := &Registry{byName: map[string]*Spec{}}
	r.Add(specs...)
	return r
}

// Add registers commands.
func (r *Registry) Add(specs ...*Spec) {
	for _, s := range specs {
		if s.Name == "" {
			panic("capability: spec with no name")
		}
		for i, a := range s.Args {
			if a.Rest && i != len(s.Args)-1 {
				panic("capability: " + s.Name + ": Rest arg " + a.Name + " is not last")
			}
		}
		for _, name := range append([]string{s.Name}, s.Aliases...) {
			if _, dup := r.byName[name]; dup {
				panic("capability: duplicate command " + name)
			}
			r.byName[name] = s
		}
		r.specs = append(r.specs, s)
	}
}

// Lookup finds a command by name or alias.
func (r *Registry) Lookup(name string) (*Spec, bool) {
	s, ok := r.byName[strings.ToLower(strings.TrimSpace(name))]
	return s, ok
}

// Specs returns the registered commands in declaration order.
func (r *Registry) Specs() []*Spec {
	out := make([]*Spec, len(r.specs))
	copy(out, r.specs)
	return out
}

// Split separates a leading /command from its arguments.
//
// The leading slash is the entire routing rule, and that is the point. Everything
// farfield can do is named explicitly, so a message either says which app it
// meant or it did not mean one — there is no longer a guess in the middle that
// turns a stray thought into a public post.
func Split(text string) (name, rest string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", text, false
	}
	name, rest, _ = strings.Cut(text[1:], " ")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", text, false
	}
	return name, strings.TrimSpace(rest), true
}

// Help renders the command list.
//
// Generated, never written. The string it replaces was a hand-maintained const
// listing eleven commands with nothing checking it against the code, and it had
// already drifted once.
func (r *Registry) Help(header string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n\n")
	}
	width := 0
	usages := make([]string, len(r.specs))
	for i, s := range r.specs {
		usages[i] = s.Usage()
		if len(usages[i]) > width {
			width = len(usages[i])
		}
	}
	for i, s := range r.specs {
		fmt.Fprintf(&b, "%-*s  %s\n", width, usages[i], s.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Names lists every command name, sorted — for shell completion and for the
// drift test that checks the markdown command files against this table.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
