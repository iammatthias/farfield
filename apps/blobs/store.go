package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ObjectInfo describes one object in a ByteStore — used by migration tooling.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ByteStore stores raw blob bytes, keyed by string. Metadata lives in SQLite,
// not here. The backend is a local directory (dev) or Cloudflare R2 (server).
type ByteStore interface {
	Put(key string, data []byte, contentType string) error
	Get(key string) ([]byte, error) // returns (nil, nil) when absent
	Delete(key string) error
	List() ([]ObjectInfo, error)
}

// LocalDir is a ByteStore backed by a directory on disk.
type LocalDir struct{ root string }

// OpenLocalDir opens (creating if absent) a blob directory at root.
func OpenLocalDir(root string) (*LocalDir, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalDir{root: root}, nil
}

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
	var out []ObjectInfo
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
