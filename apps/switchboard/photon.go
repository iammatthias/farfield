package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// Photon's webhook headers. The signature is a real HMAC over the raw body —
// not, as some providers do, a static secret echoed back in a header.
const (
	hdrSignature = "X-Spectrum-Signature"
	hdrTimestamp = "X-Spectrum-Timestamp"
	hdrWebhookID = "X-Spectrum-Webhook-Id"
)

// maxWebhookBody bounds the delivery. Photon's webhook carries metadata only —
// attachment bytes are fetched separately — so a real one is a few KB.
const maxWebhookBody = 1 << 20

// webhookSkew is how far a delivery's timestamp may drift from our clock before
// we refuse it. It bounds replay of a captured request to this window; Photon's
// own client uses the same five minutes.
const webhookSkew = 5 * time.Minute

// envelope is the Spectrum webhook body. Only "messages" carries a message
// today; every other event is acknowledged and dropped.
type envelope struct {
	Event   string         `json:"event"`
	Message webhookMessage `json:"message"`
}

type webhookMessage struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Direction string `json:"direction"`
	Timestamp string `json:"timestamp"`
	Sender    struct {
		ID       string `json:"id"`
		Platform string `json:"platform"`
	} `json:"sender"`
	Space struct {
		ID       string `json:"id"`
		Platform string `json:"platform"`
		Type     string `json:"type"`
	} `json:"space"`
	Content content `json:"content"`
}

// content is one message's payload.
//
// Two shapes are in play and both are accepted, because the documented one is
// not the one that arrives. Spectrum's SDK models content as a union
// discriminated on Type, where a caption plus photos is a "group" holding
// Items. The line's own model — and the normalized-events webhook — is flat
// instead: a Text alongside an Attachments array. Parsing only the union cost
// a real message its photo *and* its caption, so this reads whichever is
// present rather than betting on one.
//
// Attachment identity is equally unsettled: the docs say `id` with `name` and
// `size`, the wire says `guid` with `fileName` and `totalBytes` (a string,
// since proto renders int64 that way). All of them are read.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`

	ID             string `json:"id"`
	GUID           string `json:"guid"`
	AttachmentGUID string `json:"attachmentGuid"`

	Name       string      `json:"name"`
	FileName   string      `json:"fileName"`
	MimeType   string      `json:"mimeType"`
	Size       int64       `json:"size"`
	TotalBytes json.Number `json:"totalBytes"`

	Items       []content `json:"items"`
	Attachments []content `json:"attachments"`

	// Inner is the third shape this union has been observed wearing: a
	// "group" whose items are full sub-MESSAGES, each wrapping its real
	// payload one level down in a "content" key (alongside its own sender,
	// space, and timestamp). The first such delivery flattened to nothing
	// and a texted "/feed" with a photo was silently ignored — the log's
	// raw-content diagnostic is how it was caught.
	Inner *content `json:"content"`
}

// attachmentID returns the attachment's identifier under whichever key it came
// in on, or "" when this node is not an attachment.
func (c content) attachmentID() string {
	for _, id := range []string{c.ID, c.GUID, c.AttachmentGUID} {
		if id != "" {
			return id
		}
	}
	return ""
}

// bytes reports the declared size, from either spelling.
func (c content) bytes() int64 {
	if c.Size > 0 {
		return c.Size
	}
	if n, err := c.TotalBytes.Int64(); err == nil {
		return n
	}
	return 0
}

// filename is the attachment's name under whichever key it came in on.
func (c content) filename() string {
	if c.FileName != "" {
		return c.FileName
	}
	return c.Name
}

// inert content arms carry no instruction: a tapback, a read receipt, someone
// joining a thread. They are skipped entirely so their incidental text (a
// reaction's emoji, say) never becomes a post.
var inertContent = map[string]bool{
	"reaction": true, "read": true, "typing": true, "avatar": true,
	"rename": true, "addMember": true, "removeMember": true, "leaveSpace": true,
}

// objectReplacement is U+FFFC, which Apple embeds in a message's text at each
// attachment's position. Left in, every photo caption would start with a stray
// glyph that renders as a hollow box in the post.
const objectReplacement = "￼"

// attachment is one inbound file, metadata only — the bytes live on the line
// until we pull them.
type attachment struct {
	ID       string
	Name     string
	MimeType string
	Size     int64
}

