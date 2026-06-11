package main

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// checkTimeout bounds one probe end to end. Redirect handling is the
// http.Client default (follow up to 10) — a target that should not redirect
// asserts that via its expected status instead.
const checkTimeout = 10 * time.Second

// startChecker runs the uptime scheduler: one goroutine ticks every second
// and dispatches the targets that have come due, each on its own interval_s
// cadence. Targets are reloaded from the database on every dispatch cycle,
// so CRUD in the console takes effect without a restart. Next-due times live
// only in memory; a restart simply probes everything once, immediately.
func startChecker(db *sql.DB) {
	go func() {
		client := &http.Client{Timeout: checkTimeout}
		nextDue := make(map[int64]time.Time)
		dispatchDue(db, client, nextDue, time.Now())
		for now := range time.Tick(time.Second) {
			dispatchDue(db, client, nextDue, now)
		}
	}()
}

// dispatchDue probes (in their own goroutines) every enabled target whose
// next-due time has arrived, and forgets schedule state for targets that
// were deleted or disabled.
func dispatchDue(db *sql.DB, client *http.Client, nextDue map[int64]time.Time, now time.Time) {
	targets, err := listEnabledTargets(db)
	if err != nil {
		slog.Warn("checker: could not load targets", "err", err)
		return
	}
	seen := make(map[int64]bool, len(targets))
	for _, t := range targets {
		seen[t.ID] = true
		if due, ok := nextDue[t.ID]; ok && now.Before(due) {
			continue
		}
		interval := time.Duration(max(t.IntervalS, 1)) * time.Second
		nextDue[t.ID] = now.Add(interval)
		go func(t Target) {
			if err := recordCheck(db, t.ID, performCheck(client, t)); err != nil {
				slog.Warn("checker: could not record check", "target", t.Name, "err", err)
			}
		}(t)
	}
	for id := range nextDue {
		if !seen[id] {
			delete(nextDue, id)
		}
	}
}

// performCheck issues one HTTP request against the target and grades the
// answer: ok iff the status code equals the expected one. A transport
// failure records ok=false with the error text and status_code 0.
func performCheck(client *http.Client, t Target) checkResult {
	req, err := http.NewRequest(t.Method, t.URL, nil)
	if err != nil {
		return checkResult{Err: err.Error()}
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return checkResult{LatencyMS: latency, Err: err.Error()}
	}
	// Drain a bounded amount so the connection can be reused, then close.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return checkResult{
		StatusCode: resp.StatusCode,
		LatencyMS:  latency,
		OK:         resp.StatusCode == t.ExpectedStatus,
	}
}
