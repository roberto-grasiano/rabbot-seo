package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVerifyDNSSuccess(t *testing.T) {
	var gotHost string
	lookup := func(_ context.Context, host string) ([]string, error) {
		gotHost = host
		return []string{"v=spf1 include:_spf.example.com ~all", "rabbot-verify=" + testToken}, nil
	}
	reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
	if err != nil {
		t.Fatalf("VerifyDNS() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyDNS() = %q, want %q", reason, ReasonVerified)
	}
	if gotHost != "example.com" {
		t.Fatalf("lookup host = %q, want example.com (literal host)", gotHost)
	}
}

func TestVerifyDNSSuccessQuoted(t *testing.T) {
	// Some resolvers wrap TXT chunks in surrounding quotes; the verifier must
	// strip them before matching.
	lookup := func(_ context.Context, _ string) ([]string, error) {
		return []string{`"rabbot-verify=` + testToken + `"`}, nil
	}
	reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
	if err != nil {
		t.Fatalf("VerifyDNS() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyDNS() = %q on quoted TXT, want %q", reason, ReasonVerified)
	}
}

func TestVerifyDNSStripsPort(t *testing.T) {
	// DNS has no concept of ports: a site URL with an explicit port
	// (e.g. https://example.com:8443) must resolve the TXT lookup against the
	// BARE hostname, never host:port.
	var gotHost string
	lookup := func(_ context.Context, host string) ([]string, error) {
		gotHost = host
		return []string{"rabbot-verify=" + testToken}, nil
	}
	reason, err := VerifyDNS(context.Background(), "example.com:8443", testToken, lookup)
	if err != nil {
		t.Fatalf("VerifyDNS() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyDNS() = %q, want %q", reason, ReasonVerified)
	}
	if gotHost != "example.com" {
		t.Fatalf("lookup host = %q, want example.com (bare hostname, no port)", gotHost)
	}
}

func TestVerifyDNSNormalizesRecords(t *testing.T) {
	cases := []struct {
		name     string
		rec      string
		verified bool
	}{
		{"plain", "rabbot-verify=" + testToken, true},
		{"quoted", `"rabbot-verify=` + testToken + `"`, true},
		{"surrounding-whitespace", "  rabbot-verify=" + testToken + "  ", true},
		{"quoted-with-inner-whitespace", `"  rabbot-verify=` + testToken + `  "`, true},
		{"trailing-dot", "rabbot-verify=" + testToken + ".", true},
		{"different-token", "rabbot-verify=rab_WRONGWRONGWRONGWRONGWRONGWRONGWR", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(_ context.Context, _ string) ([]string, error) {
				return []string{tc.rec}, nil
			}
			reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
			if err != nil {
				t.Fatalf("VerifyDNS() error = %v", err)
			}
			if got := reason == ReasonVerified; got != tc.verified {
				t.Fatalf("VerifyDNS(%q) verified = %v (reason %q), want %v", tc.rec, got, reason, tc.verified)
			}
		})
	}
}

func TestVerifyDNSMismatch(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]string, error) {
		return []string{"rabbot-verify=rab_WRONGWRONGWRONGWRONGWRONGWRONGWR", "other=value"}, nil
	}
	reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
	if err != nil {
		t.Fatalf("VerifyDNS() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyDNS() = verified on no matching record, want not verified")
	}
}

func TestVerifyDNSNoRecords(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}
	reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
	if err != nil {
		t.Fatalf("VerifyDNS() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyDNS() = verified with no records, want not verified")
	}
}

func TestVerifyDNSLookupError(t *testing.T) {
	wantErr := errors.New("nxdomain")
	lookup := func(_ context.Context, _ string) ([]string, error) {
		return nil, wantErr
	}
	reason, err := VerifyDNS(context.Background(), "example.com", testToken, lookup)
	if reason == ReasonVerified {
		t.Fatal("VerifyDNS() = verified on lookup error, want not verified")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("VerifyDNS() error = %v, want it to surface %v", err, wantErr)
	}
	// The doc comment promises the lookup error is WRAPPED for diagnostics: it
	// must add the hostname context, not return the bare sentinel. errors.Is must
	// still unwrap to the original (asserted above).
	if !strings.Contains(err.Error(), "example.com") {
		t.Fatalf("VerifyDNS() error = %q, want it wrapped with the hostname for diagnostics", err)
	}
	if err.Error() == wantErr.Error() {
		t.Fatalf("VerifyDNS() error = %q, want a wrapped message, not the raw sentinel", err)
	}
}
