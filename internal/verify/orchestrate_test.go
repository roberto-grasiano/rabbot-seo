package verify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestBegin_DerivesTokenNoWrite(t *testing.T) {
	key := dispatchKey() // fixed non-zero key (dispatch_test.go)
	const host = "example.com"
	want := DeriveToken(key, host)

	got, err := Begin(42, host, MethodWellKnown, key)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if got.SiteID != 42 {
		t.Errorf("SiteID = %d, want 42", got.SiteID)
	}
	if got.Host != host {
		t.Errorf("Host = %q, want %q", got.Host, host)
	}
	if got.Method != MethodWellKnown {
		t.Errorf("Method = %q, want %q", got.Method, MethodWellKnown)
	}
	if got.Token != want {
		t.Errorf("Token = %q, want derived %q", got.Token, want)
	}
	// Instructions must name the well-known path and carry the token.
	if !strings.Contains(got.Instructions, wellKnownPath) {
		t.Errorf("Instructions %q missing well-known path %q", got.Instructions, wellKnownPath)
	}
	if !strings.Contains(got.Instructions, want) {
		t.Errorf("Instructions %q missing token", got.Instructions)
	}
}

func TestBegin_EmptyKeyFailsClosed(t *testing.T) {
	if _, err := Begin(1, "example.com", MethodMeta, nil); err == nil {
		t.Fatal("Begin() with empty key = nil error, want fail-closed error")
	}
}

func TestBegin_DNSInstructionsUseBareHostname(t *testing.T) {
	key := dispatchKey()
	got, err := Begin(1, "example.com:8443", MethodDNS, key)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	// DNS resolves names, not host:port — the instruction must show the bare host.
	if strings.Contains(got.Instructions, ":8443") {
		t.Errorf("DNS Instructions %q must not contain a port", got.Instructions)
	}
	if !strings.Contains(got.Instructions, "rabbot-verify=") {
		t.Errorf("DNS Instructions %q missing TXT prefix", got.Instructions)
	}
}

// fakeStore is an in-memory ProofStore: it records the saved record so the test
// can assert Check writes exactly once with the right tier.
type fakeStore struct {
	site      model.Site
	getErr    error
	saved     *ProofRecord
	saveErr   error
	saveCalls int
}

func (f *fakeStore) GetSite(_ context.Context, _ int64) (model.Site, error) {
	return f.site, f.getErr
}

func (f *fakeStore) SaveVerification(_ context.Context, _ int64, rec ProofRecord) error {
	f.saveCalls++
	r := rec
	f.saved = &r
	return f.saveErr
}

func TestCheck_VerifiedPersistsRecord(t *testing.T) {
	key := dispatchKey()
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wellKnownPath {
			_, _ = w.Write([]byte(DeriveToken(key, host)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	host = hostOf(t, srv) // helper from the existing verify test suite

	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{site: model.Site{ID: 5, BaseURL: "https://" + host}}
	opts := optsFor(srv) // sets BaseOverride + AllowPrivate for the httptest loopback
	opts.Now = now
	opts.Key = key

	res, err := Check(context.Background(), st, 5, host, MethodWellKnown, opts)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Reason != ReasonVerified {
		t.Fatalf("Reason = %q, want verified", res.Reason)
	}
	if res.Record.State != StateVerified {
		t.Fatalf("State = %q, want verified", res.Record.State)
	}
	if st.saveCalls != 1 {
		t.Fatalf("SaveVerification calls = %d, want exactly 1", st.saveCalls)
	}
	if st.saved == nil || st.saved.State != StateVerified {
		t.Fatalf("persisted record = %+v, want StateVerified", st.saved)
	}
}

func TestCheck_MissPersistsThrottledNeverVerified(t *testing.T) {
	key := dispatchKey()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("rab_WRONGWRONGWRONGWRONGWRONGWRONGWR"))
	}))
	defer srv.Close()
	host := hostOf(t, srv)

	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{site: model.Site{ID: 9, BaseURL: "https://" + host}}
	opts := optsFor(srv)
	opts.Now = now
	opts.Key = key

	res, err := Check(context.Background(), st, 9, host, MethodWellKnown, opts)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Record.State != StateThrottled {
		t.Fatalf("State = %q, want throttled on a miss", res.Record.State)
	}
	if res.Reason != ReasonMismatch {
		t.Fatalf("Reason = %q, want mismatch", res.Reason)
	}
	if st.saveCalls != 1 {
		t.Fatalf("SaveVerification calls = %d, want 1 (the throttled attempt is recorded)", st.saveCalls)
	}
}

func TestCheck_EmptyKeyThrottledUnreachable(t *testing.T) {
	st := &fakeStore{site: model.Site{ID: 1, BaseURL: "https://example.com"}}
	res, err := Check(context.Background(), st, 1, "example.com", MethodWellKnown, Options{Now: time.Now()})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Record.State != StateThrottled || res.Reason != ReasonUnreachable {
		t.Fatalf("empty key: State=%q Reason=%q, want throttled/unreachable", res.Record.State, res.Reason)
	}
	if st.saveCalls != 1 {
		t.Fatalf("SaveVerification calls = %d, want 1 (record the failed attempt)", st.saveCalls)
	}
}

func TestCheck_SaveErrorSurfaces(t *testing.T) {
	key := dispatchKey()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("rab_WRONG"))
	}))
	defer srv.Close()
	st := &fakeStore{site: model.Site{ID: 1}, saveErr: errors.New("disk full")}
	opts := optsFor(srv)
	opts.Key = key
	if _, err := Check(context.Background(), st, 1, hostOf(t, srv), MethodWellKnown, opts); err == nil {
		t.Fatal("Check() with a store save error = nil, want the error surfaced")
	}
}
