package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iammatthias/farfield/lib/cid"
)

// blobStore is the on-disk content-addressed store for .ipa files. Each build
// lives at <dir>/<cid>.ipa, so identical bytes share one file and a build's
// integrity is verifiable by re-hashing.
type blobStore struct {
	dir string
}

func newBlobStore(dir string) (*blobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &blobStore{dir: dir}, nil
}

// path returns the on-disk location for a build's full CID.
func (s *blobStore) path(fullCID string) string {
	return filepath.Join(s.dir, fullCID+".ipa")
}

// spool streams r to a temp file in the store directory while hashing it in the
// same pass, then moves it into place under its content address. It returns the
// full CID and byte count. Identical content already present is deduped: the
// temp file is discarded and the existing blob kept. maxBytes caps the upload;
// exceeding it is an error so a truncated .ipa is never stored.
func (s *blobStore) spool(r io.Reader, maxBytes int64) (fullCID string, size int64, err error) {
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

	final := s.path(fullCID)
	if _, statErr := os.Stat(final); statErr == nil {
		_ = os.Remove(tmpName) // dedupe: identical bytes already stored
		return fullCID, size, nil
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, err
	}
	return fullCID, size, nil
}

// open returns a readable handle to a stored blob plus its size, for ranged
// serving via http.ServeContent.
func (s *blobStore) open(fullCID string) (*os.File, int64, error) {
	f, err := os.Open(s.path(fullCID))
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
func (s *blobStore) remove(fullCID string) error {
	err := os.Remove(s.path(fullCID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
