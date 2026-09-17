// Package r2 is the Cloudflare R2 byte store, spoken over the S3 API and
// signed with AWS Signature V4 by hand — no S3 SDK dependency, in keeping with
// the standard-library-first stack.
//
// blobs, library, and sideload each carried their own copy of this file.
// blobs' and sideload's were byte-identical; library's had forked only in its
// HTTP client, because a multi-minute EPUB transfer cannot live under a
// wall-clock timeout sized for images. That difference is now configuration
// (Config.Client) rather than a third copy of the signing code, so a fix to
// the signer reaches every caller instead of one of three.
package r2

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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/bytestore"
)

// Config configures the byte store. The four credential fields are required;
// the rest have defaults sized for images and small files.
type Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string

	// Client performs ordinary operations. nil gets a 60s overall timeout,
	// which is right for images and wrong for anything that legitimately runs
	// for minutes — library passes a client that bounds connection setup and
	// time-to-first-byte instead, so a dead peer still fails fast without
	// severing a healthy large transfer.
	Client *http.Client

	// Stream serves GetStream bodies. An overall client timeout covers the
	// whole response body, which is fatal for a viewer scrubbing through a
	// long video, so the default bounds only the wait for headers and lets the
	// body take as long as the reader needs.
	Stream *http.Client

	// Endpoint overrides the derived <account>.r2.cloudflarestorage.com host.
	// Tests point it at an httptest server; production leaves it empty.
	Endpoint string
}

// ObjectInfo is bytestore.ObjectInfo — re-exported so a caller holding only
// this package does not need the other import to name a List result.
type ObjectInfo = bytestore.ObjectInfo

// Store is an R2 bucket. It is safe for concurrent use.
type Store struct {
	cfg    Config
	host   string
	scheme string
	client *http.Client
	stream *http.Client

	// now supplies the signing timestamp. nil means time.Now; a test pins it
	// so a signature is reproducible.
	now func() time.Time
}

// New builds a store. It does not contact R2.
func New(cfg Config) (*Store, error) {
	for name, v := range map[string]string{
		"R2_ACCOUNT_ID": cfg.AccountID, "R2_ACCESS_KEY_ID": cfg.AccessKeyID,
		"R2_SECRET_ACCESS_KEY": cfg.SecretAccessKey, "R2_BUCKET": cfg.Bucket,
	} {
		if v == "" {
			return nil, fmt.Errorf("R2 config: %s is required", name)
		}
	}

	s := &Store{
		cfg:    cfg,
		host:   cfg.AccountID + ".r2.cloudflarestorage.com",
		scheme: "https",
		client: cfg.Client,
		stream: cfg.Stream,
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("R2 config: bad Endpoint: %w", err)
		}
		s.host, s.scheme = u.Host, u.Scheme
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 60 * time.Second}
	}
	if s.stream == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second
		s.stream = &http.Client{Transport: tr}
	}
	return s, nil
}

func (s *Store) objectURL(key string) string {
	return s.scheme + "://" + s.host + "/" + s.cfg.Bucket + "/" + key
}

