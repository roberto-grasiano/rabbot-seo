package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

const testToken = "rab_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// optsFor returns verify Options targeting a loopback httptest server: the SSRF
// guard is cleared (AllowPrivate) and BaseOverride redirects https://<host> to
// the server's http base so the verifier hits the test server.
func optsFor(srv *httptest.Server) Options {
	return Options{AllowPrivate: true, BaseOverride: srv.URL}
}

// hostOf extracts the host:port from an httptest server URL.
func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u.Host
}

func TestVerifyWellKnownSuccess(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/.well-known/rabbot-verify.txt" {
			// Surround with whitespace/newlines to prove the verifier trims.
			_, _ = w.Write([]byte("\n  " + testToken + "  \n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	reason, err := VerifyWellKnown(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyWellKnown() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyWellKnown() = %q, want %q", reason, ReasonVerified)
	}
	if gotPath != "/.well-known/rabbot-verify.txt" {
		t.Fatalf("server saw path %q, want /.well-known/rabbot-verify.txt", gotPath)
	}
}

func TestVerifyWellKnownMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("rab_DIFFERENTTOKENVALUEXXXXXXXXXXXXXX"))
	}))
	defer srv.Close()

	reason, err := VerifyWellKnown(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyWellKnown() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyWellKnown() = verified on token mismatch, want not verified")
	}
}

func TestVerifyWellKnownMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	reason, err := VerifyWellKnown(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyWellKnown() error = %v (404 must be non-fatal)", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyWellKnown() = verified on 404, want not verified")
	}
}

func TestVerifyWellKnownWrongPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
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
		t.Fatal("VerifyWellKnown() = verified when token only served at /, want not verified")
	}
}

func metaServer(t *testing.T, head string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head>" + head + "</head><body>hi</body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyMetaSuccess(t *testing.T) {
	srv := metaServer(t, `<meta name="rabbot-verify" content="`+testToken+`">`)
	reason, err := VerifyMeta(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyMeta() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyMeta() = %q, want %q", reason, ReasonVerified)
	}
}

func TestVerifyMetaMismatch(t *testing.T) {
	srv := metaServer(t, `<meta name="rabbot-verify" content="rab_WRONGWRONGWRONGWRONGWRONGWRONGWR">`)
	reason, err := VerifyMeta(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyMeta() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyMeta() = verified on content mismatch, want not verified")
	}
}

func TestVerifyMetaAbsent(t *testing.T) {
	srv := metaServer(t, `<meta name="description" content="no proof here">`)
	reason, err := VerifyMeta(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyMeta() error = %v", err)
	}
	if reason == ReasonVerified {
		t.Fatal("VerifyMeta() = verified with no proof meta, want not verified")
	}
}

func TestVerifyMetaAttrOrderAndCase(t *testing.T) {
	// Reversed attribute order AND mixed-case attribute/tag name: goquery
	// (x/net/html) lowercases tag and attribute names, so the selector still hits.
	srv := metaServer(t, `<META Content="`+testToken+`" Name="rabbot-verify">`)
	reason, err := VerifyMeta(context.Background(), hostOf(t, srv), testToken, optsFor(srv))
	if err != nil {
		t.Fatalf("VerifyMeta() error = %v", err)
	}
	if reason != ReasonVerified {
		t.Fatalf("VerifyMeta() = %q on reversed-order/mixed-case meta, want %q", reason, ReasonVerified)
	}
}

// TestStateConstants pins the persisted State strings. These exact values are
// written to both the DB (verification_state) and config; a silent typo here
// would break the Phase 4 throttle resolver's reads, so they are load-bearing.
func TestStateConstants(t *testing.T) {
	cases := []struct {
		got  State
		want string
	}{
		{StateVerified, "verified"},
		{StateAttested, "attested"},
		{StateThrottled, "throttled"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("State = %q, want %q", c.got, c.want)
		}
	}
}

// TestMethodConstants pins the persisted Method strings (verification_method).
func TestMethodConstants(t *testing.T) {
	cases := []struct {
		got  Method
		want string
	}{
		{MethodWellKnown, "well_known"},
		{MethodDNS, "dns"},
		{MethodMeta, "meta"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Method = %q, want %q", c.got, c.want)
		}
	}
}

// TestProofRecordShape confirms a zero ProofRecord has empty Method/Token and
// zero-value timestamps — the proof record is never a bare boolean.
func TestProofRecordShape(t *testing.T) {
	var rec ProofRecord
	if rec.Method != "" {
		t.Errorf("zero ProofRecord.Method = %q, want empty", rec.Method)
	}
	if rec.Token != "" {
		t.Errorf("zero ProofRecord.Token = %q, want empty", rec.Token)
	}
	if !rec.VerifiedAt.IsZero() {
		t.Errorf("zero ProofRecord.VerifiedAt = %v, want zero", rec.VerifiedAt)
	}
	if !rec.LastReverifiedAt.IsZero() {
		t.Errorf("zero ProofRecord.LastReverifiedAt = %v, want zero", rec.LastReverifiedAt)
	}
	// Fields are settable with the documented types.
	now := time.Now()
	rec = ProofRecord{
		SiteID:           7,
		Method:           MethodMeta,
		Token:            "rab_x",
		State:            StateVerified,
		VerifiedAt:       now,
		LastReverifiedAt: now,
	}
	if rec.SiteID != 7 || rec.State != StateVerified {
		t.Errorf("ProofRecord not settable: %+v", rec)
	}
}
