// Command farfield drives the fleet from a shell.
//
// It is one of three faces on lib/capability, and the reason the other two are
// cheap. switchboard dispatches the same commands in-process for a text message;
// agents reach them through markdown slash commands in ~/.claude/commands that do
// nothing but shell out to this binary. Declaring a command once and deriving
// every surface from it is what keeps the CLI, the phone, and the agent from
// drifting into three different spellings of "post to the feed".
//
// Usage:
//
//	farfield <command> [args...]
//	farfield help
//	farfield qr <target> [label] [--save out.png]
//	farfield commands [dir]        regenerate the agents' slash commands
//
// Commands take positional arguments in the same loose shape they take over
// iMessage — no flags, no required quoting, the last argument swallowing the rest
// of the line — because the grammar was designed for a phone keyboard and the CLI
// is the one that should bend.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/capability"
	"github.com/iammatthias/farfield/lib/store"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the binary is run from

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "farfield: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	reg := capability.NewRegistry(capability.Fleet()...)

	if len(argv) == 0 {
		fmt.Println(help(reg))
		return nil
	}

	// A leading slash is accepted but not required. Nobody types `/qr` at a shell
	// prompt, and everybody copies one out of a text message.
	name := strings.TrimPrefix(strings.ToLower(argv[0]), "/")
	if name == "help" || name == "-h" || name == "--help" {
		fmt.Println(help(reg))
		return nil
	}

	// A meta-command, deliberately outside the registry: it acts on the command
	// table rather than on the fleet, and it has no business appearing in a text
	// message.
	if name == "commands" {
		dir := ".claude/commands"
		if len(argv) > 1 {
			dir = argv[1]
		}
		return writeCommandFiles(dir)
	}

	args, savePath := takeSaveFlag(argv[1:])

	spec, ok := reg.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown command %q\n\n%s", name, help(reg))
	}

	in, err := spec.Bind(strings.Join(args, " "), nil)
	if err != nil {
		return err
	}
	in.Actor = "cli"

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	res, err := spec.Run(ctx, capability.New(capability.FromEnv()), in)
	if err != nil {
		return err
	}
	return render(res, savePath)
}

// takeSaveFlag pulls `--save <path>` out of the argument list.
//
// It is the one flag, and it exists because a QR code's answer is a picture: over
// iMessage the raster is sent inline, and at a shell there has to be somewhere to
// put it. Everything else stays positional.
func takeSaveFlag(argv []string) (rest []string, savePath string) {
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--save" || argv[i] == "-o" {
			if i+1 < len(argv) {
				savePath = argv[i+1]
				i++
			}
			continue
		}
		rest = append(rest, argv[i])
	}
	return rest, savePath
}

// render writes one result. Output is deliberately bare — a ref or a URL and
// nothing else — so `farfield scrap ... | pbcopy` does the obvious thing and an
// agent reading stdout has no prose to parse.
func render(res capability.Result, savePath string) error {
	if len(res.Image) > 0 {
		if savePath == "" {
			// Without somewhere to put it, say what was made rather than spraying
			// bytes at a terminal.
			fmt.Println(res.Ref)
			return nil
		}
		if err := os.WriteFile(savePath, res.Image, 0o644); err != nil {
			return err
		}
		fmt.Println(savePath)
		return nil
	}
	if res.Text != "" {
		fmt.Println(res.Text)
		return nil
	}
	if res.Ref != "" {
		fmt.Println(res.Ref)
	}
	return nil
}

func help(reg *capability.Registry) string {
	return reg.Help("farfield — drive the fleet\n\nusage: farfield <command> [args...]")
}
