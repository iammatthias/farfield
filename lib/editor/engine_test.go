package editor

import (
	"context"
	"testing"
)

// TestEngine drives the module through the Go wrapper the desktop host uses,
// including the call order a host follows at startup.
func TestEngine(t *testing.T) {
	e, err := NewEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Placeholder("Write…")
	if err := e.Resize(1, 1, 2); err != nil { // a host that has not been laid out yet
		t.Fatal(err)
	}
	e.Render()
	if err := e.Resize(1200, 800, 2); err != nil {
		t.Fatal(err)
	}
	e.SetText(PreviewSample)
	e.Focus(true)
	fb, drew := e.Render()
	if !drew || len(fb) != 1200*800*4 {
		t.Fatalf("render drew=%v len=%d", drew, len(fb))
	}
	if _, drew := e.Render(); drew {
		t.Error("an unchanged frame was redrawn")
	}
	e.Command(CmdSelectAll)
	e.Insert("fresh start")
	if e.Text() != "fresh start" || e.Words() != 2 {
		t.Errorf("text %q words %d", e.Text(), e.Words())
	}
	e.Command(CmdUndo)
	if e.Text() != PreviewSample {
		t.Error("undo did not restore the sample")
	}
}
