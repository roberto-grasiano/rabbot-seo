package wizard

import (
	"errors"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// The "Connect Search Console" step mirrors the alerts step: a pure auth-mode enum
// with a single source-of-truth string↔value map shared by the Options builder and
// the Resolve parser, plus pure validators that wrap config/gsc.go's validators so
// the wizard and the headless config path reject the same inputs. Every helper is
// pure (no TTY), so it is unit-tested directly; the huh.Select that collects the
// choice in cli is the only untested seam.

func TestGSCAuthOptionsOrderAndCoverage(t *testing.T) {
	opts := GSCAuthOptions()
	// Service-account first — it is the headless default and "the main lift" the
	// walkthrough centers on; OAuth2 second; an explicit lossless skip last.
	want := []string{"service_account", "oauth2", "skip"}
	if len(opts) != len(want) {
		t.Fatalf("GSCAuthOptions length = %d, want %d (%v)", len(opts), len(want), opts)
	}
	for i, w := range want {
		if opts[i].Value != w {
			t.Errorf("GSCAuthOptions[%d].Value = %q, want %q", i, opts[i].Value, w)
		}
		if strings.TrimSpace(opts[i].Label) == "" {
			t.Errorf("GSCAuthOptions[%d].Label is empty", i)
		}
	}
}

func TestGSCAuthOptionsIsACopy(t *testing.T) {
	// Mutating the returned slice must not corrupt the canonical order (the alerts
	// step's defensive copy contract).
	a := GSCAuthOptions()
	if len(a) == 0 {
		t.Fatal("GSCAuthOptions returned no options")
	}
	a[0].Label = "MUTATED"
	b := GSCAuthOptions()
	if b[0].Label == "MUTATED" {
		t.Fatal("GSCAuthOptions returned a shared backing array; want a copy")
	}
}

func TestResolveGSCAuthValid(t *testing.T) {
	cases := map[string]GSCAuthMode{
		"service_account": GSCAuthService,
		"oauth2":          GSCAuthOAuth,
		"skip":            GSCAuthSkip,
	}
	for in, want := range cases {
		got, err := ResolveGSCAuth(in)
		if err != nil {
			t.Errorf("ResolveGSCAuth(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveGSCAuth(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveGSCAuthUnknown(t *testing.T) {
	// "oauth" (no 2) and a bare unknown must be rejected — they are NOT the seam's
	// exact strings (config/gsc.go rejects "oauth" too).
	for _, in := range []string{"", "oauth", "service-account", "sa", "bogus"} {
		if _, err := ResolveGSCAuth(in); err == nil {
			t.Errorf("ResolveGSCAuth(%q) = nil error, want unknown-choice error", in)
		}
	}
}

func TestResolveGSCAuthMatchesConfigStrings(t *testing.T) {
	// The two concrete modes MUST map to config/gsc.go's exact seam strings — the
	// values W2 keys on. A drift here would write an auth value the loader rejects.
	if got := GSCAuthService.ConfigValue(); got != config.GSCAuthServiceAccount {
		t.Errorf("GSCAuthService.ConfigValue() = %q, want %q", got, config.GSCAuthServiceAccount)
	}
	if got := GSCAuthOAuth.ConfigValue(); got != config.GSCAuthOAuth2 {
		t.Errorf("GSCAuthOAuth.ConfigValue() = %q, want %q", got, config.GSCAuthOAuth2)
	}
	if got := GSCAuthSkip.ConfigValue(); got != "" {
		t.Errorf("GSCAuthSkip.ConfigValue() = %q, want empty (skip writes no block)", got)
	}
}

func TestGSCAuthModeIsConnect(t *testing.T) {
	// Skip is an EXPLICIT terminal state (lossless), but it is NOT a connect — it
	// writes no GSC block. The two concrete modes ARE a connect.
	if !GSCAuthService.IsConnect() || !GSCAuthOAuth.IsConnect() {
		t.Error("service_account / oauth2 must report IsConnect() == true")
	}
	if GSCAuthSkip.IsConnect() {
		t.Error("skip must report IsConnect() == false (it writes no block)")
	}
	if GSCAuthUnset.IsConnect() {
		t.Error("unset must report IsConnect() == false")
	}
}

func TestGSCAuthModeIsExplicit(t *testing.T) {
	// Every concrete choice — including the deliberate skip — is explicit; only the
	// zero value forces a re-prompt (the alerts-step anti-silent-skip contract).
	for _, m := range []GSCAuthMode{GSCAuthService, GSCAuthOAuth, GSCAuthSkip} {
		if !m.IsExplicit() {
			t.Errorf("%v.IsExplicit() = false, want true", m)
		}
	}
	if GSCAuthUnset.IsExplicit() {
		t.Error("GSCAuthUnset.IsExplicit() = true, want false")
	}
}

func TestValidateGSCPropertyField(t *testing.T) {
	good := []string{
		"https://example.com/",
		"http://example.com/",
		"https://shop.example.com/path/",
		"sc-domain:whatthehellai.com",
		"sc-domain:example.co.uk",
	}
	for _, p := range good {
		if err := ValidateGSCPropertyField(p); err != nil {
			t.Errorf("ValidateGSCPropertyField(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"",                            // required
		"example.com",                 // no scheme, not sc-domain:
		"ftp://example.com/",          // wrong scheme
		"sc-domain:",                  // empty domain
		"sc-domain:https://x.com",     // scheme inside sc-domain
		"sc-domain:example.com/path",  // slash inside sc-domain
		"sc-domain:example.com extra", // space inside sc-domain
	}
	for _, p := range bad {
		if err := ValidateGSCPropertyField(p); err == nil {
			t.Errorf("ValidateGSCPropertyField(%q) = nil, want an error", p)
		}
	}
}

func TestValidateGSCPropertyFieldDelegatesToConfig(t *testing.T) {
	// The wizard validator must accept EXACTLY what config.ValidateGSC accepts, so the
	// step and the loader never disagree. Drive a representative value through both.
	const p = "sc-domain:whatthehellai.com"
	if err := ValidateGSCPropertyField(p); err != nil {
		t.Fatalf("wizard accepts %q? got %v", p, err)
	}
	cfgErr := config.ValidateGSC([]config.SiteConfig{{
		URL: "https://whatthehellai.com",
		GSC: config.GSCConfig{Property: p, Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/etc/rabbot/sa.json"},
	}})
	if cfgErr != nil {
		t.Fatalf("config rejects a value the wizard accepts: %v", cfgErr)
	}
}

func TestValidateGSCKeyFileField(t *testing.T) {
	// A path is required (the credential lives in a 0600 FILE referenced by path);
	// the validator checks PRESENCE only, never reads or echoes the body.
	if err := ValidateGSCKeyFileField(""); err == nil {
		t.Error("ValidateGSCKeyFileField(\"\") = nil, want a required-path error")
	}
	for _, p := range []string{"/etc/rabbot/sa.json", "sa-key.json", "${RABBOT_GSC_KEY}"} {
		if err := ValidateGSCKeyFileField(p); err != nil {
			t.Errorf("ValidateGSCKeyFileField(%q) = %v, want nil", p, err)
		}
	}
}

func TestBuildGSCConfigServiceAccount(t *testing.T) {
	g, err := BuildGSCConfig(GSCAuthService, "sc-domain:whatthehellai.com", "/etc/rabbot/sa.json")
	if err != nil {
		t.Fatalf("BuildGSCConfig service_account: %v", err)
	}
	if g.Auth != config.GSCAuthServiceAccount {
		t.Errorf("Auth = %q, want %q", g.Auth, config.GSCAuthServiceAccount)
	}
	if g.Property != "sc-domain:whatthehellai.com" {
		t.Errorf("Property = %q", g.Property)
	}
	if g.ServiceAccountKeyFile != "/etc/rabbot/sa.json" {
		t.Errorf("ServiceAccountKeyFile = %q", g.ServiceAccountKeyFile)
	}
	if g.OAuthTokenFile != "" {
		t.Errorf("OAuthTokenFile = %q, want empty (mutually exclusive)", g.OAuthTokenFile)
	}
	// The assembled block must pass the real config validator (mode↔credential
	// mutual-exclusion, property shape).
	if err := config.ValidateGSC([]config.SiteConfig{{URL: "https://x.example", GSC: g}}); err != nil {
		t.Errorf("BuildGSCConfig produced a block config rejects: %v", err)
	}
}

func TestBuildGSCConfigOAuth(t *testing.T) {
	g, err := BuildGSCConfig(GSCAuthOAuth, "https://example.com/", "/home/op/.config/rabbot/gsc-oauth.json")
	if err != nil {
		t.Fatalf("BuildGSCConfig oauth2: %v", err)
	}
	if g.Auth != config.GSCAuthOAuth2 {
		t.Errorf("Auth = %q, want %q", g.Auth, config.GSCAuthOAuth2)
	}
	if g.OAuthTokenFile != "/home/op/.config/rabbot/gsc-oauth.json" {
		t.Errorf("OAuthTokenFile = %q", g.OAuthTokenFile)
	}
	if g.ServiceAccountKeyFile != "" {
		t.Errorf("ServiceAccountKeyFile = %q, want empty (mutually exclusive)", g.ServiceAccountKeyFile)
	}
	if err := config.ValidateGSC([]config.SiteConfig{{URL: "https://x.example", GSC: g}}); err != nil {
		t.Errorf("BuildGSCConfig produced a block config rejects: %v", err)
	}
}

func TestBuildGSCConfigRejectsBadProperty(t *testing.T) {
	if _, err := BuildGSCConfig(GSCAuthService, "not-a-property", "/etc/rabbot/sa.json"); err == nil {
		t.Error("BuildGSCConfig with a malformed property = nil error, want a validation error")
	}
}

func TestBuildGSCConfigRejectsMissingCredential(t *testing.T) {
	if _, err := BuildGSCConfig(GSCAuthService, "https://example.com/", ""); err == nil {
		t.Error("BuildGSCConfig service_account with no key path = nil error, want an error")
	}
	if _, err := BuildGSCConfig(GSCAuthOAuth, "https://example.com/", ""); err == nil {
		t.Error("BuildGSCConfig oauth2 with no token path = nil error, want an error")
	}
}

func TestBuildGSCConfigRejectsSkipAndUnset(t *testing.T) {
	// Skip / unset are not connect modes — BuildGSCConfig must refuse to fabricate a
	// block for them (the caller skips the write entirely).
	for _, m := range []GSCAuthMode{GSCAuthSkip, GSCAuthUnset} {
		if _, err := BuildGSCConfig(m, "https://example.com/", "/etc/rabbot/sa.json"); err == nil {
			t.Errorf("BuildGSCConfig(%v, …) = nil error, want a refusal (not a connect mode)", m)
		}
	}
}

func TestGSCStepErrorsAreFriendly(t *testing.T) {
	// Errors must never leak a credential body — only the path/identifier is named.
	// (Defensive: these validators only ever receive a path string, but assert the
	// contract holds so a future change can't regress it.)
	_, err := BuildGSCConfig(GSCAuthService, "not-a-property", "/etc/rabbot/sa.json")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, errGSCNotConnect) && !strings.Contains(err.Error(), "property") {
		t.Errorf("error %q should name the offending property field", err)
	}
}
