package main

// The ⌘K menu's Ask row. When typing a name is not enough — "the post where I
// wrote about diffusion", "make a QR code for the docs" — the menu sends the
// sentence and the list it already holds, and a small model picks one item or
// says, in a line, why none fits. It never acts: the pick lands as the top row
// and the author presses Enter. Content hosts it because content is where the
// fleet's OpenRouter key lives.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// askTimeout is shorter than assist's: a menu is open and waiting.
const askTimeout = 20 * time.Second

// askMaxItems bounds the list the model reads.
const askMaxItems = 400

const askPrompt = `You are the command menu of farfield, a fleet of personal apps:
content (long-form entries, series), feed (short posts), blobs (files),
library (ebooks), bookmarks, daily (journal pages), qr (QR codes), scrap
(pastes), sideload (app builds), keys (API tokens), pulse (uptime and traffic),
backup, switchboard (iMessage agent), apex (the docs and status site).

The user typed a request. Choose the ONE item from the list that best does or
opens what they asked for. Items are actions (do), pages (go) or records (open).
Prefer an action when they asked to do something, a record when they named a
thing. Reply with ONLY a JSON object, no prose, no code fences:
{"pick": <the item's i>}
or, when nothing in the list fits:
{"answer": "<one short sentence saying what is missing, or where to look>"}`

type askItem struct {
	I int    `json:"i"`
	T string `json:"t"` // title
	A string `json:"a"` // app
	K string `json:"k"` // kind
	S string `json:"s"` // sub
}

func (s *Server) handlePaletteAsk(w http.ResponseWriter, r *http.Request) {
	key := store.Env("OPENROUTER_API_KEY", "")
	if key == "" {
		web.WriteError(w, http.StatusServiceUnavailable, "Ask is off — no OPENROUTER_API_KEY on this deployment.")
		return
	}
	var in struct {
		Q     string    `json:"q"`
		Here  string    `json:"here"`
		Items []askItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		web.WriteError(w, http.StatusBadRequest, "bad JSON")
		return
	}
	in.Q = strings.TrimSpace(in.Q)
	if in.Q == "" || len(in.Items) == 0 {
		web.WriteError(w, http.StatusBadRequest, "nothing to ask")
		return
	}
	if len(in.Q) > 500 {
		in.Q = in.Q[:500]
	}
	if len(in.Items) > askMaxItems {
		in.Items = in.Items[:askMaxItems]
	}

	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	raw, err := openrouterChat(ctx, key, assistModel(), askPrompt,
		[]chatMessage{{"user", askMessage(in.Q, in.Here, in.Items)}})
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, "Ask failed: "+err.Error())
		return
	}
	pick, answer := parseAsk(raw, len(in.Items))
	if pick >= 0 {
		web.WriteJSON(w, http.StatusOK, map[string]any{"pick": pick})
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"answer": answer})
}

// askMessage lays the list out one item per line — compact, and the index is
// the only thing the model has to echo.
func askMessage(q, here string, items []askItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Request: %s\n", q)
	if here != "" {
		fmt.Fprintf(&b, "They are in: %s\n", here)
	}
	b.WriteString("\nItems (i | app | kind | title | context):\n")
	for _, it := range items {
		fmt.Fprintf(&b, "%d | %s | %s | %s | %s\n", it.I, oneLine(it.A), oneLine(it.K), oneLine(it.T), oneLine(it.S))
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// parseAsk reads the model's reply. A pick outside the list is no pick; an
// unusable reply becomes a plain "nothing fits" rather than an error, since
// the menu still holds the typed results.
func parseAsk(reply string, n int) (pick int, answer string) {
	m := jsonObjectRe.FindString(reply)
	var out struct {
		Pick   *int   `json:"pick"`
		Answer string `json:"answer"`
	}
	if m != "" && json.Unmarshal([]byte(m), &out) == nil {
		if out.Pick != nil && *out.Pick >= 0 && *out.Pick < n {
			return *out.Pick, ""
		}
		if a := strings.TrimSpace(out.Answer); a != "" {
			if len(a) > 300 {
				a = a[:300] + "…"
			}
			return -1, a
		}
	}
	return -1, "Nothing in the fleet fits that."
}
