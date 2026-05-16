package blob

import (
	"encoding/hex"
	"testing"
)

// TestSigningKeyKnownAnswer checks the SigV4 HMAC chain — the most
// error-prone part — against a known answer. The expected value was computed
// independently with Python's hmac/hashlib for the standard AWS example
// inputs (secret/date/region/service); this pins the Go derivation to it.
func TestSigningKeyKnownAnswer(t *testing.T) {
	got := hex.EncodeToString(signingKey(
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"20150830", "us-east-1", "iam",
	))
	want := "2c94c0cf5378ada6887f09bb697df8fc0affdb34ba1cdd5bda32b664bd55b73c"
	if got != want {
		t.Fatalf("signing key mismatch:\n  got  %s\n  want %s", got, want)
	}
}

func TestNewR2RequiresFullConfig(t *testing.T) {
	if _, err := NewR2(R2Config{AccountID: "acct", Bucket: "b"}); err == nil {
		t.Fatal("expected an error for missing credentials")
	}
	full := R2Config{
		AccountID: "acct", AccessKeyID: "ak",
		SecretAccessKey: "sk", Bucket: "farfield-assets",
	}
	r, err := NewR2(full)
	if err != nil {
		t.Fatalf("a full config should build: %v", err)
	}
	if r.host != "acct.r2.cloudflarestorage.com" {
		t.Fatalf("unexpected endpoint host: %s", r.host)
	}
}

// R2 satisfies the Store interface.
var _ Store = (*R2)(nil)
