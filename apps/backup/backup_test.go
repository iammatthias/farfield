package main

import "testing"

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		2048:    "2.0 KB",
		5 << 20: "5.0 MB",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTargets(t *testing.T) {
	names := map[string]bool{}
	for _, tg := range targets() {
		if tg.Name == "" || tg.DBPath == "" {
			t.Errorf("target has empty field: %+v", tg)
		}
		names[tg.Name] = true
	}
	for _, want := range []string{"content", "feed", "blobs"} {
		if !names[want] {
			t.Errorf("targets() missing %q", want)
		}
	}
}
