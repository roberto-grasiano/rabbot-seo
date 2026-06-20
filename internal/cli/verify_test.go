package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// verifyHarness wires a temp DB + config.yaml and one registered site, returning
// the deps for runVerify pointed at an httptest server.
type verifyHarness struct {
	db         *store.DB
	configPath string
	cfg        *config.Config
	siteURL    string
}

func newVerifyHarness(t *testing.T, srv *httptest.Server, attested bool) verifyHarness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rabbot.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The site's base URL is the httptest server URL so GetSiteByBaseURL resolves
	// it and the verifier (via BaseOverride) hits the same server.
	siteURL := srv.URL
	if _, err := db.AddSite(context.Background(), model.Site{
		BaseURL: siteURL, Name: "T", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	}); err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	if err := config.AddSiteYAML(configPath, config.SiteConfig{URL: siteURL, Name: "T"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: siteURL, Name: "T"}}
	if attested {
		cfg.Setup.AttestedAt = "2026-06-04T00:00:00Z"
	}
	return verifyHarness{db: db, configPath: configPath, cfg: &cfg, siteURL: siteURL}
}

func wellKnownTokenServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/rabbot-verify.txt" {
			_, _ = w.Write([]byte(token))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testInstanceKey returns a deterministic non-zero instance key for tests. The
// token is DERIVED from this key (instance-bound), so the surface must serve
// verify.DeriveToken(key, host) to verify successfully.
func testInstanceKey() []byte {
	key := make([]byte, 32)
	key[0] = 9
	return key
}

func TestRunVerifyWellKnownSuccess(t *testing.T) {
	key := testInstanceKey()
	// Stand up a placeholder server first so we know its host, then derive the
	// token bound to that host and re-point the harness server at it.
	host := "" // filled after the server starts
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/rabbot-verify.txt" {
			_, _ = w.Write([]byte(token))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	host = hostFromURL(srv.URL)
	token = verify.DeriveToken(key, host) // what the owner would place

	h := newVerifyHarness(t, srv, false)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          now,
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	// (a) DB proof record reads verified.
	site, err := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	rec, err := h.db.GetVerification(context.Background(), site.ID)
	if err != nil {
		t.Fatalf("GetVerification: %v", err)
	}
	if rec.State != verify.StateVerified {
		t.Fatalf("DB state = %q, want verified", rec.State)
	}
	// (b) config block written with the DERIVED token.
	reloaded, err := config.Load(h.configPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(reloaded.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(reloaded.Sites))
	}
	v := reloaded.Sites[0].Verification
	if v.Method != "well_known" || v.Token != token || v.VerifiedAt == "" {
		t.Fatalf("config verification not written: %+v (want token %q)", v, token)
	}
	// (c) output mentions full speed.
	if !strings.Contains(strings.ToLower(buf.String()), "full speed") {
		t.Fatalf("output missing full-speed message:\n%s", buf.String())
	}
}

func TestRunVerifyFailureThrottled(t *testing.T) {
	key := testInstanceKey()
	// Server serves a DIFFERENT token than the one derived from the key.
	srv := wellKnownTokenServer(t, "rab_WRONGWRONGWRONGWRONGWRONGWRONGWR")
	h := newVerifyHarness(t, srv, false)

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Now(),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	site, _ := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	rec, _ := h.db.GetVerification(context.Background(), site.ID)
	if rec.State != verify.StateThrottled {
		t.Fatalf("DB state = %q, want throttled on failed verify", rec.State)
	}
	out := strings.ToLower(buf.String())
	if strings.Contains(out, "verified") && !strings.Contains(out, "not verified") {
		t.Fatalf("output falsely claims verified:\n%s", buf.String())
	}
}