// Put stores bytes already in memory.
func (s *Store) Put(key string, data []byte, contentType string) error {
	req, err := http.NewRequest(http.MethodPut, s.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.doPut(req, key, contentType, hex.EncodeToString(sha256sum(data)))
}

// PutFile streams the file at path without buffering it.
func (s *Store) PutFile(key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.PutSeeker(key, f, contentType)
}

// PutSeeker streams a seekable reader without buffering it. SigV4 signs the
// payload hash, so the content is read twice: one pass to hash, one as the
// request body — both sequential, never a whole-file buffer in memory.
func (s *Store) PutSeeker(key string, rs io.ReadSeeker, contentType string) error {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	size, err := io.Copy(h, rs)
	if err != nil {
		return err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, s.objectURL(key), rs)
	if err != nil {
		return err
	}
	req.ContentLength = size
	return s.doPut(req, key, contentType, hex.EncodeToString(h.Sum(nil)))
}

// doPut signs and sends a prepared PUT, reporting a non-200 as an error.
func (s *Store) doPut(req *http.Request, key, contentType, payloadHash string) error {
	if contentType != "" {
		req.Header.Set("Content-Type", contentType) // sent unsigned — allowed
	}
	s.signWithHash(req, payloadHash)
	resp, err := s.client.Do(req)
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

// Get reads an object whole. A missing object is (nil, nil), not an error.
func (s *Store) Get(key string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	s.sign(req, nil)
	resp, err := s.client.Do(req)
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

// GetStream returns the object's body as a stream. GET signs an empty payload,
// so streaming the response is compatible with SigV4. When the size is known
// (it always is, short of a proxy mangling the response) the stream seeks by
// refetching with a Range header, so a byte handler can answer 206s — video
// seeking over R2.
func (s *Store) GetStream(key string) (io.ReadCloser, int64, error) {
	body, size, err := s.getRange(key, 0)
	if err != nil || body == nil {
		return nil, 0, err
	}
	if size < 0 {
		return body, size, nil // unknown length — plain stream, no seeking
	}
	rr := &rangeReader{
		size: size,
		body: body,
		fetch: func(off int64) (io.ReadCloser, error) {
			b, _, err := s.getRange(key, off)
			if err == nil && b == nil {
				err = fmt.Errorf("R2 get %s: object vanished mid-read", key)
			}
			return b, err
		},
	}
	return rr, size, nil
}

// getRange GETs the object from byte offset off, returning the body and the
// response's Content-Length ((nil, 0, nil) when absent). Range is not in the
// SigV4 signed set, so adding it does not disturb the signature; should a
// middlebox strip it, the ignored prefix is discarded to keep the caller's
// offset honest.
func (s *Store) getRange(key string, off int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest(http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, 0, err
	}
	if off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
	}
	s.sign(req, nil)
	resp, err := s.stream.Do(req)
	if err != nil {
		return nil, 0, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, 0, nil
	case http.StatusPartialContent:
		return resp.Body, resp.ContentLength, nil
	case http.StatusOK:
		if off > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
				resp.Body.Close()
				return nil, 0, err
			}
		}
		return resp.Body, resp.ContentLength, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, 0, fmt.Errorf("R2 get %s: HTTP %d: %s", key, resp.StatusCode, body)
	}
}

// Delete removes an object. Already gone counts as deleted.
func (s *Store) Delete(key string) error {
	req, err := http.NewRequest(http.MethodDelete, s.objectURL(key), nil)
	if err != nil {
		return err
	}
	s.sign(req, nil)
	resp, err := s.client.Do(req)
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
func (s *Store) List() ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""
	for {
		page, next, err := s.listPage(token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

func (s *Store) listPage(continuationToken string) ([]ObjectInfo, string, error) {
	// SigV4 needs the query string in canonical (key-sorted) order:
	// continuation-token sorts before list-type.
	query := "list-type=2"
	if continuationToken != "" {
		query = "continuation-token=" + url.QueryEscape(continuationToken) + "&" + query
	}
	req, err := http.NewRequest(http.MethodGet,
		s.scheme+"://"+s.host+"/"+s.cfg.Bucket+"?"+query, nil)
	if err != nil {
		return nil, "", err
	}
	s.sign(req, nil)
	resp, err := s.client.Do(req)
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
func (s *Store) sign(req *http.Request, payload []byte) {
	s.signWithHash(req, hex.EncodeToString(sha256sum(payload)))
}

// signWithHash signs with a precomputed hex SHA-256 payload hash — SigV4 needs
// only the hash, so callers can stream bodies they never buffer.
func (s *Store) signWithHash(req *http.Request, payloadHash string) {
	clock := s.now
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
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

	key := signingKey(s.cfg.SecretAccessKey, dateStamp, "auto", "s3")
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+s.cfg.AccessKeyID+"/"+scope+", "+
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
