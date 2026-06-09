package web

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Serve runs h on host:port with sane timeouts and a graceful shutdown on
// SIGINT/SIGTERM. ReadHeaderTimeout bounds slow-header clients (slowloris);
// there is deliberately no Read/WriteTimeout because farfield apps stream
// uploads and downloads of up to 100 MiB.
func Serve(host, port string, h http.Handler) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// Health probes the app's own /status endpoint and returns a process exit
// code: 0 when it answers 200, 1 otherwise. Each app's main wires this to a
// "health" subcommand so distroless containers (no shell, no curl) can back
// a Docker healthcheck with their own binary.
func Health(port string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/status", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health: status", resp.StatusCode)
		return 1
	}
	return 0
}
