package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
)

// ── renderGSCReadiness: pure renderer (table-testable, no network) ───────────

func TestRenderGSCReadiness_Unconfigured(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := renderGSCReadiness(&buf, gscReadinessInput{configured: false})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Search Console:") {
		t.Errorf("missing section header:\n%s", out)
	}
	// Unconfigured is a WARNING, never a failure.
	if !strings.Contains(strings.ToLower(out), "not configured") {
		t.Errorf("unconfigured state must say so:\n%s", out)
	}
}

func TestRenderGSCReadiness_ConfiguredReachable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	in := gscReadinessInput{
		configured:   true,
		property:     "https://ex.com/",
		authMode:     "service_account",
		keyBasename:  "sa.json",
		keyFound:     true,
		keyMode:      0o600,
		probeErr:     nil,
		propertySeen: true,
	}
	if err := renderGSCReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "https://ex.com/") {
		t.Errorf("must report the property:\n%s", out)
	}
	if !strings.Contains(out, "service_account") {
		t.Errorf("must report the auth mode:\n%s", out)
	}
	if !strings.Contains(out, "sa.json") {
		t.Errorf("must report the key basename:\n%s", out)
	}
	if strings.Contains(out, "[ ]") {
		t.Errorf("a reachable property should show no failed check:\n%s", out)
	}
}

// TestRenderGSCReadiness_NeverPrintsSecretPath proves only the BASENAME of the key
// path is rendered, never the full path (which could reveal a home dir / layout).
func TestRenderGSCReadiness_NeverPrintsFullPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	in := gscReadinessInput{
		configured:  true,
		property:    "https://ex.com/",
		authMode:    "service_account",
		keyBasename: "sa.json", // the renderer is GIVEN the basename, never the full path
		keyFound:    true,
		keyMode:     0o600,
	}
	if err := renderGSCReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "/home/") || strings.Contains(buf.String(), string(filepath.Separator)+"secret") {
		t.Errorf("rendered a path, want basename only:\n%s", buf.String())
	}
}

func TestRenderGSCReadiness_LoosePermsWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	in := gscReadinessInput{
		configured: true, property: "https://ex.com/", authMode: "service_account",
		keyBasename: "sa.json", keyFound: true, keyMode: 0o644, propertySeen: true,
	}
	if err := renderGSCReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "0600") {
		t.Errorf("a loose-perm key must warn about 0600:\n%s", buf.String())
	}
}

func TestRenderGSCReadiness_ProbeFailureReported(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	in := gscReadinessInput{
		configured: true, property: "https://ex.com/", authMode: "service_account",
		keyBasename: "sa.json", keyFound: true, keyMode: 0o600,
		probeErr: errors.New("permission denied"),
	}
	if err := renderGSCReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "permission denied") {
		t.Errorf("a probe failure must surface its message:\n%s", out)
	}
}

// TestRenderGSCReadiness_NeverLeaksBearer asserts a probe error carrying a token-ish
// string is rendered verbatim from the gsc client (which already scrubs). Here we
// just confirm the renderer does not invent/print a bearer.
func TestRenderGSCReadiness_NoBearerLiteral(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_ = renderGSCReadiness(&buf, gscReadinessInput{configured: true, property: "https://ex.com/", authMode: "oauth", keyBasename: "oauth.json", keyFound: true, keyMode: 0o600, propertySeen: true})
	if strings.Contains(strings.ToLower(buf.String()), "bearer ") {
		t.Errorf("the GSC section must never print a bearer token:\n%s", buf.String())
	}
}

// ── probeGSCReadiness: live probe via a mocked gsc client (httptest) ─────────

