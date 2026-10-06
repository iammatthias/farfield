package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/store"
)

// provisionEdge is one env var the fleet's service-to-service calls read,
// the app its value must open, and who presents it.
//
// The env model is one var per callee, read by every caller of it — content,
// feed and backup all read BLOBS_API_KEY — so a key per var is the finest
// grain this can mint without new var names. SelfKey marks a var the callee
// ALSO reads as its own env key (blobs reads BLOBS_API_KEY as its write key):
// there the minted token is accepted twice over, and revoking it in the
// console does not stop it until .env stops carrying it.
type provisionEdge struct {
	Env     string
	App     string
	Scope   string
	Callers string
	SelfKey bool
}

// provisionEdges lists every service-to-service call. Read-only callers get
// read scope; blobs' reference scan needs write because it lists drafts.
// Keep this in step with docker-compose.yml when a service gains a sibling.
var provisionEdges = []provisionEdge{
	{"BLOBS_API_KEY", "blobs", keys.ScopeWrite, "content, feed, backup → blobs", true},
	{"CONTENT_API_KEY", "content", keys.ScopeWrite, "blobs, feed → content", true},
	{"CONTENT_READ_KEY", "content", keys.ScopeRead, "apex → content", true},
	{"FEED_READ_KEY", "feed", keys.ScopeRead, "blobs, content, apex → feed", true},
	{"BOOKMARKS_READ_KEY", "bookmarks", keys.ScopeRead, "content → bookmarks", true},
	{"SWITCHBOARD_FEED_KEY", "feed", keys.ScopeWrite, "switchboard → feed", false},
	{"SWITCHBOARD_BOOKMARKS_KEY", "bookmarks", keys.ScopeWrite, "switchboard → bookmarks", false},
	{"SWITCHBOARD_SCRAP_KEY", "scrap", keys.ScopeWrite, "switchboard → scrap", false},
	{"SWITCHBOARD_QR_KEY", "qr", keys.ScopeWrite, "switchboard → qr", false},
	{"SWITCHBOARD_PULSE_KEY", "pulse", keys.ScopeRead, "switchboard → pulse", false},
}

// runProvision is `keys provision`: mint one key per service-to-service env
// var into KEYS_DB_PATH and print the .env lines on stdout.
func runProvision(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: keys provision   (no arguments; prints .env lines)")
	}
	ks, err := keys.Open(store.Env("KEYS_DB_PATH", "keys.sqlite"))
	if err != nil {
		return err
	}
	defer ks.Close()
	return provision(ks, os.Stdout, time.Now())
}

// provision does the minting and printing, against any store and writer.
// Tokens exist only in this output — as with every mint, only hashes are
// kept — so the operator pastes it or loses it.
func provision(ks *keys.Store, w io.Writer, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# farfield service keys — `keys provision`, %s\n", now.UTC().Format(time.RFC3339))
	b.WriteString("# Replace these lines in the host .env, ff-deploy, then revoke the previous\n")
	b.WriteString("# keys of the same names in the keys console. (self) marks a var the callee\n")
	b.WriteString("# also reads as its own env key: anything else holding the old value — the\n")
	b.WriteString("# website, Shortcuts, plugins — must be updated too.\n")
	for _, e := range provisionEdges {
		token, _, err := ks.Mint("service: "+e.Env+" ("+e.Callers+")", e.App, e.Scope, time.Time{})
		if err != nil {
			return fmt.Errorf("mint %s: %w", e.Env, err)
		}
		self := ""
		if e.SelfKey {
			self = " (self)"
		}
		fmt.Fprintf(&b, "\n# %s · %s%s\n%s=%s\n", e.Callers, e.Scope, self, e.Env, token)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
