package main

import "time"

// ObjectInfo describes one stored object — the same shape the blobs service
// uses; r2.go and rangereader.go are per-app copies of its R2 client.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}
