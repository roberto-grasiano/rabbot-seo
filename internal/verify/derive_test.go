package verify

import (
	"strings"
	"testing"
)

func TestDeriveToken_DeterministicAndInstanceUnique(t *testing.T) {
	keyA := make([]byte, instanceKeyBytes) // all-zero key A
	keyB := make([]byte, instanceKeyBytes)
	keyB[0] = 1 // different key B

	a1 := DeriveToken(keyA, "example.com")
	a2 := DeriveToken(keyA, "example.com")
	if a1 != a2 {
		t.Fatalf("derivation not deterministic: %q != %q", a1, a2)
	}
	if !strings.HasPrefix(a1, TokenPrefix) {
		t.Fatalf("token %q missing prefix %q", a1, TokenPrefix)
	}

	// Different key ⇒ different token (instance-unique).
	if b := DeriveToken(keyB, "example.com"); b == a1 {
		t.Fatalf("different keys produced the same token %q", a1)
	}
	// Different host ⇒ different token.
	if other := DeriveToken(keyA, "other.com"); other == a1 {
		t.Fatalf("different hosts produced the same token %q", a1)
	}
}

func TestCanonicalHost(t *testing.T) {
	cases := map[string]string{
		"Example.com":      "example.com",
		"example.com.":     "example.com",
		"  example.com  ":  "example.com",
		"example.com:443":  "example.com",      // default https port dropped
		"example.com:80":   "example.com",      // default http port dropped
		"example.com:8443": "example.com:8443", // non-default port kept
	}
	for in, want := range cases {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}
