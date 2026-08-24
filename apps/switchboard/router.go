package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Route names. They are stored on every message row, so they double as the
// audit vocabulary in the console and as the set /undo knows how to reverse.
const (
	routeFeed     = "feed"
	routeBookmark = "bookmark"
	routeScrap    = "scrap"
	routeQR       = "qr"
	routeStatus   = "status"
	routePulse    = "pulse"
	routeHelp     = "help"
	routeUndo     = "undo"
	routeTags     = "tags"
	routeAppend   = "append"
	routeNone     = "none"
)

// appendWindow bounds `+` and /tags. Past it, "the last post" stops meaning
// what the sender thinks it means — a week-old post is not what someone
// appending a line has in mind — so the command refuses rather than guesses.
const appendWindow = 15 * time.Minute

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

// route decides what an inbound message meant and carries it out.
//
// The default is capture, so the common case — type a thought, send it — needs
// no syntax at all. Everything else is an explicit slash command, with one
// exception: a message that is nothing but a URL is a bookmark, because that is
// what sharing a bare link from Safari means.
func (s *Server) route(ctx context.Context, rec *Message, text string, atts []attachment) routeResult {
	cmd, rest := splitCommand(text)

	switch cmd {
	case "help", "?":
		return routeResult{route: routeHelp, reply: helpText}
	case "feed", "post":
		return s.toFeed(ctx, rest, atts)
	case "bm", "bookmark", "link":
		return s.toBookmark(ctx, rest)
	case "scrap", "paste":
		return s.toScrap(ctx, rest)
	case "qr":
		return s.toQR(ctx, rest)
	case "status":
		return s.toStatus(ctx)
	case "pulse":
		return s.toPulse(ctx)
	case "undo":
		return s.toUndo(ctx, rec.Sender)
	case "tags":
		return s.toTags(ctx, rec.Sender, rest)
	case "+":
		return s.toAppend(ctx, rec.Sender, rest)
	}

	// No command. Attachments always mean a post; a bare URL means a bookmark;
	// anything else is a thought.
	if len(atts) == 0 && isBareURL(text) {
		return s.toBookmark(ctx, text)
	}
	return s.toFeed(ctx, text, atts)
}

// splitCommand separates a leading command word from the rest. `+` is special:
// it needs no space after it, so `+more text` and `+ more text` both parse.
func splitCommand(text string) (cmd, rest string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if after, ok := strings.CutPrefix(text, "+"); ok {
		return "+", strings.TrimSpace(after)
	}
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	word, remainder, _ := strings.Cut(text[1:], " ")
	return strings.ToLower(strings.TrimSpace(word)), strings.TrimSpace(remainder)
}

// isBareURL reports whether the message is one URL and nothing else.
func isBareURL(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, " \t\n") {
		return false
	}
	u, err := url.Parse(text)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// extractTags pulls trailing hashtags off a message and returns the body
// without them.
//
// Hashtags are taken from the end and only from the end, because `#` at the
// start of a markdown line is a heading — treating every `#word` as a tag would
// quietly eat headings out of posts. Trailing is also how people actually
// write them: "walked the long way home #life".
func extractTags(body string) (string, []string) {
	fields := strings.Fields(body)
	var tags []string
	end := len(fields)
	for end > 0 {
		tag, ok := asHashtag(fields[end-1])
		if !ok {
			break
		}
		tags = append([]string{tag}, tags...)
		end--
	}
	if len(tags) == 0 {
		return strings.TrimSpace(body), nil
	}
	// Rebuild from the original text rather than re-joining fields, so internal
	// line breaks and spacing in the body survive.
	trimmed := body
	for i := len(fields) - 1; i >= end; i-- {
		if idx := strings.LastIndex(trimmed, fields[i]); idx >= 0 {
			trimmed = trimmed[:idx]
		}
	}
	return strings.TrimSpace(trimmed), dedupe(tags)
}

// asHashtag validates one trailing token as a tag and returns it without the #.
func asHashtag(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, "#")
	if !ok || rest == "" {
		return "", false
	}
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", false
		}
	}
	return strings.ToLower(rest), true
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ── dispatch ───────────────────────────────────────────────────────────────

func (s *Server) toFeed(ctx context.Context, text string, atts []attachment) routeResult {
	body, tags := extractTags(text)
	if body == "" && len(atts) == 0 {
		return failed(routeFeed, fmt.Errorf("nothing to post"))
	}

	// Photos ride to feed as bytes; feed owns the blob upload because feed owns
	// the post that references it. switchboard never talks to blobs.
	var files []namedFile
	for _, a := range atts {
		data, err := s.photon.Download(ctx, a.ID)
		if err != nil {
			return failed(routeFeed, fmt.Errorf("could not fetch %s: %w", displayName(a), err))
		}
		files = append(files, namedFile{Name: displayName(a), Mime: a.MimeType, Data: data})
	}

	slug, err := s.feed.CreatePost(ctx, body, tags, files)
	if err != nil {
		return failed(routeFeed, err)
	}
	reply := "posted · " + slug
	if n := len(files); n > 0 {
		reply = fmt.Sprintf("posted · %s · %d photo%s", slug, n, plural(n))
	}
	return routeResult{route: routeFeed, ref: slug, reply: reply}
}

func (s *Server) toBookmark(ctx context.Context, text string) routeResult {
	link, category := firstField(text)
	if !isBareURL(link) {
		return failed(routeBookmark, fmt.Errorf("that is not a URL"))
	}
	id, err := s.bookmarks.Create(ctx, link, category)
	if err != nil {
		return failed(routeBookmark, err)
	}
	// Bookmarks fetches OpenGraph metadata after answering, so the title is not
	// available yet — say what happened rather than echo an empty title.
	return routeResult{route: routeBookmark, ref: id, reply: "bookmarked · " + id}
}

