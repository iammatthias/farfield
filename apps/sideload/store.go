package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
)

// objectStore is the durable remote half of the blob store — the subset of
// the R2 client the store needs. An interface so tests can fake it.
type objectStore interface {
	PutFile(key, path, contentType string) error
	Put(key string, data []byte, contentType string) error
	GetStream(key string) (io.ReadCloser, int64, error)
	Delete(key string) error
	List() ([]ObjectInfo, error)
}

// blobStore is the content-addressed store for builds (<cid>.ipa) and
// screenshots (<cid>.<ext>): identical bytes share one key and integrity is
// verifiable by re-hashing. The extension is per-call, so one namespace
// holds both kinds without collision (a CID is unique to its content).
//
// With remote unset (dev), the directory is the whole store. With remote set
// (SIDELOAD_BACKEND=r2), R2 is the durable truth — an .ipa is the one kind
// of farfield content bytes that previously lived on a single disk — and the
// directory becomes a write-through cache: writes land locally and upload
// before being acknowledged, reads come from the cache and refill it from R2
// on a miss. parseIPA and ranged OTA serving always see a local file either
// way, and losing the cache directory costs one re-download per build, not
// the builds.
type blobStore struct {
	dir    string
	remote objectStore // nil = local-only
}

func newBlobStore(dir string, remote objectStore) (*blobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &blobStore{dir: dir, remote: remote}
	s.pruneTemp()
	return s, nil
}

// tempTTL is how long an upload temp file may linger before a startup sweep
// reclaims it. Well past any legitimate in-flight upload.
const tempTTL = 6 * time.Hour

// pruneTemp removes .upload-* temp files a crash stranded. spool cleans up
// after itself on every error path, so anything still here outlived the
// process that created it — and these are .ipa files, so they are large.
func (s *blobStore) pruneTemp() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempTTL)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".upload-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove orphaned upload temp", "path", path, "err", err)
		}
	}
}

// path returns the on-disk location for a content address and extension
// (".ipa", ".png", …).
func (s *blobStore) path(fullCID, ext string) string {
	return filepath.Join(s.dir, fullCID+ext)
}

// spool streams r to a temp file in the store directory while hashing it in the
// same pass, then moves it into place under its content address with the given
// extension. It returns the full CID and byte count. Identical content already
// present is deduped. maxBytes caps the upload; exceeding it is an error so a
// truncated file is never stored. Use it for large uploads (.ipa); small bytes
// (images) can use putBytes.
func (s *blobStore) spool(r io.Reader, maxBytes int64, ext string) (fullCID string, size int64, err error) {
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	// Read one byte past the cap so an over-limit upload is detected, not
	// silently truncated, then hash while spooling to disk.
	limited := io.LimitReader(r, maxBytes+1)
	fullCID, size, hashErr := cid.OfReader(io.TeeReader(limited, tmp))
	if closeErr := tmp.Close(); closeErr != nil && hashErr == nil {
		hashErr = closeErr
	}
	if hashErr != nil {
		return "", 0, hashErr
	}
	if size > maxBytes {
		return "", 0, fmt.Errorf("upload exceeds %d byte limit", maxBytes)
	}
	if size == 0 {
		return "", 0, fmt.Errorf("empty upload")
	}

	final := s.path(fullCID, ext)
	if _, statErr := os.Stat(final); statErr == nil {
		_ = os.Remove(tmpName) // dedupe: identical bytes already stored
		return fullCID, size, nil
	}
	// Durability before acknowledgement: the upload is not accepted until the
	// remote holds it. A failed push fails the whole spool rather than
	// leaving a local-only build that a dead disk would erase.
	if s.remote != nil {
		if err := s.remote.PutFile(fullCID+ext, tmpName, "application/octet-stream"); err != nil {
			return "", 0, fmt.Errorf("store remote copy: %w", err)
		}
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, err
	}
	return fullCID, size, nil
}

// putBytes stores an in-memory payload (a screenshot) under its content address
// and extension, deduping. Returns the full CID.
func (s *blobStore) putBytes(data []byte, ext string) (string, error) {
	fullCID := cid.Of(data)
	final := s.path(fullCID, ext)
	if _, err := os.Stat(final); err == nil {
		return fullCID, nil // already stored
	}
	if s.remote != nil {
		if err := s.remote.Put(fullCID+ext, data, "application/octet-stream"); err != nil {
			return "", fmt.Errorf("store remote copy: %w", err)
		}
	}
	// Temp-then-rename, the same discipline refill uses. Writing straight to
	// the CID path meant a crash mid-write stranded a truncated file at the
	// final name — and because the os.Stat above treats any file at that path
	// as "already stored", every later upload of those exact bytes
	// short-circuits onto the corrupt copy and serves it forever. The store's
	// own invariant is that a blob re-hashes to its name, so a partial write
	// there is permanent damage, not a transient one.
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	return fullCID, nil
}

// open returns a readable handle to a stored blob plus its size, for ranged
// serving via http.ServeContent. A cache miss with a remote configured
// refills the cache first — one whole-object download, after which every
// range request is a local read again.
func (s *blobStore) open(fullCID, ext string) (*os.File, int64, error) {
	f, err := os.Open(s.path(fullCID, ext))
	if os.IsNotExist(err) && s.remote != nil {
		if err := s.refill(fullCID, ext); err != nil {
			return nil, 0, err
		}
		f, err = os.Open(s.path(fullCID, ext))
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// refill downloads a remote object into the cache, via a temp file and a
// rename so a crashed download never leaves a truncated blob under a real
// content address.
func (s *blobStore) refill(fullCID, ext string) error {
	body, _, err := s.remote.GetStream(fullCID + ext)
	if err != nil {
		return err
	}
	if body == nil {
		return os.ErrNotExist
	}
	defer body.Close()
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, err = io.Copy(tmp, body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path(fullCID, ext))
}

// remove deletes a stored blob from the cache and the remote. A missing file
// is not an error.
func (s *blobStore) remove(fullCID, ext string) error {
	err := os.Remove(s.path(fullCID, ext))
	if os.IsNotExist(err) {
		err = nil
	}
	if s.remote != nil {
		if rerr := s.remote.Delete(fullCID + ext); rerr != nil && err == nil {
			err = rerr
		}
	}
	return err
}

// syncRemote uploads every locally-stored blob the remote lacks — the
// one-time migration for a store that predates the R2 backend, and a repair
// for any window when the remote was unreachable. Idempotent.
func (s *blobStore) syncRemote() (uploaded, present int, err error) {
	if s.remote == nil {
		return 0, 0, fmt.Errorf("no remote configured (SIDELOAD_BACKEND != r2)")
	}
	objects, err := s.remote.List()
	if err != nil {
		return 0, 0, err
	}
	have := make(map[string]bool, len(objects))
	for _, o := range objects {
		have[o.Key] = true
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if have[e.Name()] {
			present++
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		if err := s.remote.PutFile(e.Name(), path, "application/octet-stream"); err != nil {
			return uploaded, present, fmt.Errorf("upload %s: %w", e.Name(), err)
		}
		slog.Info("uploaded", "key", e.Name())
		uploaded++
	}
	return uploaded, present, nil
}
