package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// R2Config configures the Cloudflare R2 (S3-compatible) byte store.
type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// R2 is a ByteStore backed by Cloudflare R2 over the S3 API. Requests are
// signed with AWS Signature V4 — hand-rolled, no S3 SDK dependency.
type R2 struct {
	cfg    R2Config
	host   string
	client *http.Client
}

// NewR2 builds an R2 byte store. It does not contact R2.
func NewR2(cfg R2Config) (*R2, error) {
	for name, v := range map[string]string{
		"R2_ACCOUNT_ID": cfg.AccountID, "R2_ACCESS_KEY_ID": cfg.AccessKeyID,
		"R2_SECRET_ACCESS_KEY": cfg.SecretAccessKey, "R2_BUCKET": cfg.Bucket,
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

func (r *R2) objectURL(key string) string {
	return "https://" + r.host + "/" + r.cfg.Bucket + "/" + key
}

func (r *R2) Put(key string, data []byte, contentType string) error {
	req, err := http.NewRequest(http.MethodPut, r.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType) // sent unsigned — allowed
	}
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

// PutFile streams the file at path to R2 without buffering it. SigV4 signs
// the payload hash, so the file is read twice: one pass to hash, one as the
// request body — both sequential disk reads, never a whole-file buffer.
func (r *R2) PutFile(key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, r.objectURL(key), f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType) // sent unsigned — allowed
	}
	r.signWithHash(req, hex.EncodeToString(h.Sum(nil)))
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

func (r *R2) Get(key string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, r.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("R2 get %s: HTTP %d: %s", key, resp.StatusCode, body)
	}
	return body, nil
}

// GetStream returns the object's body as a stream. GET requests sign an
// empty payload, so streaming the response is compatible with SigV4.
func (r *R2) GetStream(key string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest(http.MethodGet, r.objectURL(key), nil)
	if err != nil {
		return nil, 0, err
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, 0, fmt.Errorf("R2 get %s: HTTP %d: %s", key, resp.StatusCode, body)
	}
	return resp.Body, resp.ContentLength, nil
}

func (r *R2) Delete(key string) error {
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
	return nil
}

// ── AWS Signature V4 ────────────────────────────────────────────────────────

// sign adds an AWS SigV4 Authorization header for the S3 service. payload is
// nil for bodyless requests (GET/HEAD/DELETE).
func (r *R2) sign(req *http.Request, payload []byte) {
	r.signWithHash(req, hex.EncodeToString(sha256sum(payload)))
}

// signWithHash signs with a precomputed hex SHA-256 payload hash — SigV4
// needs only the hash, so callers can stream bodies they never buffer.
func (r *R2) signWithHash(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery, // built in canonical (sorted, encoded) form
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
