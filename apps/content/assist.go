package main

// The assist endpoint: one deliberate button in the entry editor that asks a
// small model for tags and an excerpt, given the piece itself.
//
// Manually invoked, never automatic — metadata that writes itself on every
// save would train the author to stop reading it, and the whole value of an
// excerpt is that somebody chose it. The model proposes into the form fields;
// nothing is saved until the author saves. The key stays server-side with the
// rest of the fleet's keys, which is why this is an endpoint rather than a
// browser fetch to OpenRouter.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// assistModel is the OpenRouter model id. It must be on the workspace's
// guardrail allowlist — the flash model this used to name was not, and every
// request came back "Model blocked by guardrail". This is the model the
// switchboard agent already runs on; CONTENT_ASSIST_MODEL overrides it.
func assistModel() string {
	return store.Env("CONTENT_ASSIST_MODEL", "openai/gpt-6-luna-pro")
}

// assistTimeout bounds the upstream call. The author is watching a button,
// and a rewrite of a synopsis is a second call inside the same budget.
const assistTimeout = 45 * time.Second

// assistMaxBody caps how much of the piece is sent. Enough that the model has
// actually read it; bounded so a book-length draft is not a book-length bill.
const assistMaxBody = 24_000

// assistPrompt is the entire instruction. JSON out, and the constraints that
// make the output match the site: tags drawn from the vocabulary already in
// use, and an excerpt in the author's own voice.
//
// The excerpt instruction is the delicate one. "Describe what the piece is"
// produced a reviewer's synopsis — "Frames LLMs as an enclosure movement…",
// "An overview of three plugins the author built" — when an excerpt is the
// author's line: the dek under a title, ideally lifted from the piece itself.
// The examples are excerpts the author wrote by hand.
const assistPrompt = `You write metadata for one article on a personal site.
Reply with ONLY a JSON object, no prose, no code fences:
{"tags": ["..."], "excerpt": "..."}

tags: 3 to 5, lowercase kebab-case, each 1-3 words. Lead with the piece's own
specific subjects — the named technologies, ideas and projects it is about —
then at most two broader ones. When a tag in the site's existing vocabulary
(listed with the article) names the same thing, use that spelling ("llms", not
"llm"); never swap a specific subject for a generic one. No hashtags.

excerpt: the line a reader sees under the title — written in the author's own
voice, as if the author wrote it. The best excerpt is a sentence lifted from the
piece that states its central idea and stands alone out of context — not a
transition ("Along the way…"), an aside, or a list item. Trim it if needed.
One sentence, at most 160 characters, plain text, no markdown, no exclamation
marks.
Never describe the piece from outside. Do not start with a verb about the piece
("Frames", "Explores", "Argues", "Examines", "Describes", "Covers") and never
write "this piece", "this post", "the article", "the author", "an overview of".
Write as the author: "I", "we", or a plain statement of the idea itself.

Excerpts the author wrote, for voice:
- Cold war style number stations for good little bots built on Ethereum
- Putting my Apple Music "Now Playing" on display with a Raspberry Pi and a 64x64 LED matrix.
- Wiring a Farfield homelab up to iMessage with Photon and OpenPoke, so slash commands reach personal apps and talking to your agent feels like texting a friend.`

// synopsisRe catches an excerpt written from outside the piece — the failure
// the prompt warns against, checked because models drift back to it.
var synopsisRe = regexp.MustCompile(`(?i)^(frames|explores|examines|argues|describes|covers|discusses|outlines|reflects|presents|details|introduces|traces|considers|offers)\b|\b(this (piece|post|article|essay|entry)|the author|an overview of|in this (piece|post|article|essay))\b`)

// outsideVoice reports whether an excerpt reads as a synopsis of the piece
// rather than the author's own line.
func outsideVoice(excerpt string) bool { return synopsisRe.MatchString(strings.TrimSpace(excerpt)) }

type assistResult struct {
	Tags    []string `json:"tags"`
	Excerpt string   `json:"excerpt"`
}

