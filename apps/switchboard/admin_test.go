package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdminInspection(t *testing.T) {
	s, srv, _ := newTestServer(t)
	s.auth.APIKey = "sw-key"
	s.auth.ReadKey = "sw-read"

	for i, m := range []*Message{
		{ID: "m1", WebhookID: "wh-secret-1", Sender: "+15551234567", ChatGUID: "iMessage;-;+15551234567",
			Body: "hello", Route: "post", Ref: "abc", Reply: "posted", Status: statusOK, ReceivedAt: "2026-01-01T00:00:00Z"},
		{ID: "m2", WebhookID: "wh-secret-2", Sender: "+15551234567", ChatGUID: "iMessage;-;+15551234567",
			Body: "what's up", Route: "agent", Status: statusOK, ReceivedAt: "2026-01-02T00:00:00Z"},
	} {
		if err := recordMessage(s.db, m); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if err := insertJob(s.db, &Job{ID: "j1", MessageID: "m2", ChatGUID: "iMessage;-;+15551234567",
		Sender: "+15551234567", Prompt: "what's up", Status: jobDone, Result: "not much",
		StartedAt: "2026-01-02T00:00:01Z", FinishedAt: "2026-01-02T00:00:09Z"}); err != nil {
		t.Fatal(err)
	}

	do := func(path, key string, headers map[string]string) (int, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	for _, path := range []string{"/api/admin/messages", "/api/admin/jobs"} {
		for _, tc := range []struct {
			name    string
			key     string
			headers map[string]string
			want    int
		}{
			{"tunnel", "sw-key", map[string]string{"Cf-Ray": "x"}, 404},
			{"tunnel client ip", "sw-key", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
			{"read key", "sw-read", nil, 401},
			{"write key", "sw-key", nil, 200},
		} {
			if code, body := do(path, tc.key, tc.headers); code != tc.want {
				t.Errorf("%s %s = %d, want %d (%s)", path, tc.name, code, tc.want, body)
			}
		}
	}

	_, body := do("/api/admin/messages?limit=1", "sw-key", nil)
	var msgs struct{ Messages []messageView }
	_ = json.Unmarshal(body, &msgs)
	if len(msgs.Messages) != 1 || msgs.Messages[0].ID != "m2" || msgs.Messages[0].Direction != "inbound" {
		t.Errorf("messages?limit=1 = %s, want the newest one", body)
	}
	_, body = do("/api/admin/messages", "sw-key", nil)
	if !strings.Contains(string(body), `"reply":"posted"`) {
		t.Errorf("messages = %s, want the reply text", body)
	}
	_, jobs := do("/api/admin/jobs", "sw-key", nil)
	if !strings.Contains(string(jobs), `"result":"not much"`) {
		t.Errorf("jobs = %s", jobs)
	}
	// Thread and delivery identifiers never leave: the console shows neither.
	for _, b := range [][]byte{body, jobs} {
		if strings.Contains(string(b), "wh-secret") || strings.Contains(string(b), "iMessage;") ||
			strings.Contains(string(b), testSecret) {
			t.Errorf("admin response leaked an identifier or secret: %s", b)
		}
	}
}
