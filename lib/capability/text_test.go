package capability

import (
	"reflect"
	"testing"
)

// TestExtractTagsTakesOnlyTrailing pins the rule that keeps markdown headings
// out of the tag list: `#` at the start of a line is a heading, and scavenging
// every `#word` would quietly eat them out of posts.
func TestExtractTagsTakesOnlyTrailing(t *testing.T) {
	cases := []struct {
		in       string
		wantBody string
		wantTags []string
	}{
		{"walked the long way home #life", "walked the long way home", []string{"life"}},
		{"a thought #life #slow", "a thought", []string{"life", "slow"}},
		{"no tags here", "no tags here", nil},
		{"# A heading\n\nbody text", "# A heading\n\nbody text", nil},
		{"# A heading\n\nbody #tagged", "# A heading\n\nbody", []string{"tagged"}},
		{"mid #sentence tags are not tags", "mid #sentence tags are not tags", nil},
		{"dupes #a #a #b", "dupes", []string{"a", "b"}},
		{"case folds #Life", "case folds", []string{"life"}},
		{"punctuation is not a tag #no!", "punctuation is not a tag #no!", nil},
	}
	for _, tc := range cases {
		body, tags := ExtractTags(tc.in)
		if body != tc.wantBody {
			t.Errorf("ExtractTags(%q) body = %q, want %q", tc.in, body, tc.wantBody)
		}
		if !reflect.DeepEqual(tags, tc.wantTags) {
			t.Errorf("ExtractTags(%q) tags = %v, want %v", tc.in, tags, tc.wantTags)
		}
	}
}

// TestIsURL: /bm still has to reject something that is plainly not a link, even
// though a bare URL no longer routes anywhere on its own.
func TestIsURL(t *testing.T) {
	yes := []string{"https://example.com", "http://example.com/a/b?c=d"}
	no := []string{"", "example.com", "not a url", "https://example.com and more", "ftp://example.com"}
	for _, s := range yes {
		if !IsURL(s) {
			t.Errorf("IsURL(%q) = false", s)
		}
	}
	for _, s := range no {
		if IsURL(s) {
			t.Errorf("IsURL(%q) = true", s)
		}
	}
}
