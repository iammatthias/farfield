// Command manifest prints the fleet and its command table as JSON, for
// surfaces that are not Go. The native client in clients/ builds from it, so
// its service list (names, ports, public hosts) comes from lib/fleet and its
// quick actions from lib/capability — the same two tables every Go surface
// reads — rather than from a copy that would drift.
//
//	go run ./lib/capability/cmd/manifest > fleet.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/iammatthias/farfield/lib/capability"
	"github.com/iammatthias/farfield/lib/fleet"
)

type service struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Public string `json:"public,omitempty"`
	Host   bool   `json:"host,omitempty"`
}

type command struct {
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases,omitempty"`
	Summary    string   `json:"summary"`
	Usage      string   `json:"usage"`
	TakesFiles bool     `json:"takesFiles,omitempty"`
}

func main() {
	var out struct {
		Services []service `json:"services"`
		Commands []command `json:"commands"`
	}
	for _, s := range fleet.Services() {
		out.Services = append(out.Services, service{s.Name, s.Port, s.Public, s.Host})
	}
	for _, c := range capability.Fleet() {
		out.Commands = append(out.Commands, command{c.Name, c.Aliases, c.Summary, c.Usage(), c.TakesFiles})
	}
	if len(out.Services) == 0 {
		fmt.Fprintln(os.Stderr, "manifest: the registry is empty")
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
