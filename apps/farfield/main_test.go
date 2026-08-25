package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/capability"
)

// TestTakeSaveFlag: --save is the one flag, so it has to come out of the
// argument list cleanly wherever it appears — everything else is positional and
// must survive untouched, spaces and all.
func TestTakeSaveFlag(t *testing.T) {
	cases := []struct {
		in       []string
		wantArgs []string
		wantPath string
	}{
		{[]string{"https://x.com", "a label"}, []string{"https://x.com", "a label"}, ""},
		{[]string{"https://x.com", "--save", "out.png"}, []string{"https://x.com"}, "out.png"},
		{[]string{"--save", "out.png", "https://x.com"}, []string{"https://x.com"}, "out.png"},
		{[]string{"-o", "out.png", "https://x.com", "label"}, []string{"https://x.com", "label"}, "out.png"},
		// A trailing --save with nothing after it must not eat an argument that
		// is not there.
		{[]string{"https://x.com", "--save"}, []string{"https://x.com"}, ""},
	}
	for _, tc := range cases {
		args, path := takeSaveFlag(tc.in)
		if strings.Join(args, "|") != strings.Join(tc.wantArgs, "|") {
			t.Errorf("takeSaveFlag(%v) args = %v, want %v", tc.in, args, tc.wantArgs)
		}
		if path != tc.wantPath {
			t.Errorf("takeSaveFlag(%v) path = %q, want %q", tc.in, path, tc.wantPath)
		}
	}
}

// TestRunHelpNeedsNoNetwork: help and a mistyped command must answer without
// touching the fleet, because they are what someone runs when they do not yet
// have credentials configured.
func TestRunHelpNeedsNoNetwork(t *testing.T) {
	for _, argv := range [][]string{nil, {"help"}, {"--help"}} {
		if err := run(argv); err != nil {
			t.Errorf("run(%v) = %v, want nil", argv, err)
		}
	}
	err := run([]string{"definitely-not-a-command"})
	if err == nil {
		t.Fatal("run with an unknown command: want an error")
	}
	// The error carries the command list, so a typo is self-correcting.
	if !strings.Contains(err.Error(), "/status") {
		t.Errorf("error %q does not include the command list", err)
	}
}

// A leading slash is accepted, because people copy commands out of a text
// message and paste them at a shell.
func TestRunAcceptsLeadingSlash(t *testing.T) {
	if err := run([]string{"/help"}); err != nil {
		t.Errorf("run(/help) = %v", err)
	}
}

// TestRenderSavesImage: a QR code's answer is a picture, and --save is where it
// goes. Without a path, the id is printed rather than raw bytes at a terminal.
func TestRenderSavesImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.png")
	res := capability.Result{Ref: "abc", Image: []byte("PNGDATA"), ImageName: "abc.png"}

	if err := render(res, path); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "PNGDATA" {
		t.Errorf("wrote %q, want the image bytes", got)
	}

	// No path: must not error, and must not write anything.
	if err := render(res, ""); err != nil {
		t.Errorf("render without --save: %v", err)
	}
}

// repoRoot walks up to the directory holding go.work.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found — running outside the workspace")
	return ""
}

// TestCommandFilesAreCurrent fails when .claude/commands has drifted from the
// command table.
//
// Those files are what agents actually see — omp and Claude Code both read
// ~/.claude/commands, which is a symlink to the committed directory. Without this
// check, adding a command to lib/capability would give the CLI and iMessage a new
// verb while every agent kept offering the old list, which is exactly the split
// this whole arrangement exists to prevent.
func TestCommandFilesAreCurrent(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".claude", "commands")
	want := commandFiles(capability.NewRegistry(capability.Fleet()...))

	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s is missing — run `farfield commands`", name)
			continue
		}
		if string(got) != content {
			t.Errorf("%s is stale — run `farfield commands`", name)
		}
	}

	// And nothing left behind by a command that was removed.
	found, err := filepath.Glob(filepath.Join(dir, commandFilePrefix+"*.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range found {
		if _, ok := want[filepath.Base(path)]; !ok {
			t.Errorf("%s describes a command that no longer exists — run `farfield commands`",
				filepath.Base(path))
		}
	}
}

// TestCommandFilesAvoidHarnessBuiltins guards the reason for the ff- prefix.
//
// omp consumes several slash commands in its input controller before file
// commands are ever consulted; a farfield /status there would silently open the
// extension centre instead. The prefix is what keeps every command in the table
// reachable, so it must not be dropped for being ugly.
func TestCommandFilesAvoidHarnessBuiltins(t *testing.T) {
	// Names known to be taken by omp or Claude Code.
	taken := map[string]bool{
		"status": true, "model": true, "mcp": true, "settings": true, "exit": true,
		"help": true, "resume": true, "new": true, "compact": true, "memory": true,
		"review": true, "plan": true, "export": true, "share": true, "usage": true,
	}
	for name := range commandFiles(capability.NewRegistry(capability.Fleet()...)) {
		bare := strings.TrimSuffix(name, ".md")
		if taken[bare] {
			t.Errorf("%s collides with a harness built-in", bare)
		}
		if !strings.HasPrefix(bare, commandFilePrefix) {
			t.Errorf("%s is not namespaced with %q", bare, commandFilePrefix)
		}
	}
}
