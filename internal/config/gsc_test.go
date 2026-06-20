package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// baseGSCConfig returns a Config that already satisfies the mandatory
// contact-email gate so a test can mutate only the GSC surface and exercise
// Validate() without tripping an unrelated error.
func baseGSCConfig() Config {
	c := Defaults()
	c.Crawler.ContactEmail = "ops@example.com"
	return c
}

// withGSCSite returns baseGSCConfig with a single site carrying the given GSC
// block, so the per-site validation and accessor paths are exercised through the
// real Sites slice (not just a bare struct).
func withGSCSite(g GSCConfig) Config {
	c := baseGSCConfig()
	c.Sites = []SiteConfig{{URL: "https://example.com", Name: "Example", GSC: g}}
	return c
}

// TestGSCDefaultsAbsent pins that GSC is OPT-IN per site: Defaults() carries no
// site, and a freshly-defaulted config validates (the GSC block is the zero value
// — unconfigured — and must be a no-op, never a hard error).
func TestGSCDefaultsAbsent(t *testing.T) {
	c := baseGSCConfig()
	if len(c.Sites) != 0 {
		t.Fatalf("Defaults() seeded %d sites, want 0", len(c.Sites))
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default config (no GSC) should validate: %v", err)
	}
	// A site with an entirely empty GSC block is unconfigured and must validate.
	if err := withGSCSite(GSCConfig{}).Validate(); err != nil {
		t.Fatalf("site with empty GSC block should validate (opt-in): %v", err)
	}
}

// TestGSCConfigFieldSettable pins the struct shape: every advertised field is
// settable/readable on SiteConfig.GSC (a compile + round-trip guard so the
// koanf/yaml tags and field names cannot silently drift from the contract).
func TestGSCConfigFieldSettable(t *testing.T) {
	var s SiteConfig
	s.GSC.Property = "https://example.com/"
	s.GSC.Auth = "service_account"
	s.GSC.ServiceAccountKeyFile = "/etc/rabbot/gsc-sa.json"
	s.GSC.OAuthTokenFile = ""
	if s.GSC.Property != "https://example.com/" ||
		s.GSC.Auth != "service_account" ||
		s.GSC.ServiceAccountKeyFile != "/etc/rabbot/gsc-sa.json" {
		t.Fatalf("SiteConfig.GSC not settable/readable: %+v", s.GSC)
	}
}

// TestGSCValidProperties walks the two accepted GSC property forms (URL-prefix
// and sc-domain:) under both auth modes, asserting each well-formed combination
// passes Validate(). These are the shapes the puller will hand to the GSC client.
func TestGSCValidProperties(t *testing.T) {
	tests := []struct {
		name string
		g    GSCConfig
	}{
		{"url-prefix https + service_account", GSCConfig{
			Property: "https://example.com/", Auth: "service_account",
			ServiceAccountKeyFile: "/keys/sa.json",
		}},
		{"url-prefix http + service_account", GSCConfig{
			Property: "http://example.com/", Auth: "service_account",
			ServiceAccountKeyFile: "/keys/sa.json",
		}},
		{"url-prefix with path + oauth2", GSCConfig{
			Property: "https://example.com/blog/", Auth: "oauth2",
			OAuthTokenFile: "/keys/tok.json",
		}},
		{"sc-domain + service_account", GSCConfig{
			Property: "sc-domain:example.com", Auth: "service_account",
			ServiceAccountKeyFile: "/keys/sa.json",
		}},
		{"sc-domain + oauth2", GSCConfig{
			Property: "sc-domain:brand.co.uk", Auth: "oauth2",
			OAuthTokenFile: "/keys/tok.json",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := withGSCSite(tc.g).Validate(); err != nil {
				t.Errorf("Validate() rejected a well-formed GSC block: %v", err)
			}
		})
	}
}

// TestGSCInvalidProperty rejects property identifiers that are neither a URL-prefix
// (http/https scheme) nor an sc-domain: token. A bare host, an sc-domain: with an
// empty domain, or a non-http scheme must fail with a property-format error.
func TestGSCInvalidProperty(t *testing.T) {
	tests := []struct {
		name     string
		property string
	}{
		{"bare host", "example.com"},
		{"missing scheme", "//example.com/"},
		{"ftp scheme", "ftp://example.com/"},
		{"sc-domain empty", "sc-domain:"},
		{"sc-domain with scheme", "sc-domain:https://example.com"},
		{"empty but auth set", ""}, // property is required once the block is active
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := GSCConfig{
				Property: tc.property, Auth: "service_account",
				ServiceAccountKeyFile: "/keys/sa.json",
			}
			err := withGSCSite(g).Validate()
			if err == nil {
				t.Fatalf("Validate() accepted bad GSC property %q; want error", tc.property)
			}
			if !strings.Contains(err.Error(), "gsc") {
				t.Errorf("error %q should mention gsc", err.Error())
			}
		})
	}
}

