package main

import (
	"fmt"
	"os"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/store"
)

// runMint issues one key from the command line and prints the token on stdout.
//
// The console is the normal way to mint, but it needs a browser and a session,
// and some callers have neither: provisioning a service on the host over SSH,
// or a deploy script wiring one app to another. Only the token's hash is
// stored, so — exactly as in the console — this is the one and only time the
// token is visible.
//
// stdout carries the token and nothing else, so `keys mint … | pbcopy` and
// shell capture both work; everything else goes to stderr.
func runMint(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: keys mint <name> <app> <scope> [expires-days]")
	}
	name, app, scope := args[0], args[1], args[2]

	var expires time.Time
	if len(args) > 3 {
		days := 0
		if _, err := fmt.Sscanf(args[3], "%d", &days); err != nil || days <= 0 {
			return fmt.Errorf("expires-days must be a positive number of days")
		}
		expires = time.Now().AddDate(0, 0, days)
	}

	ks, err := keys.Open(store.Env("KEYS_DB_PATH", "keys.sqlite"))
	if err != nil {
		return err
	}
	defer ks.Close()

	token, k, err := ks.Mint(name, app, scope, expires)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "minted %q for app %q, scope %q (id %s)\n",
		k.Name, k.App, k.Scope, k.ID)
	fmt.Fprintln(os.Stdout, token)
	return nil
}
