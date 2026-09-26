package r2

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPresignGetMatchesAWSExample checks the signer against the worked example
// in the AWS SigV4 documentation ("Authenticating Requests: Using Query
// Parameters"), so the canonical form is right and not merely self-consistent.
func TestPresignGetMatchesAWSExample(t *testing.T) {
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := presignGet("https", "examplebucket.s3.amazonaws.com", "/test.txt", "us-east-1",
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now, 86400*time.Second, nil)
	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "&X-Amz-Signature="+wantSig) {
		t.Errorf("presigned URL signature mismatch\n got %s\nwant signature %s", got, wantSig)
	}
}

func TestPresignGetBindsParams(t *testing.T) {
	s, err := New(Config{AccountID: "acct", AccessKeyID: "AK", SecretAccessKey: "SK", Bucket: "books"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 9, 26, 18, 0, 0, 0, time.UTC) }

	params := url.Values{
		"response-content-type":        {"application/epub+zip"},
		"response-content-disposition": {`attachment; filename="A Book.epub"`},
	}
	raw, err := s.PresignGet("bafkreiexample", 10*time.Minute, params)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "acct.r2.cloudflarestorage.com" || u.Path != "/books/bafkreiexample" {
		t.Errorf("presigned URL targets %s%s", u.Host, u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"X-Amz-Expires":                "600",
		"X-Amz-SignedHeaders":          "host",
		"X-Amz-Credential":             "AK/20260926/auto/s3/aws4_request",
		"response-content-type":        "application/epub+zip",
		"response-content-disposition": `attachment; filename="A Book.epub"`,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// SigV4 wants %20, never '+', for a space.
	if strings.Contains(u.RawQuery, "+") {
		t.Errorf("query encodes a space as '+': %s", u.RawQuery)
	}

	// Changing any signed parameter must change the signature.
	params.Set("response-content-type", "text/html")
	other, _ := s.PresignGet("bafkreiexample", 10*time.Minute, params)
	if sigOf(raw) == sigOf(other) {
		t.Error("signature does not cover response-content-type")
	}

	for _, bad := range []time.Duration{0, -time.Second, 8 * 24 * time.Hour} {
		if _, err := s.PresignGet("k", bad, nil); err == nil {
			t.Errorf("ttl %v accepted", bad)
		}
	}
}

func sigOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Query().Get("X-Amz-Signature")
}
