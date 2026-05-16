package blob

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// R2Config configures the Cloudflare R2 (S3-compatible) blob backend.
type R2Config struct {
	AccountID       string // Cloudflare account ID
	AccessKeyID     string // R2 S3 access key id
	SecretAccessKey string // R2 S3 secret access key
	Bucket          string // bucket name
}

// R2 is a blob.Store backed by Cloudflare R2 over the S3 API. Requests are
// signed with AWS Signature V4 — hand-rolled, no S3 SDK dependency.
//
// Each blob is two objects: `<cid>` (the bytes, with its Content-Type) and
// `<cid>.json` (the metadata sidecar) — mirroring the LocalDir backend.
type R2 struct {
	cfg    R2Config
	host   string
	client *http.Client
}

// NewR2 builds an R2 backend. It does not contact R2.
func NewR2(cfg R2Config) (*R2, error) {
	for name, v := range map[string]string{
		"account id": cfg.AccountID, "access key id": cfg.AccessKeyID,
		"secret access key": cfg.SecretAccessKey, "bucket": cfg.Bucket,
	} {
		if v == "" {
			return nil, fmt.Errorf("R2 config: %s is required", name)
		}
	}
	return &R2{
		cfg:    cfg,
		host:   cfg.AccountID + ".r2.cloudflarestorage.com",
		client: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (r *R2) Put(meta Meta, data []byte) error {
	if err := r.putObject(meta.CID, data, meta.Mime); err != nil {
		return err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return r.putObject(meta.CID+".json", metaJSON, "application/json")
}

func (r *R2) GetBytes(cid string) ([]byte, error) {
	data, found, err := r.getObject(cid)
	if err != nil || !found {
		return nil, err
	}
	return data, nil
}

func (r *R2) GetMeta(cid string) (*Meta, error) {
	data, found, err := r.getObject(cid + ".json")
	if err != nil || !found {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *R2) Exists(cid string) bool {
	req, err := http.NewRequest(http.MethodHead, r.objectURL(cid), nil)
	if err != nil {
		return false
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (r *R2) Delete(cid string) error {
	for _, key := range []string{cid, cid + ".json"} {
		req, err := http.NewRequest(http.MethodDelete, r.objectURL(key), nil)
		if err != nil {
			return err
		}
		r.sign(req, nil)
		resp, err := r.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		// R2 returns 204 on delete, 404 if already gone — both are fine.
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("R2 delete %s: HTTP %d", key, resp.StatusCode)
		}
	}
	return nil
}

// List returns every blob CID. v1 reads a single ListObjectsV2 page (up to
// 1000 keys) — ample for a personal site; pagination is a later refinement.
func (r *R2) List() ([]string, error) {
	u := "https://" + r.host + "/" + r.cfg.Bucket + "?list-type=2"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("R2 list: HTTP %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("R2 list: parsing XML: %w", err)
	}
	var cids []string
	for _, c := range result.Contents {
		// The bytes object is the bare CID; skip the .json sidecars.
		if !strings.HasSuffix(c.Key, ".json") {
			cids = append(cids, c.Key)
		}
	}
	return cids, nil
}

// ---------- object operations ----------------------------------------------

func (r *R2) objectURL(key string) string {
	return "https://" + r.host + "/" + r.cfg.Bucket + "/" + key
}

func (r *R2) putObject(key string, data []byte, contentType string) error {
	req, err := http.NewRequest(http.MethodPut, r.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType) // sent unsigned — allowed
	r.sign(req, data)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("R2 put %s: HTTP %d: %s", key, resp.StatusCode, body)
	}
	return nil
}

func (r *R2) getObject(key string) (data []byte, found bool, err error) {
	req, err := http.NewRequest(http.MethodGet, r.objectURL(key), nil)
	if err != nil {
		return nil, false, err
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("R2 get %s: HTTP %d: %s", key, resp.StatusCode, body)
	}
	return body, true, nil
}

// ---------- AWS Signature V4 ------------------------------------------------

// sign adds an AWS SigV4 Authorization header for the S3 service. payload is
// nil for bodyless requests (GET/HEAD/DELETE).
func (r *R2) sign(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := hex.EncodeToString(sha256sum(payload))

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery, // our queries (list-type=2) are already canonical
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/auto/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")

	key := signingKey(r.cfg.SecretAccessKey, dateStamp, "auto", "s3")
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+r.cfg.AccessKeyID+"/"+scope+", "+
		"SignedHeaders="+signedHeaders+", "+
		"Signature="+signature)
}

// signingKey derives the SigV4 signing key by the documented HMAC chain.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}
