package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// List returns every object in the bucket, following ListObjectsV2 pagination.
func (r *R2) List() ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""
	for {
		page, next, err := r.listPage(token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		token = next
	}
	return out, nil
}

func (r *R2) listPage(continuationToken string) ([]ObjectInfo, string, error) {
	// SigV4 needs the query string in canonical (key-sorted) order:
	// continuation-token sorts before list-type.
	query := "list-type=2"
	if continuationToken != "" {
		query = "continuation-token=" + url.QueryEscape(continuationToken) + "&" + query
	}
	req, err := http.NewRequest(http.MethodGet,
		"https://"+r.host+"/"+r.cfg.Bucket+"?"+query, nil)
	if err != nil {
		return nil, "", err
	}
	r.sign(req, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("R2 list: HTTP %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Contents []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
	}
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, "", fmt.Errorf("R2 list: parsing XML: %w", err)
	}
	page := make([]ObjectInfo, 0, len(result.Contents))
	for _, c := range result.Contents {
		lm, _ := time.Parse(time.RFC3339, c.LastModified)
		page = append(page, ObjectInfo{Key: c.Key, Size: c.Size, LastModified: lm})
	}
	if result.IsTruncated {
		return page, result.NextContinuationToken, nil
	}
	return page, "", nil
}

// ── AWS Signature V4 ────────────────────────────────────────────────────────

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
