package main

import (
	"database/sql"
	"log/slog"
	"strings"

	"github.com/iammatthias/farfield/lib/fleet"
	"github.com/iammatthias/farfield/lib/store"
)

// seedTargets creates an uptime target for every public fleet service pulse
// has never seen, probing its /status through the tunnel — the path a real
// visitor takes. The registry (lib/fleet) is the source of truth for what
// exists; this is what keeps pulse's coverage from drifting when a service
// ships (the audit found it watching 12 of 15).
//
// Seeding is additive and once-per-name: seeded_targets records every name
// this has ever handled, so an operator deleting or disabling a target is a
// decision that sticks — a restart never resurrects it. A target whose URL
// already mentions the service's host is adopted as-is (and marked seeded)
// rather than duplicated.
func seedTargets(db *sql.DB) {
	rows, err := db.Query(`SELECT url FROM targets`)
	if err != nil {
		slog.Warn("seed: could not read targets", "err", err)
		return
	}
	var existing []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			slog.Warn("seed: could not scan target", "err", err)
			return
		}
		existing = append(existing, u)
	}
	rows.Close()
	if rows.Err() != nil {
		slog.Warn("seed: could not list targets", "err", rows.Err())
		return
	}

	for _, svc := range fleet.Services() {
		if svc.Public == "" {
			continue // tailnet-only — the fleet status page watches it
		}
		var seen int
		if err := db.QueryRow(`SELECT COUNT(*) FROM seeded_targets
			WHERE name = ?`, svc.Name).Scan(&seen); err != nil {
			slog.Warn("seed: ledger read failed", "name", svc.Name, "err", err)
			continue
		}
		if seen > 0 {
			continue // handled once already; operator decisions stand
		}

		covered := false
		for _, u := range existing {
			if strings.Contains(u, svc.Public) {
				covered = true
				break
			}
		}
		if !covered {
			t := &Target{Name: svc.Name, URL: svc.PublicURL() + "status",
				Method: "GET", ExpectedStatus: 200, IntervalS: 60, Enabled: true}
			if err := insertTarget(db, t); err != nil {
				slog.Warn("seed: could not insert target", "name", svc.Name, "err", err)
				continue
			}
			slog.Info("seeded uptime target", "name", svc.Name, "url", t.URL)
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO seeded_targets
			(name, seeded_at) VALUES (?, ?)`, svc.Name, store.NowRFC3339()); err != nil {
			slog.Warn("seed: ledger write failed", "name", svc.Name, "err", err)
		}
	}
}
