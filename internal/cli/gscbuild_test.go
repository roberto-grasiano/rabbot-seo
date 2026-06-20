package cli

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
)

// writeTestSAKey writes a syntactically valid service-account JSON key (with a
// freshly generated RSA key) to a file at the given mode and returns the path. When
// tokenURI is non-empty it is written as the key's token_uri so tests can point the
// jwt-bearer exchange at an httptest server (no live Google call); empty keeps the
// real Google endpoint (fine for tests that build the provider but never call Token).
func writeTestSAKey(t *testing.T, dir string, mode os.FileMode, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	sa := map[string]string{
		"type":         "service_account",
		"client_email": "robot@proj.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    tokenURI,
	}
	body, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal sa: %v", err)
	}
	path := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatalf("write sa key: %v", err)
	}
	return path
}

// TestLoadGSCSecret_ReadsAndRetightensPerms verifies the 0600 read+re-tighten
// discipline (the control/token.go pattern): a loosened secret file is read AND its
// permissions are clamped back to 0600 on read.
func TestLoadGSCSecret_ReadsAndRetightensPerms(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(path, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, mode, err := loadGSCSecret(path)
	if err != nil {
		t.Fatalf("loadGSCSecret: %v", err)
	}
	if string(data) != `{"k":"v"}` {
		t.Fatalf("content mismatch: %q", data)
	}
	// The mode REPORTED reflects what was found on disk before re-tightening, so the
	// doctor can warn; but the file on disk must now be 0600. (Unix file modes only —
	// Windows has no 0644/0600 distinction, so these perm assertions don't apply there.)
	if runtime.GOOS != "windows" {
		if mode.Perm() != 0o644 {
			t.Errorf("reported mode = %o, want the as-found 0644", mode.Perm())
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file perms after load = %o, want re-tightened 0600", fi.Mode().Perm())
		}
	}
}

// TestLoadGSCSecret_MissingFile returns a clear error (and never panics).
func TestLoadGSCSecret_MissingFile(t *testing.T) {
	t.Parallel()
	_, _, err := loadGSCSecret(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("want an error for a missing secret file")
	}
}

// TestProviderForSite_ServiceAccount builds an SA provider from a key file.
func TestProviderForSite_ServiceAccount(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := writeTestSAKey(t, dir, 0o600, "")

	gc := config.GSCConfig{
		Property:              "https://ex.com/",
		Auth:                  config.GSCAuthServiceAccount,
		ServiceAccountKeyFile: keyPath,
	}
	prov, err := providerForSite(context.Background(), gc)
	if err != nil {
		t.Fatalf("providerForSite: %v", err)
	}
	if prov.Mode() != "service_account" {
		t.Errorf("Mode() = %q, want service_account", prov.Mode())
	}
}

// TestProviderForSite_OAuth builds an OAuth provider from a persisted cred file
// carrying the BYO client creds + a refresh token.
func TestProviderForSite_OAuth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "oauth.json")
	cred := oauthCredFile{
		ClientID:     "client.apps.googleusercontent.com",
		ClientSecret: "shh",
		StoredToken:  gsc.StoredToken{RefreshToken: "refresh-xyz", AccessToken: "at", TokenType: "Bearer"},
	}
	body, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal cred: %v", err)
	}
	if err := os.WriteFile(tokenPath, body, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	gc := config.GSCConfig{
		Property:       "https://ex.com/",
		Auth:           config.GSCAuthOAuth2,
		OAuthTokenFile: tokenPath,
	}
	prov, err := providerForSite(context.Background(), gc)
	if err != nil {
		t.Fatalf("providerForSite: %v", err)
	}
	if prov.Mode() != "oauth" {
		t.Errorf("Mode() = %q, want oauth", prov.Mode())
	}
}

// TestProviderForSite_OAuthMissingRefreshToken is a clear, actionable error.
func TestProviderForSite_OAuthMissingRefreshToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "oauth.json")
	body, _ := json.Marshal(oauthCredFile{ClientID: "c", ClientSecret: "s"}) // no refresh token
	if err := os.WriteFile(tokenPath, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gc := config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthOAuth2, OAuthTokenFile: tokenPath}
	_, err := providerForSite(context.Background(), gc)
	if err == nil {
		t.Fatal("want an error when the persisted OAuth file has no refresh token")
	}
}

