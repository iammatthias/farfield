// Package bytestore is the byte-storage contract the fleet's media services
// share, plus the local-directory backend that satisfies it in development.
//
// Metadata lives in SQLite; this holds only bytes, keyed by a string the
// caller chooses (blobs and library both key by CID). The other backend is
// lib/r2, which is what runs on the server.
//
// blobs and library each declared this interface and this LocalDir
// separately, with each one carrying a method the other's copy lacked —
// blobs had List, library had PutSeeker, and neither could use the other's.
// One interface, both methods, one implementation.
package bytestore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ObjectInfo describes one stored object, as List reports it.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store holds raw bytes keyed by string.
type Store interface {
	Put(key string, data []byte, contentType string) error
	// PutFile streams the file at path into the store — for payloads too large
	// to hold in memory (media, books, backup snapshots).
	PutFile(key, path, contentType string) error
	// PutSeeker streams an already-open, seekable source. It is how an upload
	// reaches storage without ever being buffered: a multipart temp file and a
	// tus staging file are both seekable.
	PutSeeker(key string, rs io.ReadSeeker, contentType string) error
	Get(key string) ([]byte, error) // (nil, nil) when absent
	// GetStream returns the object's bytes as a stream plus its size, or
	// (nil, 0, nil) when absent. The caller closes the reader.
	GetStream(key string) (io.ReadCloser, int64, error)
	Delete(key string) error
	// List enumerates the store — used by migration and reconciliation tooling.
	List() ([]ObjectInfo, error)
}

// LocalDir is a Store backed by a directory on disk.
type LocalDir struct{ root string }

// OpenLocalDir opens (creating if absent) an object directory at root.
func OpenLocalDir(root string) (*LocalDir, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalDir{root: root}, nil
}

// keyPath rejects any key that could escape the root. Keys are flat by
// design: a separator or a parent reference is a caller bug, not a subpath.
func (d *LocalDir) keyPath(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return "", errors.New("invalid object key")
	}
	return filepath.Join(d.root, key), nil
}

func (d *LocalDir) Put(key string, data []byte, _ string) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (d *LocalDir) PutFile(key, path, contentType string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	return d.PutSeeker(key, src, contentType)
}

func (d *LocalDir) PutSeeker(key string, rs io.ReadSeeker, _ string) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, rs); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func (d *LocalDir) Get(key string) ([]byte, error) {
	p, err := d.keyPath(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (d *LocalDir) GetStream(key string) (io.ReadCloser, int64, error) {
	p, err := d.keyPath(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
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

func (d *LocalDir) Delete(key string) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *LocalDir) List() ([]ObjectInfo, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, ObjectInfo{
			Key:          e.Name(),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