// handleAssist proposes tags and an excerpt for the submitted draft.
func (s *Server) handleAssist(w http.ResponseWriter, r *http.Request) {
	key := store.Env("OPENROUTER_API_KEY", "")
	if key == "" {
		web.WriteError(w, http.StatusServiceUnavailable,
			"no OPENROUTER_API_KEY configured — assist is off on this deployment")
		return
	}

	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		web.WriteError(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		web.WriteError(w, http.StatusBadRequest, "nothing to read — write something first")
		return
	}
	body := in.Body
	if len(body) > assistMaxBody {
		body = body[:assistMaxBody]
	}

	ctx, cancel := context.WithTimeout(r.Context(), assistTimeout)
	defer cancel()
	msgs := []chatMessage{{"user", fmt.Sprintf("Existing tags on the site: %s\n\nTitle: %s\n\n%s",
		strings.Join(siteTags(s.db, 80), ", "), strings.TrimSpace(in.Title), body)}}
	raw, err := openrouterChat(ctx, key, assistModel(), assistPrompt, msgs)
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	out, err := parseAssist(raw)
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, "model answered unusably: "+err.Error())
		return
	}
	// A synopsis is the failure the prompt warns against; models drift back
	// to it. Say so, once, and keep the better answer.
	if outsideVoice(out.Excerpt) {
		msgs = append(msgs, chatMessage{"assistant", raw}, chatMessage{"user", fmt.Sprintf(
			"That excerpt describes the piece from outside: %q. Rewrite it as the author's own line — "+
				"ideally a sentence from the piece, trimmed. Same JSON shape.", out.Excerpt)})
		if raw2, err := openrouterChat(ctx, key, assistModel(), assistPrompt, msgs); err == nil {
			if again, err := parseAssist(raw2); err == nil && again.Excerpt != "" && !outsideVoice(again.Excerpt) {
				out.Excerpt = again.Excerpt
			}
		}
	}
	web.WriteJSON(w, http.StatusOK, out)
}

// openrouterURL is a var so tests can point it at a stub.
var openrouterURL = "https://openrouter.ai/api/v1/chat/completions"

var assistClient = &http.Client{Timeout: assistTimeout}

// openrouterChat runs one chat completion and returns the assistant text.
// chatMessage is one turn of the conversation after the system prompt.
type chatMessage struct{ Role, Content string }

func openrouterChat(ctx context.Context, key, model, system string, turns []chatMessage) (string, error) {
	messages := []map[string]string{{"role": "system", "content": system}}
	for _, t := range turns {
		messages = append(messages, map[string]string{"role": t.Role, "content": t.Content})
	}
	payload, err := json.Marshal(map[string]any{"model": model, "messages": messages})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openrouterURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "farfield-content/1.0")

	resp, err := assistClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// OpenRouter's error body says which model was refused — the one thing
		// worth surfacing, since fixing it means allowing the model on the key.
		return "", fmt.Errorf("openrouter: %s: %s", resp.Status, firstErrorLine(raw))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("openrouter answered non-JSON")
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openrouter returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// firstErrorLine digs the human part out of an error body without echoing the
// whole thing into a form.
func firstErrorLine(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// jsonObjectRe finds the first {...} in a reply, for models that wrap their
// JSON in prose or fences despite instructions.
var jsonObjectRe = regexp.MustCompile(`(?s)\{.*\}`)

// tagRe is what a tag may look like once normalised.
var tagRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// parseAssist validates the model's reply into something the form can take.
// The model proposes; this decides what is acceptable to show.
func parseAssist(reply string) (assistResult, error) {
	m := jsonObjectRe.FindString(reply)
	if m == "" {
		return assistResult{}, fmt.Errorf("no JSON object in the reply")
	}
	var out assistResult
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return assistResult{}, err
	}

	seen := map[string]bool{}
	tags := make([]string, 0, len(out.Tags))
	for _, t := range out.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		t = strings.Trim(t, "#")
		t = strings.ReplaceAll(t, " ", "-")
		if t == "" || seen[t] || !tagRe.MatchString(t) {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
		if len(tags) == 6 {
			break
		}
	}
	out.Tags = tags

	out.Excerpt = strings.TrimSpace(out.Excerpt)
	if len(out.Excerpt) > 300 {
		// Cut at a word rather than mid-rune; the instruction said 220, so 300
		// is already the model ignoring it.
		cut := out.Excerpt[:300]
		if i := strings.LastIndexByte(cut, ' '); i > 200 {
			cut = cut[:i]
		}
		out.Excerpt = strings.TrimRight(cut, " ,;") + "…"
	}

	if len(out.Tags) == 0 && out.Excerpt == "" {
		return assistResult{}, fmt.Errorf("nothing usable in the reply")
	}
	return out, nil
}

// siteTags is the tag vocabulary already in use on live entries, most used
// first, so the model reuses the site's own words instead of coining near
// duplicates ("llm" beside "llms" beside "large-language-models").
func siteTags(db *sql.DB, limit int) []string {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`SELECT tags FROM entries WHERE deleted_at IS NULL OR deleted_at = ''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	count := map[string]int{}
	for rows.Next() {
		var s string
		if rows.Scan(&s) != nil {
			continue
		}
		for _, t := range decodeTags(s) {
			count[t]++
		}
	}
	tags := make([]string, 0, len(count))
	for t := range count {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if count[tags[i]] != count[tags[j]] {
			return count[tags[i]] > count[tags[j]]
		}
		return tags[i] < tags[j]
	})
	if len(tags) > limit {
		tags = tags[:limit]
	}
	return tags
}
