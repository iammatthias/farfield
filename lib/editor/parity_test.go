package editor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parity suite is the contract between this module and every host that is
// not Go. Each script in testdata/parity drives the editor through its export
// surface; each "check" records what a host can observe — the text, the
// selection, the revision, the word count and a hash of the rendered frame —
// into a golden file beside it. Hosts in other languages (the native client in
// clients/) replay the same scripts and must reproduce the golden file
// exactly, which is what "the same editor" means once it leaves the browser.
//
//	go test ./lib/editor -run Parity -update   rewrite the golden files

var updateParity = flag.Bool("update", false, "rewrite testdata/parity/*.golden.json")

type parityScript struct {
	Name   string     `json:"name"`
	Width  int        `json:"width"`
	Height int        `json:"height"`
	Scale  float64    `json:"scale"`
	Ops    []parityOp `json:"ops"`
}

type parityOp struct {
	Op     string `json:"op"`
	S      string `json:"s,omitempty"`
	K      int    `json:"k,omitempty"`
	M      int    `json:"m,omitempty"`
	C      int    `json:"c,omitempty"`
	Kind   int    `json:"kind,omitempty"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	Clicks int    `json:"clicks,omitempty"`
}

// ParityCheck is one observation. Frame is the sha-256 of the RGBA
// framebuffer after a render.
type ParityCheck struct {
	Text     string `json:"text"`
	SelStart uint32 `json:"selStart"`
	SelEnd   uint32 `json:"selEnd"`
	Revision uint32 `json:"revision"`
	Words    int    `json:"words"`
	Frame    string `json:"frame"`
}

func TestParity(t *testing.T) {
	scripts, err := filepath.Glob("testdata/parity/*.json")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, path := range scripts {
		if strings.HasSuffix(path, ".golden.json") {
			continue
		}
		n++
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var s parityScript
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			got := runParity(t, s)
			golden := strings.TrimSuffix(path, ".json") + ".golden.json"
			if *updateParity {
				b, _ := json.MarshalIndent(got, "", "  ")
				if err := os.WriteFile(golden, append(b, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			b, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v — run with -update to record it", err)
			}
			var want []ParityCheck
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d checks, golden has %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("check %d:\n got  %+v\n want %+v", i, got[i], want[i])
				}
			}
		})
	}
	if n == 0 {
		t.Fatal("no parity scripts")
	}
}

func runParity(t *testing.T, s parityScript) []ParityCheck {
	e, err := NewEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Resize(s.Width, s.Height, s.Scale); err != nil {
		t.Fatal(err)
	}
	e.Focus(true)
	var out []ParityCheck
	for _, op := range s.Ops {
		switch op.Op {
		case "set_text":
			e.SetText(op.S)
		case "insert":
			e.Insert(op.S)
		case "key":
			e.Key(op.K, op.M)
		case "command":
			e.Command(op.C)
		case "pointer":
			e.Pointer(op.Kind, op.X, op.Y, op.M, op.Clicks)
		case "check":
			fb, _ := e.Render()
			sum := sha256.Sum256(fb)
			out = append(out, ParityCheck{
				Text:     e.Text(),
				SelStart: uint32(e.call("sel_start")),
				SelEnd:   uint32(e.call("sel_end")),
				Revision: e.Revision(),
				Words:    e.Words(),
				Frame:    hex.EncodeToString(sum[:]),
			})
		default:
			t.Fatalf("unknown op %q", op.Op)
		}
	}
	return out
}
