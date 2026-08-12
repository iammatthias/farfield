package main

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// mk builds a synthetic registry, newest first, for one app: a snapshot every
// 6 hours reaching back the given number of days from now.
func mk(app string, now time.Time, days int) []Backup {
	var out []Backup
	for age := time.Duration(0); age <= time.Duration(days)*24*time.Hour; age += 6 * time.Hour {
		out = append(out, Backup{
			App:       app,
			CID:       app + "-" + age.String(),
			Size:      1,
			CreatedAt: now.Add(-age).Format(time.RFC3339),
		})
	}
	return out
}

func dropSet(t *testing.T, drop []Backup) map[string]bool {
	t.Helper()
	set := make(map[string]bool, len(drop))
	for _, b := range drop {
		set[b.CID] = true
	}
	return set
}

// TestPrunableLadder walks one app's 6-hourly history through every rung:
// everything ≤48h kept, then one per day to 30d, one per week to 90d,
// nothing past that.
func TestPrunableLadder(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	backups := mk("content", now, 120)
	dropped := dropSet(t, prunable(backups, now))

	perDay := map[string]int{}
	perWeek := map[string]int{}
	for _, b := range backups {
		if dropped[b.CID] {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, b.CreatedAt)
		age := now.Sub(ts)
		day := ts.UTC().Format("2006-01-02")
		y, w := ts.UTC().ISOWeek()
		switch {
		case age <= keepAllWindow:
			// every one kept — nothing to count
		case age <= keepDailyWindow:
			perDay[day]++
		case age <= keepWeeklyWindow:
			perWeek[weekKey(y, w)]++
		default:
			t.Errorf("snapshot older than the weekly window survived: %s (age %v)", b.CID, age)
		}
	}
	// Everything inside 48h must survive.
	for _, b := range backups {
		ts, _ := time.Parse(time.RFC3339, b.CreatedAt)
		if now.Sub(ts) <= keepAllWindow && dropped[b.CID] {
			t.Errorf("snapshot inside the keep-all window dropped: %s", b.CID)
		}
	}
	for day, n := range perDay {
		if n != 1 {
			t.Errorf("day %s kept %d snapshots, want 1", day, n)
		}
	}
	for wk, n := range perWeek {
		if n != 1 {
			t.Errorf("week %s kept %d snapshots, want 1", wk, n)
		}
	}
	if len(dropped) == 0 {
		t.Fatal("a 120-day 6-hourly history must lose something to the ladder")
	}
}

func weekKey(y, w int) string {
	return fmt.Sprintf("%d-W%02d", y, w)
}

// TestPrunableKeepsNewestPerDay: of two snapshots on the same UTC day past
// the 48h window, only the newer survives — and it is specifically the newer.
func TestPrunableKeepsNewestPerDay(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	early := Backup{App: "a", CID: "early",
		CreatedAt: time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC).Format(time.RFC3339)}
	late := Backup{App: "a", CID: "late",
		CreatedAt: time.Date(2026, 8, 5, 21, 0, 0, 0, time.UTC).Format(time.RFC3339)}
	fresh := Backup{App: "a", CID: "fresh", CreatedAt: now.Format(time.RFC3339)}

	dropped := dropSet(t, prunable([]Backup{fresh, late, early}, now))
	if dropped["late"] || dropped["fresh"] {
		t.Fatalf("wrong snapshot dropped: %v", dropped)
	}
	if !dropped["early"] {
		t.Fatal("the older same-day snapshot must be dropped")
	}
}

// TestPrunableAlwaysKeepsNewestPerApp: a retired app whose only snapshots are
// ancient keeps exactly its newest one.
func TestPrunableAlwaysKeepsNewestPerApp(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	old1 := Backup{App: "calendar", CID: "newest-old",
		CreatedAt: now.AddDate(0, -8, 0).Format(time.RFC3339)}
	old2 := Backup{App: "calendar", CID: "older",
		CreatedAt: now.AddDate(0, -9, 0).Format(time.RFC3339)}

	dropped := dropSet(t, prunable([]Backup{old1, old2}, now))
	if dropped["newest-old"] {
		t.Fatal("an app's newest snapshot must never be dropped")
	}
	if !dropped["older"] {
		t.Fatal("a beyond-horizon non-newest snapshot must be dropped")
	}
}

// TestPrunableAppsIndependent: one app's snapshots never claim another's
// day/week slots.
func TestPrunableAppsIndependent(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	day := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	a := Backup{App: "a", CID: "a-day", CreatedAt: day}
	b := Backup{App: "b", CID: "b-day", CreatedAt: day}

	if dropped := dropSet(t, prunable([]Backup{a, b}, now)); len(dropped) != 0 {
		t.Fatalf("cross-app slot collision: %v", dropped)
	}
}

// TestPrunableKeepsUndatable: a row whose created_at does not parse is kept.
func TestPrunableKeepsUndatable(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	weird := Backup{App: "a", CID: "weird", CreatedAt: "not-a-time"}
	fresh := Backup{App: "a", CID: "fresh", CreatedAt: now.Format(time.RFC3339)}

	if dropped := dropSet(t, prunable([]Backup{fresh, weird}, now)); dropped["weird"] {
		t.Fatal("an undatable snapshot must never be dropped")
	}
}

// TestPrunableIsStable: pruning the survivors again drops nothing — the
// ladder is idempotent at a fixed instant.
func TestPrunableIsStable(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	backups := mk("content", now, 120)
	dropped := dropSet(t, prunable(backups, now))

	var kept []Backup
	for _, b := range backups {
		if !dropped[b.CID] {
			kept = append(kept, b)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].CreatedAt > kept[j].CreatedAt })
	if again := prunable(kept, now); len(again) != 0 {
		t.Fatalf("second pass dropped %d more snapshots — ladder not idempotent", len(again))
	}
}