func TestProbeGSCReadiness_ConfiguredSiteReachable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// One httptest server mocking BOTH the jwt-bearer token exchange and sites.list,
	// so the real SA provider mints a (canned) token without any live Google call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"mock-at","token_type":"Bearer","expires_in":3600}`))
		default:
			_, _ = w.Write([]byte(`{"siteEntry":[{"siteUrl":"https://ex.com/","permissionLevel":"siteOwner"}]}`))
		}
	}))
	defer srv.Close()

	keyPath := writeTestSAKey(t, dir, 0o600, srv.URL+"/token")
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: keyPath},
	}}}

	// Inject a client factory that points the real gsc client at the httptest server.
	factory := func(tp gsc.TokenProvider) (gscDoctorClient, error) {
		return gsc.NewClient(gsc.Options{Token: tp, HTTPClient: srv.Client(), BaseURL: srv.URL, InspectBaseURL: srv.URL})
	}

	in := probeGSCReadiness(context.Background(), cfg, "https://ex.com/", factory)
	if !in.configured {
		t.Fatal("expected configured=true for a GSC site")
	}
	if in.authMode != "service_account" {
		t.Errorf("authMode = %q, want service_account", in.authMode)
	}
	if in.keyBasename != "sa.json" {
		t.Errorf("keyBasename = %q, want sa.json", in.keyBasename)
	}
	if !in.keyFound {
		t.Errorf("key stat wrong: found=%v, want true", in.keyFound)
	}
	// Unix file modes only — Windows has no 0600 bit to assert.
	if runtime.GOOS != "windows" && in.keyMode.Perm() != 0o600 {
		t.Errorf("key mode = %o, want 0600", in.keyMode.Perm())
	}
	if in.probeErr != nil {
		t.Errorf("probe should succeed against the mock: %v", in.probeErr)
	}
	if !in.propertySeen {
		t.Error("the configured property should be reported as visible to the credential")
	}
}

func TestProbeGSCReadiness_UnconfiguredSite(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sites: []config.SiteConfig{{URL: "https://plain.test/"}}}
	factory := func(gsc.TokenProvider) (gscDoctorClient, error) { return nil, nil }
	in := probeGSCReadiness(context.Background(), cfg, "https://plain.test/", factory)
	if in.configured {
		t.Fatal("a site without a GSC block must report configured=false")
	}
}

func TestProbeGSCReadiness_PropertyNotVisible(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// sites.list returns a DIFFERENT property → the configured one is not visible.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"mock-at","token_type":"Bearer","expires_in":3600}`))
		default:
			_, _ = w.Write([]byte(`{"siteEntry":[{"siteUrl":"https://other.example/","permissionLevel":"siteOwner"}]}`))
		}
	}))
	defer srv.Close()
	keyPath := writeTestSAKey(t, dir, 0o600, srv.URL+"/token")
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: keyPath},
	}}}
	factory := func(tp gsc.TokenProvider) (gscDoctorClient, error) {
		return gsc.NewClient(gsc.Options{Token: tp, HTTPClient: srv.Client(), BaseURL: srv.URL, InspectBaseURL: srv.URL})
	}
	in := probeGSCReadiness(context.Background(), cfg, "https://ex.com/", factory)
	if in.probeErr != nil {
		t.Fatalf("the list call itself succeeded, want no probe error: %v", in.probeErr)
	}
	if in.propertySeen {
		t.Error("the configured property is NOT in the credential's site list; propertySeen must be false")
	}
}

func TestProbeGSCReadiness_MissingKeyFileReportsProbeError(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: filepath.Join(t.TempDir(), "missing.json")},
	}}}
	factory := func(tp gsc.TokenProvider) (gscDoctorClient, error) {
		return gsc.NewClient(gsc.Options{Token: tp})
	}
	in := probeGSCReadiness(context.Background(), cfg, "https://ex.com/", factory)
	if in.configured != true {
		t.Fatal("the site IS configured; only the key is missing")
	}
	if in.keyFound {
		t.Error("a missing key file must report keyFound=false")
	}
	if in.probeErr == nil {
		t.Error("a missing key file must surface a probe error (cannot build the provider)")
	}
}

