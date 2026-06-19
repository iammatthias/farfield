package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iammatthias/farfield/lib/cid"
)

// blobStore is the on-disk content-addressed store. Builds live at
// <dir>/<cid>.ipa and screenshots at <dir>/<cid>.<ext>, so identical bytes
// share one file and integrity is verifiable by re-hashing. The extension is
// per-call, so one directory holds both kinds without collision (a CID is
// unique to its content).
type blobStore struct {
	dir string
}

func newBlobStore(dir string) (*blobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &blobStore{dir: dir}, nil
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
	if err := os.WriteFile(final, data, 0o644); err != nil {
		return "", err
	}
	return fullCID, nil
}

// open returns a readable handle to a stored blob plus its size, for ranged
// serving via http.ServeContent.
func (s *blobStore) open(fullCID, ext string) (*os.File, int64, error) {
	f, err := os.Open(s.path(fullCID, ext))
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

// remove deletes a stored blob. A missing file is not an error.
func (s *blobStore) remove(fullCID, ext string) error {
	err := os.Remove(s.path(fullCID, ext))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
