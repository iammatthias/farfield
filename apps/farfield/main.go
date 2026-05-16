// Command farfield is the Farfield content backend CLI.
//
// It publishes markdown into the running services: import bulk-loads a
// directory of collections, push publishes individual files, migrate-images
// lifts ipfs:// images onto the blob service, extract-series lifts runs of
// inline media into series records, and status reports health.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultService = "http://127.0.0.1:8787"
	defaultBlobs   = "http://127.0.0.1:8789"
	defaultSchemas = "schemas/content"
)

// httpClient is shared by every command; the timeout covers slow IPFS
// gateway fetches during migrate-images.
var httpClient = &http.Client{Timeout: 60 * time.Second}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = cmdImport(os.Args[2:])
	case "push":
		err = cmdPush(os.Args[2:])
	case "migrate-images":
		err = cmdMigrateImages(os.Args[2:])
	case "extract-series":
		err = cmdExtractSeries(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `farfield — content backend CLI

  farfield import <dir>      bulk-import a directory of markdown collections
  farfield push <file>...    publish individual markdown files
  farfield migrate-images    move ipfs:// images onto the blob service
  farfield extract-series    lift runs of inline media into series records
  farfield status            print a service's status

Run a command with -h for its flags. The write token is read from
FARFIELD_TOKEN (falls back to 'dev-token' for local dev).
`)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	service := fs.String("service", defaultService, "service URL")
	_ = fs.Parse(args)

	resp, err := httpClient.Get(*service + "/status")
	if err != nil {
		return fmt.Errorf("reaching the service: %w", err)
	}
	defer resp.Body.Close()
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return fmt.Errorf("parsing the response: %w", err)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
	return nil
}

// splitPositionals separates a leading run of positional arguments from the
// flags that follow. Go's flag package stops at the first positional, so this
// lets `farfield import <dir> --dry-run` work, not just `--dry-run <dir>`.
func splitPositionals(args []string) (positionals, flags []string) {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		i++
	}
	return args[:i], args[i:]
}

// token reads the write token from the environment.
func token() string {
	if t := os.Getenv("FARFIELD_TOKEN"); t != "" {
		return t
	}
	fmt.Fprintln(os.Stderr, "warning: FARFIELD_TOKEN unset — using 'dev-token' (local dev only)")
	return "dev-token"
}
