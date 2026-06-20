package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// dispatchKey is a fixed non-zero instance key for the dispatch tests: Verify
// DERIVES the expected token from it, so the surface must serve DeriveToken(key,
// host) for a match.
func dispatchKey() []byte {
	k := make([]byte, instanceKeyBytes)
	k[0] = 0x2a
	return k
}

func TestVerifyDispatchVerified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	key := dispatchKey()
	host := hostOf(t, srv)
	// Re-handler the server now that the host (and thus the derived token) is known.
	want := DeriveToken(key, host)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wellKnownPath {
			_, _ = w.Write([]byte(want))
			return
		}
		http.NotFound(w, r)
	})

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	opts := optsFor(srv)
	opts.Now = now
	opts.Key = key

	out, err := Verify(context.Background(), Request{
		SiteID: 42,
		Host:   host,
		Method: MethodWellKnown,
	}, opts)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	rec := out.Record
	if out.Reason != ReasonVerified {
		t.Fatalf("out.Reason = %q, want %q", out.Reason, ReasonVerified)
	}
	if rec.State != StateVerified {
		t.Fatalf("rec.State = %q, want %q", rec.State, StateVerified)
	}
	if !rec.VerifiedAt.Equal(now) {
		t.Errorf("rec.VerifiedAt = %v, want %v", rec.VerifiedAt, now)
	}
	if !rec.LastReverifiedAt.Equal(now) {
		t.Errorf("rec.LastReverifiedAt = %v, want %v", rec.LastReverifiedAt, now)
	}
	if rec.Method != MethodWellKnown {
		t.Errorf("rec.Method = %q, want %q", rec.Method, MethodWellKnown)
	}
	// The token is DERIVED, not caller-supplied; it is stored for display/audit.
	if rec.Token != want {
		t.Errorf("rec.Token = %q, want the derived token %q", rec.Token, want)
	}
	if rec.SiteID != 42 {
		t.Errorf("rec.SiteID = %d, want 42", rec.SiteID)
	}
}

func TestVerifyDispatchFailureThrottled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve a token that the instance key would never derive.
		_, _ = w.Write([]byte("rab_WRONGWRONGWRONGWRONGWRONGWRONGWR"))
	}))
	defer srv.Close()

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	opts := optsFor(srv)
	opts.Now = now
	opts.Key = dispatchKey()

	out, err := Verify(context.Background(), Request{
		SiteID: 1,
		Host:   hostOf(t, srv),
		Method: MethodWellKnown,
	}, opts)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	rec := out.Record
	if rec.State != StateThrottled {
		t.Fatalf("rec.State = %q, want %q (failed verify => throttled)", rec.State, StateThrottled)
	}
	if out.Reason != ReasonMismatch {
		t.Fatalf("out.Reason = %q, want %q", out.Reason, ReasonMismatch)
	}
	if !rec.VerifiedAt.IsZero() {
		t.Errorf("rec.VerifiedAt = %v, want zero on failure", rec.VerifiedAt)
	}
	// LastReverifiedAt records the attempt even on failure.
	if !rec.LastReverifiedAt.Equal(now) {
		t.Errorf("rec.LastReverifiedAt = %v, want %v (attempt timestamp)", rec.LastReverifiedAt, now)
	}
}

func TestVerifyDispatchDNS(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	key := dispatchKey()
	want := DeriveToken(key, "example.com")
	out, err := Verify(context.Background(), Request{
		SiteID: 3,
		Host:   "example.com",
		Method: MethodDNS,
		Lookup: func(_ context.Context, _ string) ([]string, error) {
			return []string{"rabbot-verify=" + want}, nil
		},
	}, Options{Now: now, Key: key})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	rec := out.Record
	if rec.State != StateVerified {
		t.Fatalf("rec.State = %q, want verified", rec.State)
	}
	if rec.Method != MethodDNS {
		t.Errorf("rec.Method = %q, want dns", rec.Method)
	}
}

func TestAttest(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	rec := Attest(99, MethodWellKnown, now)
	if rec.State != StateAttested {
		t.Fatalf("rec.State = %q, want %q", rec.State, StateAttested)
	}
	if !rec.VerifiedAt.IsZero() {
		t.Errorf("rec.VerifiedAt = %v, want zero (attest never proves control)", rec.VerifiedAt)
	}
	if rec.SiteID != 99 {
		t.Errorf("rec.SiteID = %d, want 99", rec.SiteID)
	}
	if rec.Method != MethodWellKnown {
		t.Errorf("rec.Method = %q, want well_known", rec.Method)
	}
}
