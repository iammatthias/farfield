package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// State holds the latest reading and keeps it current. The original site
// subscribed to the provider's "block" event in the browser and re-rendered on
// every block; here one goroutine polls on the server and every visitor is
// served the same warm value, so the page costs no RPC calls per request.
type State struct {
	client *Client
	db     *sql.DB

	// ready closes as soon as a reading exists, so the first request after a
	// cold start can wait a moment for real numbers instead of rendering the
	// loading state and making the visitor reload.
	ready     chan struct{}
	readyOnce sync.Once

	mu       sync.RWMutex
	reading  Reading
	haveAny  bool
	haveName bool // epoch names have been read from the contract at least once
	lastOK   time.Time
	lastErr  error
}

// NewState builds the state, seeding it from the stored snapshot so the very
// first request after a restart renders real numbers even before the first
// poll completes.
func NewState(client *Client, db *sql.DB) *State {
	s := &State{client: client, db: db, ready: make(chan struct{})}
	if db != nil {
		if block, labels, ok := loadSnapshot(db); ok {
			// Marked not-live: it is the last thing we knew, not the head.
			s.reading = NewReading(block, Compute(block), labels, false)
			s.haveAny = true
			s.markReady()
		}
	}
	return s
}

// markReady releases anything waiting on the first reading.
func (s *State) markReady() { s.readyOnce.Do(func() { close(s.ready) }) }

// Wait returns the current reading, blocking up to d for the first one to
// arrive on a cold start. Once any reading exists it returns immediately, so
// this costs nothing on the steady-state path.
func (s *State) Wait(ctx context.Context, d time.Duration) (Reading, bool) {
	if r, ok := s.Current(); ok {
		return r, true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.ready:
	case <-timer.C:
	case <-ctx.Done():
	}
	return s.Current()
}

// Current returns the latest reading and whether one exists yet. Before the
// first successful poll on a fresh database there is nothing to show, and the
// page renders "Loading" — the same thing the original did while its provider
// was still connecting.
func (s *State) Current() (Reading, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reading, s.haveAny
}

// Endpoint reports which RPC endpoint is currently serving.
func (s *State) Endpoint() string { return s.client.Endpoint() }

// Err returns the most recent poll error, or nil if the last poll succeeded.
func (s *State) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Refresh pulls the head block and its epochs from the contract in a single
// batched round trip.
//
// The epoch names ride along only until they have been read once. They are
// owner-settable but in practice fixed, and asking for them every slot would
// add a third of the payload to every poll forever for data that has never
// changed. A restart re-reads them.
func (s *State) Refresh(ctx context.Context) error {
	s.mu.RLock()
	needLabels := !s.haveName
	known := s.reading.Labels
	haveAny := s.haveAny
	s.mu.RUnlock()
	if !haveAny {
		known = DefaultLabels
	}

	head, err := s.client.Head(ctx, needLabels)
	if err != nil {
		s.fail(err)
		return err
	}

	labels, named := known, !needLabels
	if head.HasLabels {
		labels, named = head.Labels, true
	} else if needLabels {
		slog.Warn("getEpochLabels unavailable; using known names")
	}

	s.mu.Lock()
	s.reading = NewReading(head.Block, head.Epochs, labels, true)
	s.haveAny = true
	s.haveName = named
	s.lastOK = time.Now()
	s.lastErr = nil
	s.mu.Unlock()
	s.markReady()

	if s.db != nil {
		if err := saveSnapshot(s.db, head.Block, labels); err != nil {
			slog.Warn("saving snapshot", "err", err)
		}
	}
	return nil
}

// fail records a poll error and demotes the current reading to stale, so the
// page can say so rather than presenting an old height as the head.
func (s *State) fail(err error) {
	slog.Warn("refresh failed", "err", err)
	s.mu.Lock()
	s.lastErr = err
	s.reading.Live = false
	s.mu.Unlock()
}

// Run polls until ctx is cancelled. every should be near the block time; the
// page cannot be fresher than this and there is no value in polling faster.
func (s *State) Run(ctx context.Context, every time.Duration) {
	poll := func() {
		// Budget for the whole refresh across failover; each individual
		// attempt is bounded separately by attemptTimeout.
		pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_ = s.Refresh(pollCtx)
	}

	poll() // immediately, so a cold start is warm within one round trip

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
