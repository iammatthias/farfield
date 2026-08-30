// Command publictargets prints the fleet's public /status URLs, one per line.
//
// It exists so the external uptime canary can ask the registry what to watch
// instead of carrying its own list. The canary runs on a GitHub runner with no
// access to the box, so it cannot ask the fleet directly — the repo is the only
// source of truth available from outside, and this is the smallest way to read
// it. Standard library only, no flags, no dependencies: a watchdog's target
// list should not be able to break in an interesting way.
package main

import (
	"fmt"
	"os"

	"github.com/iammatthias/farfield/lib/fleet"
)

func main() {
	out := ""
	for _, s := range fleet.PublicServices() {
		out += s.PublicStatusURL() + "\n"
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "publictargets: registry reports no public services — refusing to print an empty target list")
		os.Exit(1)
	}
	fmt.Print(out)
}
