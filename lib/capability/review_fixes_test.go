package capability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitEndsTheNameAtAnyWhitespace(t *testing.T) {
	for in, want := range map[string][2]string{
		"/feed\nhello\nworld": {"feed", "hello\nworld"},
		"/feed\thello":        {"feed", "hello"},
		"/FEED hello":         {"feed", "hello"},
		"/help":               {"help", ""},
	} {
		name, rest, ok := Split(in)
		if !ok || name != want[0] || rest != want[1] {
			t.Errorf("Split(%q) = %q, %q, %v; want %q, %q", in, name, rest, ok, want[0], want[1])
		}
	}
}

func TestBindKeepsIndentationOfARestArgument(t *testing.T) {
	spec := &Spec{Name: "scrap", Args: []Arg{{Name: "text", Rest: true}}}
	_, rest, _ := Split("/scrap\n    indented:\n      yaml: true\n")
	in, err := spec.Bind(rest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Arg("text"); got != "    indented:\n      yaml: true" {
		t.Errorf("text = %q, want the first line's indentation kept", got)
	}
	// Same-line text still loses only its separator.
	_, rest, _ = Split("/scrap   hello there")
	in, _ = spec.Bind(rest, nil)
	if got := in.Arg("text"); got != "hello there" {
		t.Errorf("text = %q", got)
	}
}

func TestBindFieldsSplitOnNewlines(t *testing.T) {
	spec := &Spec{Name: "bm", Args: []Arg{{Name: "url"}, {Name: "category", Optional: true, Rest: true}}}
	_, rest, _ := Split("/bm https://example.com\nreading")
	in, err := spec.Bind(rest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Arg("url") != "https://example.com" || in.Arg("category") != "reading" {
		t.Errorf("url=%q category=%q", in.Arg("url"), in.Arg("category"))
	}
}

func TestHashtagsNeedALetter(t *testing.T) {
	body, tags := ExtractTags("we finished #1")
	if body != "we finished #1" || len(tags) != 0 {
		t.Errorf("ExtractTags = %q %v, want #1 left in the text", body, tags)
	}
	_, tags = ExtractTags("launch day #v2 #ship")
	if strings.Join(tags, ",") != "v2,ship" {
		t.Errorf("tags = %v", tags)
	}
}

func TestSplitTagList(t *testing.T) {
	for in, want := range map[string]string{
		"#life #travel":     "life,travel",
		"life, travel":      "life,travel",
		"life travel":       "life,travel",
		"#Life,#TRAVEL, #1": "life,travel",
		"":                  "",
	} {
		if got := strings.Join(SplitTagList(in), ","); got != want {
			t.Errorf("SplitTagList(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelpListsAliases(t *testing.T) {
	r := NewRegistry(&Spec{Name: "Feed", Aliases: []string{"POST"}, Summary: "post"})
	if _, ok := r.Lookup("post"); !ok {
		t.Error("mixed-case alias not found")
	}
	if !strings.Contains(r.Help(""), "(also /post)") {
		t.Errorf("help = %q, want the alias listed", r.Help(""))
	}
}

func TestErrorsAreReadableAndRedirectsAreNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"body is required"}`))
		case "/login-redirect":
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case "/login":
			w.Write([]byte("<html>login</html>"))
		}
	}))
	defer srv.Close()
	s := newSvc(srv.URL, "k")

	_, err := s.do(context.Background(), http.MethodGet, "/json", "", nil)
	if err == nil || strings.Contains(err.Error(), "{") || !strings.Contains(err.Error(), "body is required") {
		t.Errorf("json error = %v, want the message without raw JSON", err)
	}
	_, err = s.do(context.Background(), http.MethodGet, "/login-redirect", "", nil)
	if err == nil || !strings.Contains(err.Error(), "key not accepted") {
		t.Errorf("redirect = %v, want a key error, not the login page", err)
	}
}

func TestPulseSummarySkipsDisabledAndUnchecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"targets":[
			{"name":"a","enabled":true,"last":{"ok":true}},
			{"name":"b","enabled":true,"last":{"ok":false}},
			{"name":"c","enabled":true},
			{"name":"old","enabled":false,"last":{"ok":false}}]}`))
	}))
	defer srv.Close()
	c := New(Config{PulseURL: srv.URL, PulseKey: "k"})
	got, err := c.PulseSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "3 targets · 1 up · 1 down · 1 not yet checked") {
		t.Errorf("summary = %q", got)
	}
}