// TestRunVerifyCleanMissPersistsDerivedToken pins the contract for a clean
// (non-verified, no transport error) verify-now: the DERIVED token the user was
// told to place is persisted to the config block so a SECOND `rabbot verify`
// shows the SAME token (derivation is deterministic per instance key + host).
func TestRunVerifyCleanMissPersistsDerivedToken(t *testing.T) {
	key := testInstanceKey()
	// The server serves a DIFFERENT token, so the fetch SUCCEEDS (verr==nil) but
	// the proof is a clean miss (StateThrottled).
	srv := wellKnownTokenServer(t, "rab_WRONGWRONGWRONGWRONGWRONGWRONGWR")
	h := newVerifyHarness(t, srv, false)

	wantToken := verify.DeriveToken(key, hostFromURL(h.siteURL))

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Now(),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	// The DB proof record carries the exact DERIVED token.
	site, _ := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	rec, err := h.db.GetVerification(context.Background(), site.ID)
	if err != nil {
		t.Fatalf("GetVerification: %v", err)
	}
	if rec.Token != wantToken {
		t.Fatalf("DB record token = %q, want derived %q", rec.Token, wantToken)
	}

	// The config block MUST persist that same token so a retry shows it again.
	reloaded, err := config.Load(h.configPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(reloaded.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(reloaded.Sites))
	}
	v := reloaded.Sites[0].Verification
	if v.Token != wantToken {
		t.Fatalf("config token = %q, want %q (the derived token the user was told to place)", v.Token, wantToken)
	}
	if v.Method != string(verify.MethodWellKnown) {
		t.Fatalf("config method = %q, want well_known", v.Method)
	}
	// A clean miss must NOT record a verified timestamp.
	if v.VerifiedAt != "" {
		t.Fatalf("clean-miss config should not set VerifiedAt, got %q", v.VerifiedAt)
	}
}

// TestRunVerifyTransportErrorPersistsDerivedToken is the transport-error twin of
// the clean-miss test: when the verifier fetch itself fails (verr!=nil), the
// derived token must still be persisted so a retry shows it again.
func TestRunVerifyTransportErrorPersistsDerivedToken(t *testing.T) {
	key := testInstanceKey()
	// Stand up a server, capture its URL, then close it so Do() fails with a
	// connection-refused transport error (verr!=nil).
	srv := wellKnownTokenServer(t, "rab_unused")
	h := newVerifyHarness(t, srv, false)
	deadURL := srv.URL
	srv.Close()

	wantToken := verify.DeriveToken(key, hostFromURL(h.siteURL))

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: deadURL,
		now:          time.Now(),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	site, _ := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	rec, err := h.db.GetVerification(context.Background(), site.ID)
	if err != nil {
		t.Fatalf("GetVerification: %v", err)
	}
	if rec.Token != wantToken {
		t.Fatalf("DB record token = %q, want derived %q", rec.Token, wantToken)
	}

	reloaded, err := config.Load(h.configPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	v := reloaded.Sites[0].Verification
	if v.Token != wantToken {
		t.Fatalf("config token = %q, want %q (persisted across a transport error)", v.Token, wantToken)
	}
	if v.VerifiedAt != "" {
		t.Fatalf("transport-error config should not set VerifiedAt, got %q", v.VerifiedAt)
	}
}

// TestRunVerifyConfigAbsentWarns covers the DB-present/config-absent drift: the
// site exists in the DB (so GetSiteByBaseURL resolves) but is NOT in config.yaml.
// SetSiteVerificationYAML returns found=false and writes nothing, so runVerify
// must emit a note so the success message does not falsely imply the config intent
// block landed.
func TestRunVerifyConfigAbsentWarns(t *testing.T) {
	key := testInstanceKey()
	host := ""
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/rabbot-verify.txt" {
			_, _ = w.Write([]byte(token))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	host = hostFromURL(srv.URL)
	token = verify.DeriveToken(key, host)

	h := newVerifyHarness(t, srv, false)

	// Remove the site from config.yaml so SetSiteVerificationYAML reports
	// found=false while the DB row (added by the harness) remains.
	if found, err := config.RemoveSiteYAML(h.configPath, h.siteURL); err != nil || !found {
		t.Fatalf("RemoveSiteYAML found=%v err=%v", found, err)
	}
	// The in-memory cfg must match the on-disk drift (no site).
	h.cfg.Sites = nil

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	// The DB still flips to verified (the proof check succeeded).
	site, _ := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	rec, _ := h.db.GetVerification(context.Background(), site.ID)
	if rec.State != verify.StateVerified {
		t.Fatalf("DB state = %q, want verified", rec.State)
	}
	out := strings.ToLower(buf.String())
	if !strings.Contains(out, "not found in config.yaml") {
		t.Fatalf("output must warn the config intent was not recorded:\n%s", buf.String())
	}
}

func TestRunVerifySkipRecordsAttested(t *testing.T) {
	key := testInstanceKey()
	srv := wellKnownTokenServer(t, "rab_unused")
	h := newVerifyHarness(t, srv, true) // attested

	wantToken := verify.DeriveToken(key, hostFromURL(h.siteURL))

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		skip:         true,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Now(),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify --skip: %v", err)
	}
	site, _ := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	rec, _ := h.db.GetVerification(context.Background(), site.ID)
	if rec.State != verify.StateAttested {
		t.Fatalf("DB state = %q, want attested on --skip", rec.State)
	}
	// The DERIVED token MUST persist on the attested DB record so a later verify
	// shows the operator the same token to place.
	if rec.Token != wantToken {
		t.Fatalf("DB record token = %q, want derived %q (token must persist on --skip)", rec.Token, wantToken)
	}
	out := strings.ToLower(buf.String())
	if !strings.Contains(out, "throttled") {
		t.Fatalf("skip output should mention throttled:\n%s", buf.String())
	}
}

