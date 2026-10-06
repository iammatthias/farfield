package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

func adminBackupServer(t *testing.T, apiKey string) (*Server, *httptest.Server) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "backup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, auth: &web.Auth{DB: db, Password: "pw", APIKey: apiKey},
		rd: &web.Renderer{Templates: tmpl}}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func getSnapshots(t *testing.T, ts *httptest.Server, key string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/admin/snapshots", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Backup had no API key before this route; with BACKUP_API_KEY unset the
// admin API must stay shut rather than open.
func TestAdminSnapshotsFailClosed(t *testing.T) {
	_, ts := adminBackupServer(t, "")
	if code, body := getSnapshots(t, ts, "anything", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("no BACKUP_API_KEY = %d %s, want 503", code, body)
	}
}

func TestAdminSnapshots(t *testing.T) {
	s, ts := adminBackupServer(t, "bk")
	for _, b := range []*Backup{
		{App: "content", CID: "bafyone", Size: 10, CreatedAt: "2026-01-01T00:00:00Z"},
		{App: "feed", CID: "bafytwo", Size: 20, CreatedAt: "2026-01-02T00:00:00Z"},
	} {
		if err := insertBackup(s.db, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "bk", map[string]string{"Cf-Ray": "x"}, 404},
		{"tunnel client ip", "bk", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"no key", "", nil, 401},
		{"wrong key", "nope", nil, 401},
		{"key", "bk", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := getSnapshots(t, ts, tc.key, tc.headers); code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", code, tc.want, body)
			}
		})
	}
	_, body := getSnapshots(t, ts, "bk", nil)
	var got struct{ Snapshots []Backup }
	_ = json.Unmarshal(body, &got)
	if len(got.Snapshots) != 2 || got.Snapshots[0].CID != "bafytwo" || got.Snapshots[0].App != "feed" {
		t.Errorf("snapshots = %s, want both, newest first", body)
	}
}
