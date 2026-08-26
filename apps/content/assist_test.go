package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseAssistNormalises: the model proposes, parseAssist decides. Fences,
// prose wrappers, hashtags, spaces, case, duplicates and over-long excerpts
// all arrive in practice, and none of them may reach the form as-is.
func TestParseAssistNormalises(t *testing.T) {
	reply := "Sure! Here you go:\n```json\n" +
		`{"tags": ["Go", "#SQLite", "home lab", "go", "", "weird!tag", "one", "two", "three"],
		  "excerpt": "  A tour of the fleet.  "}` + "\n```"
	out, err := parseAssist(reply)
	if err != nil {
		t.Fatalf("parseAssist: %v", err)
	}
	want := []string{"go", "sqlite", "home-lab", "one", "two", "three"}
	if strings.Join(out.Tags, ",") != strings.Join(want, ",") {
		t.Errorf("tags = %v, want %v", out.Tags, want)
	}
	if out.Excerpt != "A tour of the fleet." {
		t.Errorf("excerpt = %q", out.Excerpt)
	}
}

func TestParseAssistRejectsGarbage(t *testing.T) {
	for _, reply := range []string{
		"I could not do that.",
		`{"tags": [], "excerpt": ""}`,
		`{"tags": ["!!!", "###"], "excerpt": ""}`,
	} {
		if _, err := parseAssist(reply); err == nil {
			t.Errorf("parseAssist(%q) accepted garbage", reply)
		}
	}
}

func TestParseAssistTruncatesRunawayExcerpt(t *testing.T) {
	long := strings.Repeat("word ", 120)
	out, err := parseAssist(`{"tags":["ok"],"excerpt":"` + strings.TrimSpace(long) + `"}`)
	if err != nil {
		t.Fatalf("parseAssist: %v", err)
	}
	if len(out.Excerpt) > 310 || !strings.HasSuffix(out.Excerpt, "…") {
		t.Errorf("excerpt len %d = %q...", len(out.Excerpt), out.Excerpt[:40])
	}
}

// TestHandleAssistRoundTrip drives the handler against a stubbed OpenRouter,
// checking what leaves (auth, model, both text fields) and what comes back.
func TestHandleAssistRoundTrip(t *testing.T) {
	var gotAuth, gotModel, gotUser string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		for _, m := range req.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"tags\":[\"fleet\",\"go\"],\"excerpt\":\"About the fleet.\"}"}}]}`)
	}))
	defer stub.Close()
	prev := openrouterURL
	openrouterURL = stub.URL
	defer func() { openrouterURL = prev }()
	t.Setenv("OPENROUTER_API_KEY", "sk-test")
	t.Setenv("CONTENT_ASSIST_MODEL", "google/gemini-2.5-flash")

	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/assist",
		strings.NewReader(`{"title":"The Fleet","body":"words about the fleet"}`))
	s.handleAssist(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out assistResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response: %v", err)
	}
	if strings.Join(out.Tags, ",") != "fleet,go" || out.Excerpt != "About the fleet." {
		t.Errorf("out = %+v", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotModel != "google/gemini-2.5-flash" {
		t.Errorf("model = %q", gotModel)
	}
	if !strings.Contains(gotUser, "The Fleet") || !strings.Contains(gotUser, "words about the fleet") {
		t.Errorf("user message = %q, want title and body", gotUser)
	}
}

// TestHandleAssistWithoutKey: assist off is a clear 503, not a mystery 502.
func TestHandleAssistWithoutKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleAssist(w, httptest.NewRequest("POST", "/assist", strings.NewReader(`{"body":"x"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestHandleAssistSurfacesModelRefusal: a key that is not allowed to use the
// model is THE likely misconfiguration, and its message must reach the form.
func TestHandleAssistSurfacesModelRefusal(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"Model google/gemini-2.5-flash is not enabled for this key"}}`)
	}))
	defer stub.Close()
	prev := openrouterURL
	openrouterURL = stub.URL
	defer func() { openrouterURL = prev }()
	t.Setenv("OPENROUTER_API_KEY", "sk-test")

	s := &Server{}
	w := httptest.NewRecorder()
	s.handleAssist(w, httptest.NewRequest("POST", "/assist", strings.NewReader(`{"body":"x"}`)))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not enabled for this key") {
		t.Errorf("body = %q, want the refusal surfaced", w.Body.String())
	}
}

// TestHandleAssistEmptyBody: nothing to read is the author's problem to fix,
// said plainly, with no credit spent.
func TestHandleAssistEmptyBody(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-test")
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleAssist(w, httptest.NewRequest("POST", "/assist", strings.NewReader(`{"title":"x","body":"  "}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