// flatten reduces a content tree to the text and attachments switchboard acts
// on. Unknown and non-actionable arms (reactions, read receipts, membership
// changes) contribute nothing, so a message made only of those flattens to
// empty and is ignored.
func flatten(c content) (text string, atts []attachment) {
	var texts []string
	seen := map[string]bool{}

	// inList marks nodes reached through an Attachments array: those are
	// attachments whether or not they carry a Type saying so.
	var walk func(node content, inList bool)
	walk = func(node content, inList bool) {
		if inertContent[node.Type] {
			return
		}
		// A sub-message wrapper carries its payload one level down; the
		// wrapper itself has no text or type of its own.
		if node.Inner != nil {
			walk(*node.Inner, inList)
		}
		if s := cleanText(node.Text); s != "" {
			texts = append(texts, s)
		}
		if id := node.attachmentID(); id != "" && !seen[id] &&
			(inList || node.Type == "attachment" || node.filename() != "" || node.MimeType != "") {
			seen[id] = true
			atts = append(atts, attachment{
				ID: id, Name: node.filename(), MimeType: node.MimeType, Size: node.bytes(),
			})
		}
		for _, item := range node.Items {
			walk(item, false)
		}
		for _, a := range node.Attachments {
			walk(a, true)
		}
	}
	walk(c, false)
	return strings.TrimSpace(strings.Join(texts, "\n")), atts
}

// cleanText normalizes a message's text: attachment placeholders removed, ends
// trimmed. A caption that was nothing but a placeholder becomes empty, which is
// what makes a bare photo post as a photo rather than as a box glyph.
func cleanText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, objectReplacement, " "))
}

// verifySignature checks Photon's HMAC over the exact bytes received.
//
// The signed string is "v0:{timestamp}:{rawBody}", so the body must be the
// unparsed original — re-marshalling parsed JSON would reorder keys and never
// match. Comparison is constant-time.
func verifySignature(secret, timestamp string, body []byte, header string) bool {
	if secret == "" || timestamp == "" || header == "" {
		return false
	}
	want, ok := strings.CutPrefix(header, "v0=")
	if !ok {
		return false
	}
	sum, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(":"))
	mac.Write(body)
	return subtle.ConstantTimeCompare(mac.Sum(nil), sum) == 1
}

// freshTimestamp reports whether a delivery's timestamp is within the accepted
// skew. Signatures never expire on their own, so without this a request
// captured once could be replayed forever.
func freshTimestamp(timestamp string, now time.Time) bool {
	secs, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	drift := now.Sub(time.Unix(secs, 0))
	if drift < 0 {
		drift = -drift
	}
	return drift <= webhookSkew
}

// handleWebhook receives one inbound message from Photon.
//
// Response discipline follows Photon's retry rule — it retries 5xx only — so
// anything a retry cannot fix answers 2xx: a message from a stranger, a
// reaction, a duplicate delivery. A bad signature answers 401 because it is not
// a delivery we want to acknowledge as understood. Genuine downstream failures
// are the one case worth a 5xx, and they get one, so Photon retries them.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Fail closed. With no secret configured there is no way to tell Photon
	// from anyone else who found the URL, and this endpoint publishes to a
	// public feed — so the route behaves as if it does not exist.
	if s.webhookSecret == "" {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, "body too large")
		return
	}

	ts := r.Header.Get(hdrTimestamp)
	if !verifySignature(s.webhookSecret, ts, body, r.Header.Get(hdrSignature)) {
		slog.Warn("webhook signature rejected", "ip", web.ClientIP(r))
		web.WriteError(w, http.StatusUnauthorized, "bad signature")
		return
	}
	if !freshTimestamp(ts, time.Now()) {
		slog.Warn("webhook timestamp outside window", "ts", ts)
		web.WriteError(w, http.StatusUnauthorized, "stale timestamp")
		return
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		// Signed but unparseable: acknowledge, since a retry sends the same
		// bytes and would fail identically.
		slog.Warn("webhook body not JSON", "err", err)
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "unparseable"})
		return
	}
	if env.Event != "messages" || env.Message.ID == "" {
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "event"})
		return
	}

	s.dispatchWebhook(w, r, &env, r.Header.Get(hdrWebhookID), body)
}

