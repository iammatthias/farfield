package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRemote is a map-backed objectStore.
type fakeRemote struct {
	objects map[string][]byte
	fail    bool // every write errors — the unreachable-R2 case
}

func newFakeRemote() *fakeRemote { return &fakeRemote{objects: map[string][]byte{}} }

func (f *fakeRemote) PutFile(key, path, _ string) error {
	if f.fail {
		return errors.New("remote unavailable")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f.objects[key] = b
	return nil
}

func (f *fakeRemote) Put(key string, data []byte, _ string) error {
	if f.fail {
		return errors.New("remote unavailable")
	}
	f.objects[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeRemote) GetStream(key string) (io.ReadCloser, int64, error) {
	b, ok := f.objects[key]
	if !ok {
		return nil, 0, nil
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeRemote) Delete(key string) error {
	delete(f.objects, key)
	return nil
}

func (f *fakeRemote) List() ([]ObjectInfo, error) {
	var out []ObjectInfo
	for k, v := range f.objects {
		out = append(out, ObjectInfo{Key: k, Size: int64(len(v)), LastModified: time.Now()})
	}
	return out, nil
}

// TestRemoteStoreContract pins the R2-backed behavior: a spool is not
// acknowledged until the remote holds the bytes; an unreachable remote fails
// the upload without storing locally; a cache miss refills from the remote;
// remove clears both sides; syncRemote uploads exactly what the remote lacks.
func TestRemoteStoreContract(t *testing.T) {
	dir := t.TempDir()
	remote := newFakeRemote()
	bs, err := newBlobStore(dir, remote)
	if err != nil {
		t.Fatal(err)
	}

	// spool: local + remote in one acknowledged write.
	cidStr, size, err := bs.spool(strings.NewReader("ipa bytes"), 1<<20, ".ipa")
	if err != nil || size != 9 {
		t.Fatalf("spool: cid=%q size=%d err=%v", cidStr, size, err)
	}
	if _, ok := remote.objects[cidStr+".ipa"]; !ok {
		t.Fatal("spool acknowledged without the remote holding the bytes")
	}

	// Unreachable remote: the upload must fail and leave no local file.
	remote.fail = true
	if _, _, err := bs.spool(strings.NewReader("other bytes"), 1<<20, ".ipa"); err == nil {
		t.Fatal("spool succeeded with the remote down — a local-only build")
	}
	remote.fail = false

	// Cache miss: deleting the local file only, open refills from remote.
	if err := os.Remove(filepath.Join(dir, cidStr+".ipa")); err != nil {
		t.Fatal(err)
	}
	f, n, err := bs.open(cidStr, ".ipa")
	if err != nil || n != 9 {
		t.Fatalf("open after cache loss: n=%d err=%v", n, err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "ipa bytes" {
		t.Fatalf("refilled bytes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, cidStr+".ipa")); err != nil {
		t.Fatal("refill did not repopulate the cache")
	}

	// putBytes: screenshots take the same dual write.
	shot, err := bs.putBytes([]byte("png bytes"), ".png")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.objects[shot+".png"]; !ok {
		t.Fatal("putBytes skipped the remote")
	}

	// remove: both sides clear.
	if err := bs.remove(cidStr, ".ipa"); err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.objects[cidStr+".ipa"]; ok {
		t.Fatal("remove left the remote copy")
	}

	// syncRemote: a pre-existing local file the remote lacks gets uploaded;
	// temp files are skipped.
	if err := os.WriteFile(filepath.Join(dir, "legacy.ipa"), []byte("old build"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".upload-junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploaded, present, err := bs.syncRemote()
	if err != nil {
		t.Fatal(err)
	}
	if uploaded != 1 || present != 1 { // legacy.ipa uploaded; shot.png already present
		t.Fatalf("syncRemote = %d uploaded, %d present; want 1, 1", uploaded, present)
	}
	if _, ok := remote.objects["legacy.ipa"]; !ok {
		t.Fatal("syncRemote missed the legacy build")
	}
}
