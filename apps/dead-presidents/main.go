// Command dead-presidents serves the Dead Presidents GPT browser inference app:
// a character-level GPT trained on US presidential speeches whose weights run
// entirely client-side in a Web Worker. The server only ships static assets and
// the shared farfield theme; all inference happens in the browser.
package main

import (
	"context"
	"embed"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed web
var webFS embed.FS

func main() {
	_ = store.LoadEnv()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("DEAD_PRESIDENTS_PORT", "8796")

	site, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("loading embedded site", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	// Serve the shared farfield theme at the same path the other apps use, so
	// dead-presidents inherits the canonical stylesheet instead of a local copy
	// that drifts.
	mux.HandleFunc("GET /static/styles.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = io.WriteString(w, theme.CSS)
	})
	mux.Handle("/", http.FileServerFS(site))

	srv := &http.Server{
		Addr:    net.JoinHostPort(host, port),
		Handler: logRequests(mux),
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
