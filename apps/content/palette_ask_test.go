package main

import "testing"

func TestParseAsk(t *testing.T) {
	for reply, want := range map[string]int{
		`{"pick": 3}`:                          3,
		"```json\n{\"pick\": 0}\n```":          0,
		`Sure! {"pick": 9}`:                    -1, // outside the list
		`{"pick": -2}`:                         -1,
		`{"answer": "No QR codes exist yet."}`: -1,
		`not json at all`:                      -1,
	} {
		pick, answer := parseAsk(reply, 5)
		if pick != want {
			t.Errorf("parseAsk(%q) pick = %d, want %d", reply, pick, want)
		}
		if pick < 0 && answer == "" {
			t.Errorf("parseAsk(%q) gave neither a pick nor an answer", reply)
		}
	}
	if _, a := parseAsk(`{"answer": "No QR codes exist yet."}`, 5); a != "No QR codes exist yet." {
		t.Errorf("answer = %q", a)
	}
}
