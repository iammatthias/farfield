package r2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedClock pins the signing timestamp so a signature is reproducible.
var fixedClock = func() time.Time {
	return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
}

// newTestStore points a real Store at a stand-in for the S3 endpoint. The
// signing, the request shaping and the response handling are all the real
// code; only the far side of the wire is substituted.
func newTestStore(t *testing.T, h http.Handler) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := New(Config{
		AccountID:       "acct",
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret",
		Bucket:          "bucket",
		Endpoint:        srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = fixedClock
	return s, srv
}

// TestSignatureIsStable pins the exact Authorization header for a known
// request. The signer has no test of its own anywhere in the fleet, and a
// silent change to it breaks every read and write of every stored byte with
// no local symptom — the failure only shows up against R2.
func TestSignatureIsStable(t *testing.T) {
	var got http.Header
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	if err := s.Put("key.txt", []byte("hello"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if d := got.Get("X-Amz-Date"); d != "20260916T120000Z" {
		t.Errorf("X-Amz-Date = %q", d)
	}
	// SHA-256 of "hello" — the payload hash SigV4 signs over.
	wantHash := hex.EncodeToString(sha256sum([]byte("hello")))
	if h := got.Get("X-Amz-Content-Sha256"); h != wantHash {
		t.Errorf("payload hash = %q, want %q", h, wantHash)
	}

	auth := got.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 ",
		"Credential=AKIAEXAMPLE/20260916/auto/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization missing %q\n  got: %s", want, auth)
		}
	}
	// The signature itself, pinned. Recomputing this by hand is the point: if
	// the canonical request changes shape, this is what catches it.
	const wantSig = "Signature="
	i := strings.Index(auth, wantSig)
	if i < 0 {
		t.Fatalf("no Signature in %q", auth)
	}
	sig := auth[i+len(wantSig):]
	if len(sig) != 64 {
		t.Errorf("signature is %d hex chars, want 64: %q", len(sig), sig)
	}
	if sig != recomputeSignature(t, s, http.MethodPut, "/bucket/key.txt", "", wantHash) {
		t.Errorf("signature does not match an independent derivation: %s", sig)
	}
}

// recomputeSignature derives the expected signature from the documented SigV4
// steps, independently of signWithHash's own string building.
func recomputeSignature(t *testing.T, s *Store, method, path, query, payloadHash string) string {
	t.Helper()
	const amzDate = "20260916T120000Z"
	const dateStamp = "20260916"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		s.host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		method, path, query, canonicalHeaders,
		"host;x-amz-content-sha256;x-amz-date", payloadHash,
	}, "\n")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate,
		dateStamp + "/auto/s3/aws4_request",
		hex.EncodeToString(sum[:]),
	}, "\n")
	key := signingKey(s.cfg.SecretAccessKey, dateStamp, "auto", "s3")
	return hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	objects := map[string][]byte{}
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			b, ok := objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(b)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	if err := s.Put("a.txt", []byte("alpha"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("a.txt")
	if err != nil || string(got) != "alpha" {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// A missing object is (nil, nil) — callers branch on that, not on an error.
	missing, err := s.Get("nope.txt")
	if err != nil || missing != nil {
		t.Fatalf("Get(missing) = %q, %v; want nil, nil", missing, err)
	}

	if err := s.Delete("a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := objects["a.txt"]; ok {
		t.Error("object survived Delete")
	}
}

// TestDeleteToleratesMissing: R2 answers 404 for an object already gone, and
// that is a successful delete, not an error. blobs' hygiene sweep relies on it.
func TestDeleteToleratesMissing(t *testing.T) {
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := s.Delete("gone.txt"); err != nil {
		t.Errorf("Delete of a missing object = %v, want nil", err)
	}
}

func TestPutSeekerHashesWithoutBuffering(t *testing.T) {
	var body []byte
	var sentHash string
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentHash = r.Header.Get("X-Amz-Content-Sha256")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	content := strings.Repeat("payload ", 1000)
	if err := s.PutSeeker("big.bin", strings.NewReader(content), "application/octet-stream"); err != nil {
		t.Fatalf("PutSeeker: %v", err)
	}
	if string(body) != content {
		t.Errorf("body round-tripped wrong: %d bytes, want %d", len(body), len(content))
	}
	want := hex.EncodeToString(sha256sum([]byte(content)))
	if sentHash != want {
		t.Errorf("signed payload hash = %q, want %q", sentHash, want)
	}
}

// TestGetStreamSeeksByRefetching: the store's native stream is not seekable,
// so GetStream wraps it to answer seeks with ranged GETs. Video playback
// depends on this — Safari will not play media unless Range is honored.
func TestGetStreamSeeksByRefetching(t *testing.T) {
	const content = "0123456789abcdef"
	var ranges []string
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		ranges = append(ranges, rng)
		if rng == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(content)))
			w.Write([]byte(content))
			return
		}
		var off int64
		fmt.Sscanf(rng, "bytes=%d-", &off)
		w.Header().Set("Content-Length", fmt.Sprint(int64(len(content))-off))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(content[off:]))
	}))

	rc, size, err := s.GetStream("v.mp4")
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	defer rc.Close()
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}

	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		t.Fatal("a sized stream must be seekable, or ServeContent cannot answer Range")
	}
	if _, err := rs.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	rest, err := io.ReadAll(rs)
	if err != nil {
		t.Fatalf("ReadAll after seek: %v", err)
	}
	if string(rest) != content[10:] {
		t.Errorf("after seek read %q, want %q", rest, content[10:])
	}
	if len(ranges) < 2 || ranges[1] != "bytes=10-" {
		t.Errorf("expected a ranged refetch at offset 10, got %v", ranges)
	}
}

