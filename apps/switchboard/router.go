package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/capability"
)

// appendWindow bounds /append and /tags. Past it, "the last post" stops meaning
// what the sender thinks it means — a week-old post is not what someone
// appending a line has in mind — so the command refuses rather than guesses.
const appendWindow = 15 * time.Minute

// routeNone marks a message that was recorded but acted on by nothing. Every
// other value in the `route` column is a command name from the registry, so the
// audit vocabulary and the command table cannot drift apart.
const routeNone = "none"

// routeResult is one dispatched command's outcome.
type routeResult struct {
	route     string
	ref       string // id of the record created or changed
	reply     string // text sent back into the thread
	image     []byte // optional image reply (a QR code)
	imageName string
	err       error
}

func failed(route string, err error) routeResult {
	return routeResult{route: route, reply: "✗ " + err.Error(), err: err}
}

// commands builds the command table this switchboard answers.
//
// The fleet half comes from lib/capability and is identical everywhere — the
// same declaration backs the `farfield` CLI and the markdown slash commands
// agents load. The half added here needs a message log to mean anything: /undo
// and /append are both "the last thing I did", which only exists where inbound
// messages are recorded.
func (s *Server) commands() *capability.Registry {
	reg := capability.NewRegistry(capability.Fleet()...)
	reg.Add(
		&capability.Spec{
			Name: "append", Aliases: []string{"+"},
			Summary: "add a line to your last post",
			Args:    []capability.Arg{{Name: "text", Rest: true}},
			Run:     s.runAppend,
		},
		&capability.Spec{
			Name:    "tags",
			Summary: "replace the tags on your last post",
			Args:    []capability.Arg{{Name: "list", Optional: true, Rest: true}},
			Run:     s.runTags,
		},
		&capability.Spec{
			Name:    "undo",
			Summary: "reverse your last action",
			Run:     s.runUndo,
		},
	)
	// Registered last and closing over the finished registry, so help describes
	// whatever this surface actually has rather than a list maintained beside it.
	reg.Add(&capability.Spec{
		Name: "help", Aliases: []string{"?"},
		Summary: "this list",
		Run: func(context.Context, *capability.Clients, capability.Invocation) (capability.Result, error) {
			return capability.Result{Text: reg.Help(helpHeader)}, nil
		},
	})
	return reg
}

const helpHeader = "farfield switchboard"

// route decides what an inbound message meant and carries it out.
//
// A leading slash names a command explicitly. Anything else falls through to
// implicitFallback, which is the last of the old guessing behaviour and is
// scheduled to become the agent handoff — at that point a message either names a
// command or is conversation, with nothing in between.
func (s *Server) route(ctx context.Context, rec *Message, text string, atts []attachment) routeResult {
	if name, rest, ok := capability.Split(text); ok {
		spec, found := s.reg.Lookup(name)
		if !found {
			// An unknown slash command is not an error worth a stack trace; it is
			// someone misremembering the name.
			return failed(routeNone, fmt.Errorf("no such command: /%s — try /help", name))
		}
		return s.dispatch(ctx, rec, spec, rest, atts)
	}
	return s.implicitFallback(ctx, rec, text, atts)
}

// dispatch binds arguments and runs one command.
func (s *Server) dispatch(ctx context.Context, rec *Message, spec *capability.Spec, rest string, atts []attachment) routeResult {
	files, err := s.fetchAttachments(ctx, atts)
	if err != nil {
		return failed(spec.Name, err)
	}
	in, err := spec.Bind(rest, files)
	if err != nil {
		return failed(spec.Name, err)
	}
	in.Actor = rec.Sender

	res, err := spec.Run(ctx, s.caps, in)
	if err != nil {
		return failed(spec.Name, err)
	}
	return routeResult{
		route: spec.Name, ref: res.Ref, reply: res.Text,
		image: res.Image, imageName: res.ImageName,
	}
}

// implicitFallback handles a message that named no command.
//
// This is the behaviour the slash-command rule replaces: a bare link became a
// bookmark and anything else became a feed post, both by guessing. It stays only
// until the agent lands, because deleting it first would leave every
// non-command message with nowhere to go.
func (s *Server) implicitFallback(ctx context.Context, rec *Message, text string, atts []attachment) routeResult {
	name := "feed"
	if len(atts) == 0 && capability.IsURL(text) {
		name = "bm"
	}
	spec, ok := s.reg.Lookup(name)
	if !ok {
		return failed(routeNone, fmt.Errorf("no %s command registered", name))
	}
	return s.dispatch(ctx, rec, spec, text, atts)
}

