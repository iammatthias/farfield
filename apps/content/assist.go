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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// assistModel is the OpenRouter model id. Flash-class on purpose: this is a
// summarising errand, not an essay, and it runs while somebody waits with a
// form open.
func assistModel() string {
	return store.Env("CONTENT_ASSIST_MODEL", "z-ai/glm-5.3-flash")
}

// assistTimeout bounds the upstream call. The author is watching a button.
const assistTimeout = 25 * time.Second

// assistMaxBody caps how much of the piece is sent. Enough that the model has
// actually read it; bounded so a book-length draft is not a book-length bill.
const assistMaxBody = 24_000

// assistPrompt is the entire instruction. JSON out, and the constraints that
// make the output match the site: lowercase kebab tags like the ones already
// in use, and an excerpt in the register of a description rather than a pitch.
const assistPrompt = `You write metadata for one article on a personal site.
Reply with ONLY a JSON object, no prose, no code fences:
{"tags": ["..."], "excerpt": "..."}

tags: 3 to 6, each 1-3 words, lowercase, hyphenated (kebab-case), concrete
topics found in the piece. No hashtags, no invented themes.

excerpt: one or two plain sentences, at most 220 characters, describing what
the piece is — factual and specific, in the piece's own register. Not a teaser,
no "in this post", no exclamation marks, no markdown.`

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
	raw, err := openrouterChat(ctx, key, assistModel(),
		fmt.Sprintf("Title: %s\n\n%s", strings.TrimSpace(in.Title), body))
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	out, err := parseAssist(raw)
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, "model answered unusably: "+err.Error())
		return
	}
	web.WriteJSON(w, http.StatusOK, out)
}

// openrouterURL is a var so tests can point it at a stub.
var openrouterURL = "https://openrouter.ai/api/v1/chat/completions"

var assistClient = &http.Client{Timeout: assistTimeout}

// openrouterChat runs one chat completion and returns the assistant text.
func openrouterChat(ctx context.Context, key, model, user string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": assistPrompt},
			{"role": "user", "content": user},
		},
	})
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