// TestProviderForSite_SecretNeverInError proves the loaded credential CONTENT never
// leaks into an error string. We craft a malformed SA key whose body contains a
// recognizable secret marker and assert the marker is absent from the error.
func TestProviderForSite_SecretNeverInError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "bad.json")
	const marker = "SUPER-SECRET-PRIVATE-KEY-MATERIAL"
	// Valid JSON, type=service_account, but a bogus PEM carrying the marker.
	body := `{"type":"service_account","client_email":"x@y.iam.gserviceaccount.com","private_key":"` + marker + `"}`
	if err := os.WriteFile(keyPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gc := config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: keyPath}
	_, err := providerForSite(context.Background(), gc)
	if err == nil {
		t.Fatal("want a parse error for the bogus key")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaked the private key material: %v", err)
	}
}

// TestBuildGSCPuller_NilWhenNoSiteConfigured pins severability: no GSC-configured
// site → nil puller (the feature is off, registerGSCPull registers nothing).
func TestBuildGSCPuller_NilWhenNoSiteConfigured(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sites: []config.SiteConfig{{URL: "https://plain.test/"}}}
	db := openCLITestStore(t)
	var mu sync.Mutex
	p := buildGSCPuller(&mu, cfg, db, nil)
	if p != nil {
		t.Fatalf("no GSC site must yield a nil puller, got %+v", p)
	}
}

// TestBuildGSCPuller_NonNilWhenConfigured builds a puller when at least one site has
// a GSC block, wiring the store + config.
func TestBuildGSCPuller_NonNilWhenConfigured(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://ex.com/",
		GSC: config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"},
	}}}
	db := openCLITestStore(t)
	var mu sync.Mutex
	p := buildGSCPuller(&mu, cfg, db, nil)
	if p == nil {
		t.Fatal("a configured GSC site must yield a non-nil puller")
	}
	if p.ResolveGSC == nil || p.Metrics == nil || p.Index == nil || p.Candidates == nil {
		t.Fatalf("puller not fully wired: %+v", p)
	}
	if p.API == nil || p.ProviderForSite == nil {
		t.Fatal("puller missing API/ProviderForSite factories")
	}
	// The resolver must reflect cfg and be guarded (a smoke call returns the block).
	if gc, ok := p.ResolveGSC("https://ex.com/"); !ok || gc.Property != "https://ex.com/" {
		t.Fatalf("resolver did not return the configured GSC block: %+v ok=%v", gc, ok)
	}
}

// TestParseOAuthCred_Malformed covers the JSON-decode error branch.
func TestParseOAuthCred_Malformed(t *testing.T) {
	t.Parallel()
	if _, err := parseOAuthCred([]byte(`{not json`)); err == nil {
		t.Error("parseOAuthCred must error on malformed JSON")
	}
}

// TestProviderForSite_OAuthMalformedFileErrors covers providerForSite's oauth
// parse-error branch: a present-but-corrupt token file surfaces a parse error.
func TestProviderForSite_OAuthMalformedFileErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "oauth.json")
	if err := os.WriteFile(tokenPath, []byte(`{ broken`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gc := config.GSCConfig{Property: "https://ex.com/", Auth: config.GSCAuthOAuth2, OAuthTokenFile: tokenPath}
	if _, err := providerForSite(context.Background(), gc); err == nil {
		t.Fatal("a corrupt oauth token file must surface a parse error")
	}
}

// TestProviderForSite_UnsupportedAuthErrors covers the default arm: an auth mode that
// validation would normally reject (here forced by a hand-edited config) fails clearly
// rather than silently.
func TestProviderForSite_UnsupportedAuthErrors(t *testing.T) {
	t.Parallel()
	gc := config.GSCConfig{Property: "https://ex.com/", Auth: "nonsense"}
	if _, err := providerForSite(context.Background(), gc); err == nil || !strings.Contains(err.Error(), "unsupported auth") {
		t.Fatalf("want an unsupported-auth error, got %v", err)
	}
}

// TestProductionGSCClient_BuildsClient covers the thin production factory: it builds a
// non-nil scheduler.GSCClient from a token provider (the real network client; we never
// call it, just prove the factory wires a client).
func TestProductionGSCClient_BuildsClient(t *testing.T) {
	t.Parallel()
	c, err := productionGSCClient(staticTokenProvider{})
	if err != nil {
		t.Fatalf("productionGSCClient: %v", err)
	}
	if c == nil {
		t.Fatal("productionGSCClient returned a nil client")
	}
}

// TestAnySiteHasGSC_NilAndEmpty covers the nil-config and no-GSC-site arms.
func TestAnySiteHasGSC_NilAndEmpty(t *testing.T) {
	t.Parallel()
	if anySiteHasGSC(nil) {
		t.Error("nil config must report no GSC site")
	}
	if anySiteHasGSC(&config.Config{Sites: []config.SiteConfig{{URL: "https://plain.test/"}}}) {
		t.Error("a config with no GSC block must report no GSC site")
	}
}