// fetchAttachments pulls inbound photo bytes off the line.
//
// Photos ride to feed as bytes and feed owns the blob upload, because feed owns
// the post that references them. switchboard never talks to blobs.
func (s *Server) fetchAttachments(ctx context.Context, atts []attachment) ([]capability.NamedFile, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	files := make([]capability.NamedFile, 0, len(atts))
	for _, a := range atts {
		data, err := s.photon.Download(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("could not fetch %s: %w", displayName(a), err)
		}
		files = append(files, capability.NamedFile{Name: displayName(a), Mime: a.MimeType, Data: data})
	}
	return files, nil
}

// ── commands that need the message log ─────────────────────────────────────

// runUndo reverses the sender's last mutating action, whichever service owned it.
func (s *Server) runUndo(ctx context.Context, c *capability.Clients, in capability.Invocation) (capability.Result, error) {
	prior, err := lastAction(s.db, in.Actor, "feed", "bm", "scrap", "qr", "append")
	if err != nil {
		return capability.Result{}, err
	}
	if prior == nil || prior.Ref == "" {
		return capability.Result{}, fmt.Errorf("nothing to undo")
	}
	if err := deleteRef(ctx, c, prior.Route, prior.Ref); err != nil {
		return capability.Result{}, err
	}
	return capability.Result{Ref: prior.Ref,
		Text: fmt.Sprintf("undone · %s %s", prior.Route, prior.Ref)}, nil
}

// deleteRef removes a record from whichever service created it.
func deleteRef(ctx context.Context, c *capability.Clients, route, ref string) error {
	switch route {
	case "feed", "append":
		return c.Feed.DeletePost(ctx, ref)
	case "bm":
		return c.Bookmarks.Delete(ctx, ref)
	case "scrap":
		return c.Scrap.Delete(ctx, ref)
	case "qr":
		return c.QR.Delete(ctx, ref)
	}
	return fmt.Errorf("cannot undo %s", route)
}

// runAppend adds a line to the most recent post.
func (s *Server) runAppend(ctx context.Context, c *capability.Clients, in capability.Invocation) (capability.Result, error) {
	text := in.Arg("text")
	if strings.TrimSpace(text) == "" {
		return capability.Result{}, fmt.Errorf("nothing to append")
	}
	post, err := s.recentPost(in.Actor)
	if err != nil {
		return capability.Result{}, err
	}
	body, tags := capability.ExtractTags(text)
	slug, err := c.Feed.AppendToPost(ctx, post.Ref, body, tags)
	if err != nil {
		return capability.Result{}, err
	}
	return capability.Result{Ref: slug, Text: "appended · " + slug}, nil
}

// runTags replaces the tags on the most recent post.
func (s *Server) runTags(ctx context.Context, c *capability.Clients, in capability.Invocation) (capability.Result, error) {
	post, err := s.recentPost(in.Actor)
	if err != nil {
		return capability.Result{}, err
	}
	tags := capability.Dedupe(capability.SplitCommaList(in.Arg("list")))
	if err := c.Feed.RetagPost(ctx, post.Ref, tags); err != nil {
		return capability.Result{}, err
	}
	if len(tags) == 0 {
		return capability.Result{Ref: post.Ref, Text: "tags cleared · " + post.Ref}, nil
	}
	return capability.Result{Ref: post.Ref,
		Text: "tagged · " + post.Ref + " · " + strings.Join(tags, ", ")}, nil
}

// recentPost finds the post /append and /tags should act on, enforcing the window.
func (s *Server) recentPost(sender string) (*Message, error) {
	prior, err := lastAction(s.db, sender, "feed", "append")
	if err != nil {
		return nil, err
	}
	if prior == nil || prior.Ref == "" {
		return nil, fmt.Errorf("no recent post")
	}
	at, err := time.Parse(time.RFC3339, prior.ReceivedAt)
	if err == nil && time.Since(at) > appendWindow {
		return nil, fmt.Errorf("last post is older than %s — edit it in the feed console",
			appendWindow)
	}
	return prior, nil
}

// displayName is the filename to give an inbound attachment. Photon usually
// supplies one; when it doesn't, the guid keeps it unique and traceable.
func displayName(a attachment) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	return "attachment-" + a.ID
}