func (s *Server) toScrap(ctx context.Context, text string) routeResult {
	if strings.TrimSpace(text) == "" {
		return failed(routeScrap, fmt.Errorf("nothing to paste"))
	}
	pasteURL, id, err := s.scrap.Create(ctx, text)
	if err != nil {
		return failed(routeScrap, err)
	}
	return routeResult{route: routeScrap, ref: id, reply: pasteURL}
}

func (s *Server) toQR(ctx context.Context, text string) routeResult {
	target, label := firstField(text)
	if strings.TrimSpace(target) == "" {
		return failed(routeQR, fmt.Errorf("give me something to encode"))
	}
	id, err := s.qr.Create(ctx, target, label)
	if err != nil {
		return failed(routeQR, err)
	}
	// The picture is the whole point of texting for a QR code, so send it and
	// nothing else — a URL beside it is noise you would have to open. The link
	// is the fallback, not the default: it appears only when the raster could
	// not be fetched, so a failure still leaves something usable in the thread.
	res := routeResult{route: routeQR, ref: id}
	png, err := s.qr.PNG(ctx, id)
	if err != nil {
		res.reply = s.qr.PublicURL + "/qr/" + id + ".png"
		return res
	}
	res.image, res.imageName = png, id+".png"
	return res
}

func (s *Server) toStatus(ctx context.Context) routeResult {
	return routeResult{route: routeStatus, reply: s.fleetStatus(ctx)}
}

func (s *Server) toPulse(ctx context.Context) routeResult {
	summary, err := s.pulseSummary(ctx)
	if err != nil {
		return failed(routePulse, err)
	}
	return routeResult{route: routePulse, reply: summary}
}

// toUndo reverses the sender's last mutating action, whichever service owned it.
func (s *Server) toUndo(ctx context.Context, sender string) routeResult {
	prior, err := lastAction(s.db, sender, routeFeed, routeBookmark, routeScrap, routeQR, routeAppend)
	if err != nil {
		return failed(routeUndo, err)
	}
	if prior == nil || prior.Ref == "" {
		return failed(routeUndo, fmt.Errorf("nothing to undo"))
	}
	if err := s.deleteRef(ctx, prior.Route, prior.Ref); err != nil {
		return failed(routeUndo, err)
	}
	return routeResult{route: routeUndo, ref: prior.Ref,
		reply: fmt.Sprintf("undone · %s %s", prior.Route, prior.Ref)}
}

// deleteRef removes a record from whichever service created it.
func (s *Server) deleteRef(ctx context.Context, route, ref string) error {
	switch route {
	case routeFeed, routeAppend:
		return s.feed.DeletePost(ctx, ref)
	case routeBookmark:
		return s.bookmarks.Delete(ctx, ref)
	case routeScrap:
		return s.scrap.Delete(ctx, ref)
	case routeQR:
		return s.qr.Delete(ctx, ref)
	}
	return fmt.Errorf("cannot undo %s", route)
}

// toAppend adds a line to the most recent post.
func (s *Server) toAppend(ctx context.Context, sender, text string) routeResult {
	if strings.TrimSpace(text) == "" {
		return failed(routeAppend, fmt.Errorf("nothing to append"))
	}
	post, err := s.recentPost(sender)
	if err != nil {
		return failed(routeAppend, err)
	}
	body, tags := extractTags(text)
	slug, err := s.feed.AppendToPost(ctx, post.Ref, body, tags)
	if err != nil {
		return failed(routeAppend, err)
	}
	return routeResult{route: routeAppend, ref: slug, reply: "appended · " + slug}
}

// toTags replaces the tags on the most recent post.
func (s *Server) toTags(ctx context.Context, sender, text string) routeResult {
	post, err := s.recentPost(sender)
	if err != nil {
		return failed(routeTags, err)
	}
	tags := dedupe(splitCommaList(text))
	if err := s.feed.RetagPost(ctx, post.Ref, tags); err != nil {
		return failed(routeTags, err)
	}
	if len(tags) == 0 {
		return routeResult{route: routeTags, ref: post.Ref, reply: "tags cleared · " + post.Ref}
	}
	return routeResult{route: routeTags, ref: post.Ref,
		reply: "tagged · " + post.Ref + " · " + strings.Join(tags, ", ")}
}

// recentPost finds the post `+` and /tags should act on, enforcing the window.
func (s *Server) recentPost(sender string) (*Message, error) {
	prior, err := lastAction(s.db, sender, routeFeed, routeAppend)
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

// ── small helpers ──────────────────────────────────────────────────────────

// firstField splits off the first whitespace-delimited token.
func firstField(s string) (first, rest string) {
	s = strings.TrimSpace(s)
	first, rest, _ = strings.Cut(s, " ")
	return strings.TrimSpace(first), strings.TrimSpace(rest)
}

func splitCommaList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// displayName is the filename to give an inbound attachment. Photon usually
// supplies one; when it doesn't, the guid keeps it unique and traceable.
func displayName(a attachment) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	return "attachment-" + a.ID
}

const helpText = `farfield switchboard

text            → feed post
text + photos   → feed post with the photos
a bare link     → bookmark

/feed <text>    force a post
/bm <url> [cat] force a bookmark
/scrap <text>   paste, returns a link
/qr <url> [lbl] QR code, returns the image
/status         fleet health
/pulse          traffic and incidents
+ <text>        append to the last post
/tags a, b      retag the last post
/undo           undo the last action

trailing #hashtags become tags`