func TestListFollowsPagination(t *testing.T) {
	page := 0
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "list-type=2") {
			t.Errorf("list query = %q, want list-type=2", r.URL.RawQuery)
		}
		page++
		if page == 1 {
			fmt.Fprint(w, `<ListBucketResult>
				<Contents><Key>a</Key><Size>1</Size><LastModified>2026-09-16T00:00:00Z</LastModified></Contents>
				<IsTruncated>true</IsTruncated>
				<NextContinuationToken>tok</NextContinuationToken>
			</ListBucketResult>`)
			return
		}
		if !strings.Contains(r.URL.RawQuery, "continuation-token=tok") {
			t.Errorf("second page did not send the continuation token: %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, `<ListBucketResult>
			<Contents><Key>b</Key><Size>2</Size><LastModified>2026-09-16T01:00:00Z</LastModified></Contents>
			<IsTruncated>false</IsTruncated>
		</ListBucketResult>`)
	}))

	objs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 || objs[0].Key != "a" || objs[1].Key != "b" {
		t.Fatalf("List = %+v, want both pages", objs)
	}
	if objs[1].Size != 2 {
		t.Errorf("size = %d, want 2", objs[1].Size)
	}
	if objs[0].LastModified.IsZero() {
		t.Error("LastModified did not parse")
	}
}

// TestListCanonicalQueryOrder: SigV4 signs the query string as written, and
// the canonical form is key-sorted. continuation-token must precede list-type
// or every paged list past the first fails authentication.
func TestListCanonicalQueryOrder(t *testing.T) {
	var seen string
	s, _ := newTestStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	if _, _, err := s.listPage("tok"); err != nil {
		t.Fatalf("listPage: %v", err)
	}
	if seen != "continuation-token=tok&list-type=2" {
		t.Errorf("query = %q, want continuation-token before list-type", seen)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	for _, missing := range []string{"AccountID", "AccessKeyID", "SecretAccessKey", "Bucket"} {
		cfg := Config{AccountID: "a", AccessKeyID: "b", SecretAccessKey: "c", Bucket: "d"}
		switch missing {
		case "AccountID":
			cfg.AccountID = ""
		case "AccessKeyID":
			cfg.AccessKeyID = ""
		case "SecretAccessKey":
			cfg.SecretAccessKey = ""
		case "Bucket":
			cfg.Bucket = ""
		}
		if _, err := New(cfg); err == nil {
			t.Errorf("New with no %s succeeded, want an error naming the env var", missing)
		}
	}
}

// TestDefaultEndpointDerivesFromAccount: production passes no Endpoint, and
// the host must come out as the account's R2 domain.
func TestDefaultEndpointDerivesFromAccount(t *testing.T) {
	s, err := New(Config{AccountID: "acct123", AccessKeyID: "k",
		SecretAccessKey: "s", Bucket: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := "https://acct123.r2.cloudflarestorage.com/b/key"
	if got := s.objectURL("key"); got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}
