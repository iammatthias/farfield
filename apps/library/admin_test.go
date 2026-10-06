package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminBooks(t *testing.T) {
	s := newTestServer(t)
	s.uploadKey = "intern"
	for _, b := range []*Book{
		{CID: "bafyold", Title: "Old", Collection: "Sci-Fi", Size: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{CID: "bafymid", Title: "Mid", Collection: "", Size: 1, CreatedAt: "2026-01-02T00:00:00Z"},
		{CID: "bafynew", Title: "New", Collection: "Sci-Fi", Size: 1, CreatedAt: "2026-01-03T00:00:00Z"},
	} {
		if err := upsertBook(s.db, b); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	do := func(key string, headers map[string]string) (int, []byte) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/admin/books", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
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

	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "secret", map[string]string{"Cf-Ray": "x"}, 404},
		{"tunnel client ip", "secret", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"no key", "", nil, 401},
		// The upload key adds books; it must not read the catalog here either.
		{"upload key", "intern", nil, 401},
		{"full key", "secret", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := do(tc.key, tc.headers); code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", code, tc.want, body)
			}
		})
	}

	_, body := do("secret", nil)
	var got struct {
		Books         []Book
		Collections   []CollectionStat
		Uncategorized int
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Books) != 3 || got.Books[0].CID != "bafynew" || got.Books[2].CID != "bafyold" {
		t.Errorf("books = %+v, want all three newest first", got.Books)
	}
	if len(got.Collections) != 1 || got.Collections[0] != (CollectionStat{Name: "Sci-Fi", Count: 2}) || got.Uncategorized != 1 {
		t.Errorf("collections = %+v, uncategorized = %d", got.Collections, got.Uncategorized)
	}
}

func TestAdminBooksEmpty(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/api/admin/books", nil)
	req.Header.Set("X-API-Key", "secret")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if string(raw["books"]) != "[]" || string(raw["collections"]) != "[]" {
		t.Errorf("empty catalog = %s, want empty arrays, not null", b)
	}
}
