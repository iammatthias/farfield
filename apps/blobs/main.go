// Command blobs is the farfield blob service — a content-addressed media
// store. It exposes an HTML admin UI for uploading and moderating blobs and a
// public JSON/bytes API for consumers. Blob bytes live in a byte store (a
// local directory or Cloudflare R2); metadata lives in SQLite.
//
// Usage:
//
//	blobs                          serve the HTTP service (default)
//	blobs import-sidecars          copy R2 <cid>.json sidecars into SQLite
//	blobs prune-sidecars           dry-run: report sidecars to delete from R2
//	blobs prune-sidecars --confirm delete the <cid>.json sidecars from R2
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
)

func main() {
	_ = store.LoadEnv()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		host := store.Env("HOST", "127.0.0.1")
		port := store.Env("BLOBS_PORT", "8789")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "import-sidecars":
		if err := runMigration(false, false); err != nil {
			slog.Error("import-sidecars failed", "err", err)
			os.Exit(1)
		}
	case "prune-sidecars":
		confirm := len(os.Args) > 2 && os.Args[2] == "--confirm"
		if err := runMigration(true, confirm); err != nil {
			slog.Error("prune-sidecars failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr,
			"usage: blobs [serve | import-sidecars | prune-sidecars [--confirm]]")
		os.Exit(2)
	}
}

// runMigration opens the database and byte store, then runs a sidecar
// migration. prune=false runs the import; prune=true runs the prune (a dry
// run unless confirm is set).
func runMigration(prune, confirm bool) error {
	db, err := openDB(store.Env("BLOBS_DB_PATH", "blobs.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("blob store", "backend", desc)

	if prune {
		return pruneSidecars(db, bs, confirm)
	}
	return importSidecars(db, bs)
}