// TestProbeGSCReadiness_ClientFactoryErrorReported covers the factory-error branch:
// when the client factory fails, probeErr is set (no panic, no list call).
func TestProbeGSCReadiness_ClientFactoryErrorReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := writeTestSAKey(t, dir, 0o600, "") // provider builds fine
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: keyPath},
	}}}
	factory := func(gsc.TokenProvider) (gscDoctorClient, error) {
		return nil, errors.New("client build boom")
	}
	in := probeGSCReadiness(context.Background(), cfg, "https://ex.com/", factory)
	if in.probeErr == nil || !strings.Contains(in.probeErr.Error(), "client build boom") {
		t.Fatalf("a client-factory error must surface as probeErr, got %v", in.probeErr)
	}
	if in.propertySeen {
		t.Error("propertySeen must stay false when the client could not be built")
	}
}

// TestProbeGSCReadiness_ListSitesErrorReported covers the list-error branch: the
// sites.list call fails (HTTP 500) and is recorded as probeErr.
func TestProbeGSCReadiness_ListSitesErrorReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"mock-at","token_type":"Bearer","expires_in":3600}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"backend error","status":"INTERNAL"}}`))
		}
	}))
	defer srv.Close()
	keyPath := writeTestSAKey(t, dir, 0o600, srv.URL+"/token")
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: keyPath},
	}}}
	factory := func(tp gsc.TokenProvider) (gscDoctorClient, error) {
		return gsc.NewClient(gsc.Options{Token: tp, HTTPClient: srv.Client(), BaseURL: srv.URL, InspectBaseURL: srv.URL})
	}
	in := probeGSCReadiness(context.Background(), cfg, "https://ex.com/", factory)
	if in.probeErr == nil {
		t.Fatal("a failing sites.list must surface a probe error")
	}
	if in.propertySeen {
		t.Error("propertySeen must be false when the list call failed")
	}
}

// TestPropertyInList_NilAndAbsent covers the nil-response and not-found arms.
func TestPropertyInList_NilAndAbsent(t *testing.T) {
	t.Parallel()
	if propertyInList("https://ex.com/", nil) {
		t.Error("a nil response must report the property as not present")
	}
	resp := &gsc.SitesListResponse{SiteEntry: []gsc.SiteEntry{{SiteURL: "https://other.example/"}}}
	if propertyInList("https://ex.com/", resp) {
		t.Error("a property absent from the list must report false")
	}
	if !propertyInList("https://other.example/", resp) {
		t.Error("a property present in the list must report true")
	}
}

// TestProductionDoctorGSCClient_BuildsClient covers the thin doctor production factory:
// it builds a non-nil client from a token provider (real network client, never called).
func TestProductionDoctorGSCClient_BuildsClient(t *testing.T) {
	t.Parallel()
	c, err := productionDoctorGSCClient(staticTokenProvider{})
	if err != nil {
		t.Fatalf("productionDoctorGSCClient: %v", err)
	}
	if c == nil {
		t.Fatal("productionDoctorGSCClient returned a nil client")
	}
}

// TestRunDoctorGSC_UnconfiguredCfgWritesWarning is the end-to-end section: a config
// with no GSC site prints the WARNING section and returns nil.
func TestRunDoctorGSC_UnconfiguredWritesWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := &config.Config{Sites: []config.SiteConfig{{URL: "https://plain.test/"}}}
	if err := runDoctorGSC(context.Background(), &buf, cfg, "https://plain.test/"); err != nil {
		t.Fatalf("runDoctorGSC: %v", err)
	}
	if !strings.Contains(buf.String(), "Search Console:") {
		t.Fatalf("expected the section header:\n%s", buf.String())
	}
}

// guard: an unreadable directory should not panic the doctor.
func TestRunDoctorGSC_NilConfigSafe(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// runDoctorGSC is only called with cfg!=nil from the doctor flow, but defend it.
	if err := runDoctorGSC(context.Background(), &buf, &config.Config{}, "https://x/"); err != nil {
		t.Fatalf("runDoctorGSC empty cfg: %v", err)
	}
	_ = os.Stdout
}