// TestGSCAuthModeRequired rejects an active GSC block (property set) whose auth
// mode is empty or not one of the two supported values.
func TestGSCAuthModeRequired(t *testing.T) {
	tests := []struct {
		name string
		auth string
	}{
		{"empty auth", ""},
		{"unknown auth", "api_key"},
		{"oauth not oauth2", "oauth"}, // the contract's W2 key is exactly "oauth2"
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := GSCConfig{
				Property: "https://example.com/", Auth: tc.auth,
				ServiceAccountKeyFile: "/keys/sa.json",
				OAuthTokenFile:        "/keys/tok.json",
			}
			if err := withGSCSite(g).Validate(); err == nil {
				t.Fatalf("Validate() accepted GSC auth %q; want error", tc.auth)
			}
		})
	}
}

// TestGSCServiceAccountMutualExclusion pins the credential-reference rules for the
// service_account mode: the key file is REQUIRED and the oauth token file must be
// ABSENT (the two credential references are mutually exclusive per auth mode).
func TestGSCServiceAccountMutualExclusion(t *testing.T) {
	t.Run("key file required", func(t *testing.T) {
		g := GSCConfig{Property: "https://example.com/", Auth: "service_account"}
		if err := withGSCSite(g).Validate(); err == nil {
			t.Fatal("service_account with no key file should fail Validate()")
		}
	})
	t.Run("oauth token file forbidden", func(t *testing.T) {
		g := GSCConfig{
			Property: "https://example.com/", Auth: "service_account",
			ServiceAccountKeyFile: "/keys/sa.json",
			OAuthTokenFile:        "/keys/tok.json", // wrong mode → mutually exclusive
		}
		if err := withGSCSite(g).Validate(); err == nil {
			t.Fatal("service_account with an oauth_token_file set should fail (mutually exclusive)")
		}
	})
}

// TestGSCOAuthMutualExclusion is the mirror for the oauth2 mode: the token file is
// REQUIRED and the service-account key file must be ABSENT.
func TestGSCOAuthMutualExclusion(t *testing.T) {
	t.Run("token file required", func(t *testing.T) {
		g := GSCConfig{Property: "https://example.com/", Auth: "oauth2"}
		if err := withGSCSite(g).Validate(); err == nil {
			t.Fatal("oauth2 with no token file should fail Validate()")
		}
	})
	t.Run("service-account key file forbidden", func(t *testing.T) {
		g := GSCConfig{
			Property: "https://example.com/", Auth: "oauth2",
			OAuthTokenFile:        "/keys/tok.json",
			ServiceAccountKeyFile: "/keys/sa.json", // wrong mode → mutually exclusive
		}
		if err := withGSCSite(g).Validate(); err == nil {
			t.Fatal("oauth2 with a service_account_key_file set should fail (mutually exclusive)")
		}
	})
}

// TestGSCForBaseURL pins the accessor that bridges a DB site (model.Site, keyed by
// BaseURL) to its per-site GSC config: an exact URL match returns the block + true;
// a site with no GSC block returns ok=false; an unknown URL returns ok=false.
func TestGSCForBaseURL(t *testing.T) {
	c := baseGSCConfig()
	c.Sites = []SiteConfig{
		{URL: "https://example.com", GSC: GSCConfig{
			Property: "https://example.com/", Auth: "service_account",
			ServiceAccountKeyFile: "/keys/sa.json",
		}},
		{URL: "https://nogsc.example"}, // no GSC block
	}
	t.Run("match returns block + true", func(t *testing.T) {
		g, ok := c.GSCForBaseURL("https://example.com")
		if !ok {
			t.Fatal("GSCForBaseURL(configured site) ok = false, want true")
		}
		if g.Property != "https://example.com/" || g.Auth != "service_account" {
			t.Errorf("returned GSC block = %+v, want the configured values", g)
		}
	})
	t.Run("site without GSC returns false", func(t *testing.T) {
		if _, ok := c.GSCForBaseURL("https://nogsc.example"); ok {
			t.Error("GSCForBaseURL(site without GSC) ok = true, want false")
		}
	})
	t.Run("unknown URL returns false", func(t *testing.T) {
		if _, ok := c.GSCForBaseURL("https://absent.example"); ok {
			t.Error("GSCForBaseURL(unknown URL) ok = true, want false")
		}
	})
}