// dispatchWebhook applies the sender/shape filters, dedupes, and routes.
func (s *Server) dispatchWebhook(w http.ResponseWriter, r *http.Request, env *envelope, webhookID string, body []byte) {
	msg := &env.Message
	sender := normalizeHandle(msg.Sender.ID)

	// Idempotency. Photon retries on 5xx and a network hiccup can duplicate a
	// delivery; without this a retried "post this" posts twice. A completed
	// message replays its original reply and dispatches nothing.
	if prior, err := getMessage(s.db, msg.ID); err != nil {
		s.fail(w, "read message log", err)
		return
	} else if prior != nil {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"duplicate": true, "status": prior.Status, "ref": prior.Ref,
		})
		return
	}

	// The allowlist is the load-bearing check: it is what makes this a personal
	// endpoint rather than a public one. A non-matching sender is logged and
	// dropped without a reply — answering would confirm the number is live.
	if !s.allowed(sender) {
		slog.Warn("inbound from disallowed sender", "sender", sender)
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "sender"})
		return
	}
	if msg.Direction != "" && msg.Direction != "inbound" {
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "direction"})
		return
	}
	if p := msg.Platform; p != "" && !strings.EqualFold(p, "imessage") {
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "platform"})
		return
	}
	// Group threads are refused even from an allowed sender: adding the line to
	// a group would otherwise let anything said there reach the feed.
	if t := strings.ToLower(msg.Space.Type); t == "group" {
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "group"})
		return
	}
	if !s.rl.Allow(sender) {
		slog.Warn("inbound rate limited", "sender", sender)
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "rate"})
		return
	}

	text, atts := flatten(msg.Content)
	rec := &Message{
		ID: msg.ID, WebhookID: webhookID, Sender: sender,
		ChatGUID: msg.Space.ID, Body: text,
	}
	if text == "" && len(atts) == 0 {
		rec.Route, rec.Status = routeNone, statusIgnored
		if err := recordMessage(s.db, rec); err != nil {
			slog.Error("record message", "err", err)
		}
		// A message that flattens to nothing is either genuinely inert (a
		// tapback) or a content shape the parser does not know — and from the
		// outside those look identical. Logging the raw content makes the
		// difference visible immediately instead of requiring the sender to
		// reproduce it while someone watches.
		slog.Warn("message flattened to nothing",
			"type", msg.Content.Type, "content", truncate(rawContent(body), 400))
		web.WriteJSON(w, http.StatusOK, map[string]any{"ignored": "empty"})
		return
	}

	result := s.route(r.Context(), rec, text, atts)
	rec.Route, rec.Ref, rec.Reply = result.route, result.ref, result.reply
	rec.Status = statusOK
	if result.err != nil {
		rec.Status = statusError
		slog.Error("dispatch failed", "route", result.route, "err", result.err)
	}
	if err := recordMessage(s.db, rec); err != nil {
		slog.Error("record message", "err", err)
	}

	// The reply is best effort in both directions: a send failure must not undo
	// a post that already exists, and must not provoke a retry that would post
	// it again. It is logged and the delivery is acknowledged.
	s.reply(r.Context(), rec.ChatGUID, result)

	web.WriteJSON(w, http.StatusOK, map[string]any{
		"route": result.route, "ref": result.ref, "status": rec.Status,
	})
}

// reply sends the outcome back into the thread — text, plus an image when the
// action produced one (a QR code).
func (s *Server) reply(ctx context.Context, chatGUID string, res routeResult) {
	if s.photon == nil || chatGUID == "" {
		return
	}
	if res.reply != "" {
		if err := s.photon.SendText(ctx, chatGUID, res.reply); err != nil {
			slog.Warn("reply failed", "err", err)
		}
	}
	if len(res.image) > 0 {
		if err := s.photon.SendImage(ctx, chatGUID, res.imageName, res.image); err != nil {
			slog.Warn("reply image failed", "err", err)
		}
	}
}

// normalizeHandle reduces an iMessage handle to a comparable form: phone
// numbers keep only their digits and a leading +, email handles lowercase.
// Photon sends E.164, but a handle that arrives punctuated ("+1 (555) 123-4567")
// must still match an allowlist entry written plainly.
func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.Contains(h, "@") {
		return strings.ToLower(h)
	}
	var b strings.Builder
	for i, r := range h {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// allowed reports whether a normalized handle may act. An empty allowlist
// permits nobody — this fails closed like the webhook secret, because the cost
// of a mistake is a public post from a stranger.
func (s *Server) allowed(handle string) bool {
	if handle == "" || len(s.allow) == 0 {
		return false
	}
	return s.allow[handle]
}

// rawContent extracts the message.content subtree from a delivery, for
// diagnostics. It returns "" when the body cannot be walked — the caller is
// already on an error path and must not fail again.
func rawContent(body []byte) string {
	var probe struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return string(probe.Message.Content)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