func TestRunVerifySkipRequiresAttestation(t *testing.T) {
	srv := wellKnownTokenServer(t, "rab_unused")
	h := newVerifyHarness(t, srv, false) // NOT attested

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          testInstanceKey(),
		skip:         true,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Now(),
	}
	if err := runVerify(context.Background(), &buf, deps); err == nil {
		t.Fatal("runVerify --skip without attestation should error")
	}
}

// TestPrintPlacementDNSStripsPort pins the DNS placement hint to the BARE
// hostname: VerifyDNS (internal/verify/dns.go) strips any :port before resolving
// (DNS has no concept of ports), so the hint must name the same host or it would
// tell the operator to add the TXT record on "example.com:8443" while the lookup
// targets "example.com". The well-known/meta branches keep the full host:port
// because their HTTP fetch uses it.
func TestPrintPlacementDNSStripsPort(t *testing.T) {
	const token = "rab_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	var dnsBuf bytes.Buffer
	printPlacement(&errWriter{w: &dnsBuf}, verify.MethodDNS, "example.com:8443", token)
	dnsOut := dnsBuf.String()
	if !strings.Contains(dnsOut, "example.com:\n") {
		t.Fatalf("DNS hint should name the bare host:\n%s", dnsOut)
	}
	if strings.Contains(dnsOut, "8443") {
		t.Fatalf("DNS hint must not include the port:\n%s", dnsOut)
	}

	// Sanity: the well-known branch DOES keep the port (its HTTP fetch needs it).
	var wkBuf bytes.Buffer
	printPlacement(&errWriter{w: &wkBuf}, verify.MethodWellKnown, "example.com:8443", token)
	if !strings.Contains(wkBuf.String(), "example.com:8443") {
		t.Fatalf("well-known hint should keep the full host:port:\n%s", wkBuf.String())
	}
}

func TestVerifyCommandRegistered(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "verify" {
			found = true
		}
	}
	if !found {
		t.Fatal("verify command not registered on the root command")
	}
}

// verifyCmd returns the standalone verify command for flag-level assertions.
func verifyCmd(t *testing.T) *cobra.Command {
	t.Helper()
	root := NewRootCmd(BuildInfo{Version: "test"})
	for _, c := range root.Commands() {
		if c.Name() == "verify" {
			return c
		}
	}
	t.Fatal("verify command not found")
	return nil
}

// TestVerifyCommandHasNoTokenFlag pins the instance-bound contract: the token is
// always DERIVED from the per-instance key and re-derived on every check, so
// there is nothing to pass — the --token flag is removed. The sibling flags stay.
func TestVerifyCommandHasNoTokenFlag(t *testing.T) {
	cmd := verifyCmd(t)
	if cmd.Flags().Lookup("token") != nil {
		t.Fatal("verify command must NOT register a --token flag (the token is instance-derived)")
	}
	for _, name := range []string{"method", "skip"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("verify command must keep the --%s flag", name)
		}
	}
}
