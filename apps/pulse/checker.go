package main

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// checkTimeout bounds one probe end to end. 15s leaves headroom for the
// Cloudflare-tunnel hairpin path, where a cold tunnel connection can take
// several seconds to dial before the target even sees the request. Redirect
// handling is the http.Client default (follow up to 10) — a target that
// should not redirect asserts that via its expected status instead.
const checkTimeout = 15 * time.Second

// retryDelay is the pause before the single in-probe retry of a failed
// check. A var so tests can shrink it.
var retryDelay = 3 * time.Second

// defaultFailThreshold is how many consecutive failed checks open an
// incident when PULSE_FAIL_THRESHOLD is unset.
const defaultFailThreshold = 2

// failThreshold reads PULSE_FAIL_THRESHOLD: the number of consecutive failed
// checks that open an incident. 1 restores the original open-on-first-fail
// behavior; unset or invalid values fall back to defaultFailThreshold.
func failThreshold() int {
	raw := store.Env("PULSE_FAIL_THRESHOLD", "")
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 {
		return n
	}
	return defaultFailThreshold
}

// startChecker runs the uptime scheduler: one goroutine ticks every second
// and dispatches the targets that have come due, each on its own interval_s
// cadence. Targets are reloaded from the database on every dispatch cycle,
// so CRUD in the console takes effect without a restart. Next-due times live
// only in memory; a restart simply probes everything once, immediately.
func startChecker(db *sql.DB) {
	go func() {
		client := &http.Client{Timeout: checkTimeout}
		threshold := failThreshold()
		nextDue := make(map[int64]time.Time)
		dispatchDue(db, client, nextDue, time.Now(), threshold)
		for now := range time.Tick(time.Second) {
			dispatchDue(db, client, nextDue, now, threshold)
		}
	}()
}

// dispatchDue probes (in their own goroutines) every enabled target whose
// next-due time has arrived, and forgets schedule state for targets that
// were deleted or disabled.
func dispatchDue(db *sql.DB, client *http.Client, nextDue map[int64]time.Time, now time.Time, threshold int) {
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
			if err := recordCheck(db, t.ID, probeTarget(client, t), threshold); err != nil {
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

// probeTarget grades a target with one built-in retry: a failed probe is
// re-issued once after retryDelay and only the retry's outcome — including
// its latency — is recorded. A recorded failure therefore means two
// consecutive misses a few seconds apart, which keeps one-off transport
// flakes (common on the Cloudflare-tunnel hairpin path) out of the checks
// data entirely, before the incident debounce in recordCheck even applies.
func probeTarget(client *http.Client, t Target) checkResult {
	res := performCheck(client, t)
	if res.OK {
		return res
	}
	time.Sleep(retryDelay)
	return performCheck(client, t)
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
