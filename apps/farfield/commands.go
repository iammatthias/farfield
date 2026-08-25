package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iammatthias/farfield/lib/capability"
)

// commandFilePrefix namespaces the generated slash commands.
//
// Agent harnesses own their own command namespace and several of ours would
// collide: omp binds /status as an alias for its extension centre and consumes
// it before file commands are ever consulted, so a farfield /status there would
// silently do something else. The prefix matches the ff-* helpers on the box, so
// "farfield thing" reads the same at a shell and in an agent.
const commandFilePrefix = "ff-"

// renderCommandFile writes one markdown slash command.
//
// The body is a prompt, not a script: omp and Claude Code both expand $ARGUMENTS
// into the template and then hand the result to the model, so the file's job is
// to name the exact command and then get out of the way. The instructions to
// report nothing but the output are there because these are the same actions a
// text message triggers, and a paragraph of explanation is the wrong answer on a
// phone as well as in a terminal.
func renderCommandFile(s *capability.Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ndescription: %s\n---\n\n", s.Summary)
	fmt.Fprintf(&b, "Run exactly this, substituting the arguments given:\n\n")
	fmt.Fprintf(&b, "```sh\nfarfield %s $ARGUMENTS\n```\n\n", s.Name)
	fmt.Fprintf(&b, "Usage: `%s`\n\n", s.Usage())
	b.WriteString("Report only what the command printed — no preamble, no summary, ")
	b.WriteString("no explanation of what the command does. If it fails, report the error verbatim.\n")
	return b.String()
}

// commandFiles renders every command in the registry, keyed by filename.
func commandFiles(reg *capability.Registry) map[string]string {
	out := map[string]string{}
	for _, s := range reg.Specs() {
		out[commandFilePrefix+s.Name+".md"] = renderCommandFile(s)
	}
	return out
}

// writeCommandFiles regenerates the slash commands in dir.
//
// Generated rather than written, for the same reason help is: the set of things
// farfield can do is declared in exactly one place, and every surface is a
// rendering of it. A test regenerates into a temp dir and diffs, so a command
// added without regenerating fails CI instead of quietly missing from the agents.
func writeCommandFiles(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := commandFiles(capability.NewRegistry(capability.Fleet()...))

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(files[name]), 0o644); err != nil {
			return err
		}
		fmt.Println(path)
	}

	// A command removed from the table has to lose its file too, or agents keep
	// offering something that no longer exists.
	stale, err := filepath.Glob(filepath.Join(dir, commandFilePrefix+"*.md"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if _, current := files[filepath.Base(path)]; current {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Println("removed " + path)
	}
	return nil
}
