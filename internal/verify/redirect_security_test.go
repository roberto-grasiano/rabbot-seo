package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file is the SECURITY-LOAD-BEARING refutation of the spec §Security rule:
// "a redirect to an attacker-controlled host must not satisfy a proof." It pins
// the no-redirect behavior of fetcher.GuardedNoRedirectClient
// (CheckRedirect == http.ErrUseLastResponse) used by VerifyWellKnown/VerifyMeta.
// If any of these tests start FAILING, the verifier is following redirects: fix
// the IMPLEMENTATION (restore the no-follow client), never loosen the test.

// TestWellKnownOffHostRedirectDoesNotVerify: the target 30x-redirects the
// well-known path to an attacker host that serves the CORRECT token. Because the
// verifier never follows the redirect, the proof must NOT be satisfied.
func TestWellKnownOffHostRedirectDoesNotVerify(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Attacker serves the correct token at the well-known path.
		_, _ = w.Write([]byte(testToken))
	}))
	defer attacker.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect the well-known path to the attacker's matching URL.
		http.Redirect(w, r, attacker.URL+wellKnownPath, http.StatusFound)
	}))
	defer target.Close()

	reason, err := VerifyWellKnown(context.Background(), hostOf(t, target), testToken, optsFor(target))
	if err != nil {
		t.Fatalf("VerifyWellKnown() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("SECURITY REGRESSION: off-host redirect satisfied the well-known proof; the verifier is following redirects")
	}
	if reason != ReasonRedirected {
		t.Fatalf("VerifyWellKnown() = %q, want %q (a refused redirect)", reason, ReasonRedirected)
	}
}

// TestMetaOffHostRedirectDoesNotVerify: the target 30x-redirects the homepage to
// an attacker host whose homepage carries the CORRECT meta token. The verifier
// must not follow it, so the proof must NOT be satisfied.
func TestMetaOffHostRedirectDoesNotVerify(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><meta name="rabbot-verify" content="` + testToken + `"></head><body>x</body></html>`))
	}))
	defer attacker.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/", http.StatusFound)
	}))
	defer target.Close()

	reason, err := VerifyMeta(context.Background(), hostOf(t, target), testToken, optsFor(target))
	if err != nil {
		t.Fatalf("VerifyMeta() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("SECURITY REGRESSION: off-host redirect satisfied the meta proof; the verifier is following redirects")
	}
	if reason != ReasonRedirected {
		t.Fatalf("VerifyMeta() = %q, want %q (a refused redirect)", reason, ReasonRedirected)
	}
}

// TestSameHostRedirectNotFollowed: even a SAME-host 302 on the well-known path
// must not verify — the token must sit at the exact path returning 200, not
// behind a redirect (the no-redirect rule is absolute, not just off-host).
func TestSameHostRedirectNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wellKnownPath {
			// Same-host redirect to another path that serves the token.
			http.Redirect(w, r, "/elsewhere.txt", http.StatusFound)
			return
		}
		if r.URL.Path == "/elsewhere.txt" {
			_, _ = w.Write([]byte(testToken))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	reason, err := VerifyWellKnown(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyWellKnown() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("SECURITY REGRESSION: same-host redirect satisfied the well-known proof; require 200 at the exact path")
	}
	if reason != ReasonRedirected {
		t.Fatalf("VerifyWellKnown() = %q, want %q (a refused redirect)", reason, ReasonRedirected)
	}
}
