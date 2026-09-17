package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int64
		want string
	}{
		{"bytes", 412, "412 B"},
		{"exactly a kilobyte", 1024, "1.0 KB"},
		{"kilobytes", 8601, "8.4 KB"},
		{"exactly a megabyte", 1 << 20, "1.0 MB"},
		// The case the drifted copies got wrong: three apps stopped at MB, so
		// a multi-gigabyte object rendered as a four-digit megabyte count.
		{"gigabytes", 2 << 30, "2.0 GB"},
		{"zero", 0, "0 B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HumanSize(tc.n); got != tc.want {
				t.Errorf("HumanSize(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func TestRelAge(t *testing.T) {
	stamp := func(d time.Duration) string {
		return time.Now().Add(-d).Format(time.RFC3339)
	}
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"seconds", stamp(10 * time.Second), "just now"},
		{"minutes", stamp(9 * time.Minute), "9m ago"},
		{"hours", stamp(5 * time.Hour), "5h ago"},
		{"days", stamp(3 * 24 * time.Hour), "3d ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelAge(tc.in); got != tc.want {
				t.Errorf("RelAge(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Past two months an age stops informing and the date starts.
	old := time.Now().Add(-100 * 24 * time.Hour)
	if got, want := RelAge(old.Format(time.RFC3339)), old.Format("2006-01-02"); got != want {
		t.Errorf("RelAge(100 days) = %q, want the date %q", got, want)
	}

	// A stamp that will not parse comes back unchanged, so a bad row shows
	// something rather than nothing.
	if got := RelAge("not a time"); got != "not a time" {
		t.Errorf("RelAge(garbage) = %q, want it returned unchanged", got)
	}
}

func TestWantsJSON(t *testing.T) {
	for _, tc := range []struct {
		accept string
		want   bool
	}{
		{"application/json", true},
		{"text/html,application/xhtml+xml", false},
		{"", false},
		{"application/json, text/plain, */*", true},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		if tc.accept != "" {
			r.Header.Set("Accept", tc.accept)
		}
		if got := WantsJSON(r); got != tc.want {
			t.Errorf("WantsJSON(Accept: %q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("FirstNonEmpty = %q, want %q", got, "third")
	}
	if got := FirstNonEmpty("first", "second"); got != "first" {
		t.Errorf("FirstNonEmpty = %q, want %q", got, "first")
	}
	if got := FirstNonEmpty("", ""); got != "" {
		t.Errorf("FirstNonEmpty(all empty) = %q, want empty", got)
	}
}
