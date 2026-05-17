// Command backup snapshots every farfield app's SQLite database into R2 (via
// the blobs service) and provides an HTML admin UI to review and manage the
// snapshots. A snapshot is a whole database — every markdown body included —
// so it is a complete, restorable backup.
//
// Usage:
//
//	backup                                  serve the HTTP admin UI (default)
//	backup snapshot                         snapshot every app's database now
//	backup restore <app> <cid>              dry-run a restore from snapshot <cid>
//	backup restore <app> <cid> --confirm    perform the restore
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		host := store.Env("HOST", "127.0.0.1")
		port := store.Env("BACKUP_PORT", "8791")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "snapshot":
		if err := runSnapshot(); err != nil {
			slog.Error("snapshot failed", "err", err)
			os.Exit(1)
		}
	case "restore":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: backup restore <app> <cid> [--confirm]")
			os.Exit(2)
		}
		confirm := len(os.Args) > 4 && os.Args[4] == "--confirm"
		if err := runRestore(os.Args[2], os.Args[3], confirm); err != nil {
			slog.Error("restore failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr,
			"usage: backup [serve | snapshot | restore <app> <cid> [--confirm]]")
		os.Exit(2)
	}
}
