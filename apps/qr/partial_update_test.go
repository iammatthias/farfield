package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPartialUpdateKeepsUnsentFields pins the guarantee the handler's comment
// always claimed but only delivered for three of seven fields. A PUT carrying
// just a target used to unpublish the code, disable it, and erase its label
// and admin notes, because those decode to Go zero values with no way to tell
// "absent" from "false"/"".
func TestPartialUpdateKeepsUnsentFields(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	orig := &Code{
		ID: "keepme", Label: "Poster in the hallway", Mode: ModeProxy,
		Target: "https://example.com/a", EC: "M",
		Public: true, Enabled: true, AdminNotes: "printed 40 of these",
	}
	if err := insertCode(s.db, orig); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/codes/keepme",
		bytes.NewReader([]byte(`{"target":"https://example.com/b"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", resp.StatusCode)
	}

	got, err := getCode(s.db, "keepme")
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Target != "https://example.com/b" {
		t.Errorf("target = %q, want the updated one", got.Target)
	}
	if got.Label != orig.Label {
		t.Errorf("label = %q, want %q — a partial update erased it", got.Label, orig.Label)
	}
	if !got.Public {
		t.Error("public = false — a partial update unpublished the code")
	}
	if !got.Enabled {
		t.Error("enabled = false — a partial update disabled the code")
	}
	if got.AdminNotes != orig.AdminNotes {
		t.Errorf("adminNotes = %q, want %q — a partial update erased them", got.AdminNotes, orig.AdminNotes)
	}
	if got.Mode != orig.Mode || got.EC != orig.EC {
		t.Errorf("mode/ec drifted: %v/%v", got.Mode, got.EC)
	}
}

// TestExplicitFalseStillApplies — seeding from the existing record must not
// make a deliberate false unsettable.
func TestExplicitFalseStillApplies(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	if err := insertCode(s.db, &Code{ID: "flip", Label: "x", Mode: ModeProxy,
		Target: "https://example.com/a", EC: "M", Public: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/codes/flip",
		bytes.NewReader([]byte(`{"public":false}`)))
	req.Header.Set("Authorization", "Bearer k1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", resp.StatusCode)
	}

	got, _ := getCode(s.db, "flip")
	if got == nil || got.Public {
		t.Error("an explicit public:false was not applied")
	}
	if got != nil && !got.Enabled {
		t.Error("enabled was collaterally cleared")
	}
}
