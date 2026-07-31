// Command epochs serves a frontend for the Epochs contract on Ethereum
// mainnet — a crypto-native temporal wayfinding system in which every block is
// the base unit and each concentric epoch is 11^n blocks long.
//
// It is a faithful rebuild of epochs.cosmiccomputation.org, the Next.js site
// that went offline. The markup, copy, type scale, palette and the
// concentric-circle diagram are reproduced from the site's own webpack bundles
// and its Wayback capture; only the stack is farfield. Because of that, this
// app deliberately does not use lib/theme or follow the house style — see the
// note in templates/base.html before "fixing" its appearance.
//
// Everything is read-only: no wallet, no login, no writes. The server polls
// the contract once per block and every visitor is served the same warm
// reading, so the page works with JavaScript disabled.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// "health" probes the running server's /status for Docker healthchecks.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(web.Health(store.Env("EPOCHS_PORT", "8802")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("EPOCHS_PORT", "8802")

	client := NewClient(rpcURLs())

	// The database only caches the last good reading and backs pulse. Neither
	// is essential to rendering, so an open failure degrades rather than exits.
	var db = mustOpenDB()
	if db != nil {
		defer db.Close()
	}

	state := NewState(client, db)
	srv, err := NewServer(state,
		store.Env("EPOCHS_PUBLIC_URL", "https://epochs.farfield.systems"),
		store.Env("EPOCHS_FONTS_DIR", ""))
	if err != nil {
		slog.Error("building server", "err", err)
		os.Exit(1)
	}

	// Poll on roughly the block time. Serve does its own signal handling, so
	// this context is only here to stop the poller on shutdown.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go state.Run(ctx, pollInterval())

	var handler http.Handler = srv.Routes()
	if db != nil {
		rec := pulse.New(db, "epochs")
		defer rec.Close()
		handler = rec.Wrap(handler)
	}

	if err := web.Serve(host, port, web.LogRequests(web.Gzip(handler))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// mustOpenDB opens the snapshot/pulse database, returning nil when it cannot
// be opened. A public read-only page must not fail to boot over its cache.
func mustOpenDB() *sql.DB {
	db, err := openDB(store.Env("EPOCHS_DB_PATH", "epochs.sqlite"))
	if err != nil {
		slog.Warn("snapshot cache and pulse disabled: could not open database", "err", err)
		return nil
	}
	return db
}

// rpcURLs reads the endpoint list from EPOCHS_RPC_URL — comma-separated, tried
// in order. Empty falls back to the public defaults.
func rpcURLs() []string {
	raw := store.Env("EPOCHS_RPC_URL", "")
	if raw == "" {
		return nil
	}
	var out []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// pollInterval is how often the contract is read. It defaults to one slot.
func pollInterval() time.Duration {
	d, err := time.ParseDuration(store.Env("EPOCHS_POLL_INTERVAL", "12s"))
	if err != nil || d < time.Second {
		return 12 * time.Second
	}
	return d
}
