package r2

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxPresignTTL is SigV4's ceiling on X-Amz-Expires: seven days.
const maxPresignTTL = 7 * 24 * time.Hour

// PresignGet returns a SigV4 query-signed URL that lets anyone holding it GET
// the object at key — and nothing else — until ttl elapses. The bucket stays
// private: the signature binds the method, the host, the exact key, and every
// parameter in params (response-content-type, response-content-disposition),
// so a holder can neither reach another object nor change the headers R2
// answers with. S3 checks expiry when a request starts, so a transfer begun in
// time runs to completion.
//
// The URL is a bearer credential for its lifetime: mint one per request, after
// the caller is authorized, and never log or cache it.
func (s *Store) PresignGet(key string, ttl time.Duration, params url.Values) (string, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", fmt.Errorf("R2 presign: ttl %v outside (0, %v]", ttl, maxPresignTTL)
	}
	clock := s.now
	if clock == nil {
		clock = time.Now
	}
	return presignGet(s.scheme, s.host, "/"+s.cfg.Bucket+"/"+key, "auto",
		s.cfg.AccessKeyID, s.cfg.SecretAccessKey, clock().UTC(), ttl, params), nil
}

// presignGet builds the query-signed GET URL. It is split from PresignGet so a
// test can check it against the AWS documentation's worked example, which
// uses a region and host R2 never does.
func presignGet(scheme, host, path, region, accessKey, secret string,
	now time.Time, ttl time.Duration, params url.Values) string {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + region + "/s3/aws4_request"

	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(ttl/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	query := canonicalQuery(q)
	escPath := uriEncode(path, false)

	// A presigned URL cannot know the payload a client will send, so it signs
	// the literal UNSIGNED-PAYLOAD; host is the only signed header, which is
	// what lets a plain browser or URLSession GET replay it.
	canonicalRequest := strings.Join([]string{
		"GET", escPath, query, "host:" + host + "\n", "host", "UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(secret, dateStamp, region, "s3"), []byte(stringToSign)))

	return scheme + "://" + host + escPath + "?" + query + "&X-Amz-Signature=" + sig
}

// canonicalQuery renders q in SigV4 canonical form: every name and value
// URI-encoded, pairs sorted by name then value, joined by '&'. url.Values'
// own Encode is not it — it writes a space as '+', which SigV4 rejects.
func canonicalQuery(q url.Values) string {
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode is SigV4's URI encoding: the RFC 3986 unreserved characters pass
// through, everything else becomes uppercase %XX. A path keeps its '/'
// separators; a query component encodes them too.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
