package main

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

func raceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "sideload.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestRecordInstallDoesNotLoseUpdates: a reusable token counts every install.
//
// Writing an absolute used_installs computed from a Token loaded at the start
// of the request loses concurrent updates — ten downloads each read used=0,
// each write 1, and the counter lands on 1 instead of 10. The install log a
// self token exists to keep was silently wrong under any parallelism.
func TestRecordInstallDoesNotLoseUpdates(t *testing.T) {
	db := raceDB(t)

	tok := &Token{Token: "tok-self", BuildID: "b1", Kind: kindSelf,
		State: stateActive, MaxInstalls: 0} // unlimited
	if err := insertToken(db, tok); err != nil {
		t.Fatal(err)
	}

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot := *tok // each request holds the copy it loaded, as the handler does
			_ = recordInstall(db, &snapshot, "ua", "ip")
		}()
	}
	wg.Wait()

	got, err := getToken(db, "tok-self")
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedInstalls != n {
		t.Errorf("used_installs = %d after %d concurrent installs, want %d — updates were lost",
			got.UsedInstalls, n, n)
	}
}

// TestRecordInstallCannotResurrect: a consumed token stays consumed.
//
// A download that began while the token was active and finished after another
// request consumed it used to write its stale snapshot back — state='active',
// consumed_at=” — reviving a link the UI advertises as self-revoking. Needs a
// cap above 1 to show, because a stale copy of a max=1 token happens to
// recompute the same consumed state.
func TestRecordInstallCannotResurrect(t *testing.T) {
	db := raceDB(t)

	tok := &Token{Token: "tok-share", BuildID: "b1", Kind: kindShare,
		State: stateActive, MaxInstalls: 2}
	if err := insertToken(db, tok); err != nil {
		t.Fatal(err)
	}

	stale := *tok // loaded while active, used=0 — the slow download

	// Two installs run to completion and consume the token.
	for i := 0; i < 2; i++ {
		cur, err := getToken(db, "tok-share")
		if err != nil {
			t.Fatal(err)
		}
		if err := recordInstall(db, cur, "ua", "ip"); err != nil {
			t.Fatal(err)
		}
	}
	if mid, _ := getToken(db, "tok-share"); mid.State != stateConsumed {
		t.Fatalf("setup: token should be consumed after 2 installs, got %q", mid.State)
	}

	// The slow one now finishes and writes back what it read at the start.
	if err := recordInstall(db, &stale, "ua-slow", "ip-slow"); err != nil {
		t.Fatal(err)
	}

	got, err := getToken(db, "tok-share")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != stateConsumed {
		t.Errorf("state = %q after a stale write-back, want %q — the link came back to life",
			got.State, stateConsumed)
	}
	if got.ConsumedAt == "" {
		t.Error("consumed_at was cleared by a stale write-back")
	}
	if got.UsedInstalls != 2 {
		t.Errorf("used_installs = %d, want 2 — the stale snapshot overwrote the real count",
			got.UsedInstalls)
	}
}
