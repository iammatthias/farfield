package bytestore

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *LocalDir {
	t.Helper()
	d, err := OpenLocalDir(t.TempDir())
	if err != nil {
		t.Fatalf("OpenLocalDir: %v", err)
	}
	return d
}

// TestKeyPathRejectsEscapes: keys are flat by design, and these are the shapes
// that would write outside the root if the check ever came out.
func TestKeyPathRejectsEscapes(t *testing.T) {
	d := openTemp(t)
	for _, key := range []string{"", "..", "../etc/passwd", "a/b", `a\b`, "x/../../y"} {
		if err := d.Put(key, []byte("x"), ""); err == nil {
			t.Errorf("Put(%q) succeeded; a key that escapes the root must be refused", key)
		}
		if _, err := d.Get(key); err == nil {
			t.Errorf("Get(%q) succeeded; want refusal", key)
		}
		if err := d.Delete(key); err == nil {
			t.Errorf("Delete(%q) succeeded; want refusal", key)
		}
	}
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	d := openTemp(t)
	if err := d.Put("k", []byte("value"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := d.Get("k")
	if err != nil || string(got) != "value" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := d.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Absent reads as (nil, nil) — callers branch on that, not on an error.
	got, err = d.Get("k")
	if err != nil || got != nil {
		t.Errorf("Get after Delete = %q, %v; want nil, nil", got, err)
	}
	// Deleting what is already gone is not an error.
	if err := d.Delete("k"); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

func TestPutSeekerRewindsBeforeReading(t *testing.T) {
	d := openTemp(t)
	rs := strings.NewReader("full content")
	// Consume the reader first: PutSeeker must rewind, or an upload that was
	// already hashed would store only its tail.
	io.ReadAll(rs)

	if err := d.PutSeeker("k", rs, ""); err != nil {
		t.Fatalf("PutSeeker: %v", err)
	}
	got, _ := d.Get("k")
	if string(got) != "full content" {
		t.Errorf("stored %q, want the whole content — the reader was not rewound", got)
	}
}

func TestPutFileStreamsFromDisk(t *testing.T) {
	d := openTemp(t)
	src := filepath.Join(t.TempDir(), "in.bin")
	content := bytes.Repeat([]byte("abc"), 5000)
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := d.PutFile("k", src, ""); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	got, _ := d.Get("k")
	if !bytes.Equal(got, content) {
		t.Errorf("PutFile stored %d bytes, want %d", len(got), len(content))
	}
}

func TestGetStreamReportsSizeAndAbsence(t *testing.T) {
	d := openTemp(t)
	if err := d.Put("k", []byte("0123456789"), ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, size, err := d.GetStream("k")
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	defer rc.Close()
	if size != 10 {
		t.Errorf("size = %d, want 10", size)
	}
	body, _ := io.ReadAll(rc)
	if string(body) != "0123456789" {
		t.Errorf("stream = %q", body)
	}

	missing, _, err := d.GetStream("nope")
	if err != nil || missing != nil {
		t.Errorf("GetStream(missing) = %v, %v; want nil, nil", missing, err)
	}
}

func TestListIsSortedAndSkipsDirs(t *testing.T) {
	d := openTemp(t)
	for _, k := range []string{"c", "a", "b"} {
		if err := d.Put(k, []byte(k), ""); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}
	// A stray subdirectory must not appear as an object.
	if err := os.Mkdir(filepath.Join(d.root, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	objs, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("List returned %d objects, want 3 (a directory is not an object)", len(objs))
	}
	for i, want := range []string{"a", "b", "c"} {
		if objs[i].Key != want {
			t.Errorf("objs[%d].Key = %q, want %q — List must be sorted", i, objs[i].Key, want)
		}
	}
	if objs[0].Size != 1 || objs[0].LastModified.IsZero() {
		t.Errorf("objs[0] = %+v, want a real size and mtime", objs[0])
	}
}

// LocalDir must satisfy the interface it is the reference implementation of.
var _ Store = (*LocalDir)(nil)
