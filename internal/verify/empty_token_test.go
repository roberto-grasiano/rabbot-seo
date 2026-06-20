package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmptyTokenNeverVerifies pins the empty-token guard inside each per-method
// verifier. subtle.ConstantTimeCompare returns 1 for two empty byte slices, so
// without a guard an empty token against a 200 with an empty/whitespace body
// (well-known) — or an empty meta content, or an empty DNS value — would falsely
// verify. An empty token is never a real proof, so every surface must return a
// non-verified Reason for it. (Production never passes an empty token: Verify
// DERIVES a non-empty token from the instance key. This guards the verifiers
// directly so a future caller cannot reintroduce the empty-proof bypass.)
func TestEmptyTokenNeverVerifies(t *testing.T) {
	// well-known: 200 with an empty body. Without the guard, TrimSpace("")=="" ==
	// token("") would verify.
	wkEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200 with an empty body.
	}))
	defer wkEmpty.Close()
	// well-known: 200 with a whitespace-only body (TrimSpace -> "").
	wkWhitespace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("   \n\t "))
	}))
	defer wkWhitespace.Close()

	t.Run("well-known empty body", func(t *testing.T) {
		reason, err := VerifyWellKnown(context.Background(), hostOf(t, wkEmpty), "", optsFor(wkEmpty))
		if err != nil {
			t.Fatalf("VerifyWellKnown() error = %v", err)
		}
		if reason == ReasonVerified {
			t.Fatal("empty token must NOT verify against an empty well-known body")
		}
	})

	t.Run("well-known whitespace body", func(t *testing.T) {
		reason, err := VerifyWellKnown(context.Background(), hostOf(t, wkWhitespace), "", optsFor(wkWhitespace))
		if err != nil {
			t.Fatalf("VerifyWellKnown() error = %v", err)
		}
		if reason == ReasonVerified {
			t.Fatal("empty token must NOT verify against a whitespace-only well-known body")
		}
	})

	t.Run("meta empty content", func(t *testing.T) {
		srv := metaServer(t, `<meta name="rabbot-verify" content="">`)
		reason, err := VerifyMeta(context.Background(), hostOf(t, srv), "", optsFor(srv))
		if err != nil {
			t.Fatalf("VerifyMeta() error = %v", err)
		}
		if reason == ReasonVerified {
			t.Fatal("empty token must NOT verify against an empty meta content")
		}
	})

	t.Run("dns empty value", func(t *testing.T) {
		lookup := func(_ context.Context, _ string) ([]string, error) {
			// A TXT record with the prefix but an empty value.
			return []string{dnsTXTPrefix}, nil
		}
		reason, err := VerifyDNS(context.Background(), "example.com", "", lookup)
		if err != nil {
			t.Fatalf("VerifyDNS() error = %v", err)
		}
		if reason == ReasonVerified {
			t.Fatal("empty token must NOT verify against an empty DNS TXT value")
		}
	})

	t.Run("empty key never verifies via Verify", func(t *testing.T) {
		// An empty instance KEY is the production-relevant fail-safe: Verify derives
		// the expected token from the key, so an absent key must never verify any
		// surface (it returns ReasonUnreachable, throttled). This is the post-cutover
		// equivalent of the old empty-token dispatch guard.
		now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
		for _, m := range []Method{MethodWellKnown, MethodMeta, MethodDNS} {
			opts := optsFor(wkEmpty)
			opts.Now = now
			// No Key set: opts.Key is nil.
			out, err := Verify(context.Background(), Request{
				SiteID: 1,
				Host:   hostOf(t, wkEmpty),
				Method: m,
				Lookup: func(_ context.Context, _ string) ([]string, error) {
					return []string{dnsTXTPrefix}, nil
				},
			}, opts)
			if err != nil {
				t.Fatalf("Verify(method=%s) error = %v", m, err)
			}
			if out.Record.State == StateVerified {
				t.Fatalf("Verify(method=%s) returned verified with no instance key", m)
			}
			if out.Reason != ReasonUnreachable {
				t.Fatalf("Verify(method=%s) reason = %q, want %q for an empty key", m, out.Reason, ReasonUnreachable)
			}
		}
	})
}