// TestGSCConfigured pins the IsConfigured predicate used to decide whether a GSC
// block is active (so the puller/doctor can cheaply skip unconfigured sites).
func TestGSCConfigured(t *testing.T) {
	if (GSCConfig{}).IsConfigured() {
		t.Error("empty GSCConfig.IsConfigured() = true, want false")
	}
	g := GSCConfig{Property: "sc-domain:example.com", Auth: "oauth2", OAuthTokenFile: "/k/t"}
	if !g.IsConfigured() {
		t.Error("populated GSCConfig.IsConfigured() = false, want true")
	}
}

// --- load / precedence / interpolation ------------------------------------

// TestGSCLoadFromYAML is the end-to-end load: a config.yaml carrying a per-site
// GSC block round-trips into SiteConfig.GSC with every field intact, and the
// resulting config validates.
func TestGSCLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
sites:
  - url: https://example.com
    name: Example
    gsc:
      property: https://example.com/
      auth: service_account
      service_account_key_file: /etc/rabbot/gsc-sa.json
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(c.Sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(c.Sites))
	}
	g := c.Sites[0].GSC
	if g.Property != "https://example.com/" {
		t.Errorf("gsc.property = %q, want https://example.com/", g.Property)
	}
	if g.Auth != "service_account" {
		t.Errorf("gsc.auth = %q, want service_account", g.Auth)
	}
	if g.ServiceAccountKeyFile != "/etc/rabbot/gsc-sa.json" {
		t.Errorf("gsc.service_account_key_file = %q, want the configured path", g.ServiceAccountKeyFile)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("loaded GSC config should validate: %v", err)
	}
}

// TestGSCInterpolatesCredentialPath is the secret-handling round-trip: the
// credential-PATH fields carry ${ENV} references (so the path — never the key body
// — can live outside config.yaml) and Load resolves them against the environment
// by the time it returns, exactly like the existing access/notifier secrets.
//
// The CONTENT of the credential is read at runtime from the file path (W2), never
// stored inline; this test only proves the path string interpolates.
func TestGSCInterpolatesCredentialPath(t *testing.T) {
	t.Setenv("RABBOT_GSC_SA_PATH", "/run/secrets/gsc-sa.json")
	t.Setenv("RABBOT_GSC_TOKEN_PATH", "/run/secrets/gsc-token.json")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
sites:
  - url: https://sa.example
    name: SA
    gsc:
      property: https://sa.example/
      auth: service_account
      service_account_key_file: ${RABBOT_GSC_SA_PATH}
  - url: https://oauth.example
    name: OAuth
    gsc:
      property: sc-domain:oauth.example
      auth: oauth2
      oauth_token_file: ${RABBOT_GSC_TOKEN_PATH}
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(c.Sites) != 2 {
		t.Fatalf("got %d sites, want 2", len(c.Sites))
	}
	if got := c.Sites[0].GSC.ServiceAccountKeyFile; got != "/run/secrets/gsc-sa.json" {
		t.Errorf("service_account_key_file = %q, want interpolated path", got)
	}
	if got := c.Sites[1].GSC.OAuthTokenFile; got != "/run/secrets/gsc-token.json" {
		t.Errorf("oauth_token_file = %q, want interpolated path", got)
	}
}

// TestGSCLoadPrecedenceFlagsOverFile pins the koanf precedence story for a
// top-level-addressable secret path: an env-resolved ${VAR} placed in the file is
// the documented way to supply the path (sites.* is not array-addressable via
// RABBOT_ env), and an unset ${VAR} resolves to empty (os.Expand semantics) rather
// than the literal placeholder.
func TestGSCLoadPrecedenceUnsetEnvToEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `crawler:
  contact_email: ops@example.com
sites:
  - url: https://example.com
    name: Example
    gsc:
      property: https://example.com/
      auth: service_account
      service_account_key_file: ${DEFINITELY_UNSET_GSC_KEY_PATH}
`)
	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := c.Sites[0].GSC.ServiceAccountKeyFile; got != "" {
		t.Errorf("service_account_key_file = %q, want empty string for unset env var", got)
	}
}

// TestGSCNotControlSettable pins the secret-adjacency rule: the credential-PATH
// keys are secret-adjacent/floor and must NEVER be settable over the control plane
// (an agent must not be able to repoint Rabbot at an arbitrary credential file).
// They are not on the allow-list, so AllowConfigKey rejects them.
func TestGSCNotControlSettable(t *testing.T) {
	keys := []string{
		"sites.0.gsc.service_account_key_file",
		"sites.0.gsc.oauth_token_file",
		"gsc.service_account_key_file",
		"gsc.oauth_token_file",
	}
	for _, k := range keys {
		if err := AllowConfigKey(k); err == nil {
			t.Errorf("AllowConfigKey(%q) = nil; GSC credential paths must be control-plane rejected", k)
		}
	}
}
