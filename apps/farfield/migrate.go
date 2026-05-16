package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/iammatthias/farfield/lib/core"
	"github.com/iammatthias/farfield/lib/schema"
)

// cmdMigrateImages lifts every ipfs:// image off IPFS: fetch from a gateway,
// upload to the blob service, create a media record, and rewrite the body to
// blob://<cid>.
func cmdMigrateImages(args []string) error {
	fs := flag.NewFlagSet("migrate-images", flag.ExitOnError)
	content := fs.String("content", defaultService, "content service URL")
	blobs := fs.String("blobs", defaultBlobs, "blob service URL")
	gateway := fs.String("gateway", "https://ipfs.io/ipfs/", "IPFS gateway base (pass your Pinata gateway for reliability)")
	schemaDir := fs.String("schemas", defaultSchemas, "schema directory")
	dryRun := fs.Bool("dry-run", false, "fetch and report, but upload and rewrite nothing")
	_ = fs.Parse(args)

	set, err := schema.Load(*schemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas: %w", err)
	}
	tok := token()
	cache := map[string]string{} // ipfs CID -> blob CID
	var images, failed, rewritten int

	for _, col := range set.Collections() {
		if col.Name == "media" { // media records carry no markdown body
			continue
		}
		records, err := listRecords(*content, col.Name)
		if err != nil {
			return err
		}
		for _, rec := range records {
			value, _ := rec["value"].(map[string]any)
			rkey, _ := rec["rkey"].(string)
			body, _ := value["body"].(string)
			refs := scanIPFS(body)
			if len(refs) == 0 {
				continue
			}
			newBody := body
			count := 0
			for _, ipfsCID := range refs {
				blobCID, ok := cache[ipfsCID]
				if !ok {
					bc, err := ensureBlob(*gateway, *blobs, *content, tok, ipfsCID, *dryRun)
					if err != nil {
						fmt.Printf("fail  image ipfs://%s — %v\n", ipfsCID, err)
						failed++
						continue
					}
					images++
					cache[ipfsCID] = bc
					blobCID = bc
				}
				newBody = strings.ReplaceAll(newBody, "ipfs://"+ipfsCID, "blob://"+blobCID)
				count++
			}
			if newBody == body {
				continue
			}
			if *dryRun {
				fmt.Printf("ok    %s/%s — would rewrite %d ref(s)\n", col.Name, rkey, count)
			} else {
				value["body"] = newBody
				if _, err := send(*content, col.Name, rkey, value, tok); err != nil {
					fmt.Printf("fail  %s/%s — %v\n", col.Name, rkey, err)
					failed++
					continue
				}
				fmt.Printf("ok    %s/%s — rewrote %d ref(s)\n", col.Name, rkey, count)
			}
			rewritten++
		}
	}

	verb := "done"
	if *dryRun {
		verb = "dry-run"
	}
	fmt.Printf("\n%s: %d image(s) migrated, %d record(s) rewritten, %d failed\n",
		verb, images, rewritten, failed)
	if failed > 0 {
		return fmt.Errorf("%d failure(s)", failed)
	}
	return nil
}

// ensureBlob fetches an ipfs:// image, stores it as a Farfield blob + media
// record, and returns the blob CID. In dry-run it only fetches and hashes.
func ensureBlob(gateway, blobs, content, tok, ipfsCID string, dryRun bool) (string, error) {
	resp, err := httpClient.Get(gateway + ipfsCID)
	if err != nil {
		return "", fmt.Errorf("gateway fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading image bytes: %w", err)
	}
	if dryRun {
		return core.BlobCID(data), nil
	}
	cid, _, err := storeMedia(blobs, content, tok, data)
	return cid, err
}

// scanIPFS returns every distinct ipfs://<cid> reference in a body, in order.
func scanIPFS(body string) []string {
	var found []string
	seen := map[string]bool{}
	rest := body
	for {
		idx := strings.Index(rest, "ipfs://")
		if idx < 0 {
			break
		}
		after := rest[idx+len("ipfs://"):]
		end := 0
		for end < len(after) {
			c := after[end]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				end++
			} else {
				break
			}
		}
		cid := after[:end]
		if cid != "" && !seen[cid] {
			seen[cid] = true
			found = append(found, cid)
		}
		rest = after[end:]
	}
	return found
}

// listRecords GETs a collection's records.
func listRecords(content, collection string) ([]map[string]any, error) {
	resp, err := httpClient.Get(fmt.Sprintf("%s/records/%s", content, collection))
	if err != nil {
		return nil, fmt.Errorf("content service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("listing %s: HTTP %d", collection, resp.StatusCode)
	}
	var rb struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rb); err != nil {
		return nil, fmt.Errorf("parsing record list: %w", err)
	}
	return rb.Records, nil
}
