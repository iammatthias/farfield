// Command content is the farfield content service — durable, long-form
// content organised into user-managed collections. It exposes an HTML admin
// UI for writing, editing, publishing, and moderating entries, and a public
// JSON read API for consumers. Ephemeral short-form posts live in a separate
// app (feed); content is for collections that are meant to last.
//
// Usage:
//
//	content                       serve the HTTP service (default)
//	content import-vault <dir>     import an Obsidian content vault — each
//	                               subfolder is a collection, each .md file
//	                               an entry with YAML frontmatter
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
		port := store.Env("CONTENT_PORT", "8787")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "import-vault":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: content import-vault <dir>")
			os.Exit(2)
		}
		if err := runImportVault(os.Args[2]); err != nil {
			slog.Error("import-vault failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: content [serve | import-vault <dir>]")
		os.Exit(2)
	}
}

// runImportVault opens the database and imports an Obsidian content vault.
func runImportVault(dir string) error {
	db, err := openDB(store.Env("CONTENT_DB_PATH", "content.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	return importVault(db, dir)
}
