package store

import (
	"testing"

	"github.com/iammatthias/farfield/lib/core"
)

func mem(t *testing.T) *Store {
	t.Helper()
	s, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutThenGetRoundTrips(t *testing.T) {
	s := mem(t)
	w, err := s.Put("posts", "hello", core.Record{"body": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != "created" {
		t.Fatalf("expected created, got %s", w.Status)
	}
	got, err := s.Get("posts", "hello")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Value["body"] != "hi" || got.Seq != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestIdenticalContentIsANoopAndMovesNoCursor(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "x"})
	w, err := s.Put("posts", "a", core.Record{"body": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != "unchanged" {
		t.Fatalf("expected unchanged, got %s", w.Status)
	}
	if seq, _ := s.CurrentSeq(); seq != 1 {
		t.Fatalf("cursor moved on a no-op: %d", seq)
	}
}

func TestUpdateBumpsTheSeq(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "one"})
	w, _ := s.Put("posts", "a", core.Record{"body": "two"})
	if w.Status != "updated" || w.Seq != 2 {
		t.Fatalf("expected updated@2, got %s@%d", w.Status, w.Seq)
	}
}

func TestDeleteRemovesRecordAndWritesTombstone(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "x"})
	existed, _ := s.Delete("posts", "a")
	if !existed {
		t.Fatal("delete reported no record")
	}
	if got, _ := s.Get("posts", "a"); got != nil {
		t.Fatal("record survived delete")
	}
	_, tombs, _ := s.ChangedSince("posts", 0)
	if len(tombs) != 1 || tombs[0].Rkey != "a" {
		t.Fatalf("expected one tombstone for `a`, got %+v", tombs)
	}
	if again, _ := s.Delete("posts", "a"); again {
		t.Fatal("second delete should be a no-op")
	}
}

func TestRecreateClearsTheTombstone(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "x"})
	s.Delete("posts", "a")
	s.Put("posts", "a", core.Record{"body": "back"})
	recs, tombs, _ := s.ChangedSince("posts", 0)
	if len(recs) != 1 || len(tombs) != 0 {
		t.Fatalf("a recreated rkey is no longer deleted: recs=%d tombs=%d", len(recs), len(tombs))
	}
}

func TestChangedSinceReturnsOnlyNewerWrites(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "1"}) // seq 1
	s.Put("posts", "b", core.Record{"body": "2"}) // seq 2
	recs, _, _ := s.ChangedSince("posts", 1)
	if len(recs) != 1 || recs[0].Rkey != "b" {
		t.Fatalf("expected only `b`, got %+v", recs)
	}
}

func TestListIsRkeyOrderedAndETagTracksContent(t *testing.T) {
	s := mem(t)
	s.Put("posts", "b", core.Record{"body": "2"})
	s.Put("posts", "a", core.Record{"body": "1"})
	list, _ := s.List("posts")
	if len(list) != 2 || list[0].Rkey != "a" || list[1].Rkey != "b" {
		t.Fatalf("list not rkey-ordered: %+v", list)
	}
	etag1, _ := s.ListETag("posts")
	s.Put("posts", "a", core.Record{"body": "changed"})
	etag2, _ := s.ListETag("posts")
	if etag1 == "" || etag1 == etag2 {
		t.Fatalf("list ETag should move when a member CID moves (%q -> %q)", etag1, etag2)
	}
}

func TestCollectionsAreIsolated(t *testing.T) {
	s := mem(t)
	s.Put("posts", "a", core.Record{"body": "p"})
	s.Put("recipes", "a", core.Record{"body": "r"})
	if p, _ := s.List("posts"); len(p) != 1 {
		t.Fatalf("posts: %d", len(p))
	}
	if r, _ := s.List("recipes"); len(r) != 1 {
		t.Fatalf("recipes: %d", len(r))
	}
	if a, _ := s.List("art"); len(a) != 0 {
		t.Fatalf("art should be empty, got %d", len(a))
	}
}
