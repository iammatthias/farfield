package main

import (
	"fmt"
	"os"

	"github.com/iammatthias/farfield/lib/store"
)

// openBlobStore builds the blob store from the environment. SIDELOAD_DIR is
// always the local directory; SIDELOAD_BACKEND=r2 additionally makes the
// shared R2 bucket the durable truth (same credentials every R2-backed app
// uses), demoting the directory to a write-through cache.
func openBlobStore() (*blobStore, error) {
	dir := store.Env("SIDELOAD_DIR", "sideload-blobs")
	switch store.Env("SIDELOAD_BACKEND", "local") {
	case "local":
		return newBlobStore(dir, nil)
	case "r2":
		r2, err := NewR2(R2Config{
			AccountID:       os.Getenv("R2_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
			Bucket:          os.Getenv("R2_BUCKET"),
		})
		if err != nil {
			return nil, err
		}
		return newBlobStore(dir, r2)
	default:
		return nil, fmt.Errorf(`SIDELOAD_BACKEND must be "local" or "r2"`)
	}
}

// runSyncStore is the `sideload sync-store` CLI command — the one-time
// migration (and any-time repair) that uploads every locally-held blob the
// remote lacks.
func runSyncStore() error {
	bs, err := openBlobStore()
	if err != nil {
		return err
	}
	uploaded, present, err := bs.syncRemote()
	if err != nil {
		return err
	}
	fmt.Printf("sync-store: %d uploaded, %d already present\n", uploaded, present)
	return nil
}
