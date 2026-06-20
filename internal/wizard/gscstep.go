package wizard

import (
	"errors"
	"fmt"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// The "Connect Search Console" step (after the alerts step in the wizard menu) joins
// a monitored site to its Google Search Console property so Rabbot can read Google's
// own ground truth — index status, the canonical Google actually chose, and search
// performance. It mirrors the alerts step exactly: a pure auth-mode enum with a
// single source-of-truth string↔value map shared by the Options builder and the
// Resolve parser (so the huh.Select and the parser never drift), plus pure
// collectors/validators that DELEGATE to config/gsc.go's validators so the wizard and
// the headless config path reject the same inputs. Every helper here is pure (no
// TTY); the huh.Select that collects the choice lives in internal/cli (the only
// untested seam), driven off these helpers.
//
// SECRET DISCIPLINE: the step collects only PATHS to the 0600 credential files
// (mirroring control.token, not the inline notifier secrets) — never a credential
// body. The two concrete modes write a config.GSCConfig whose *File field holds a
// path the daemon reads at runtime; the path strings are also control-plane
// non-settable (config/allowlist), so an agent cannot repoint Rabbot at an arbitrary
// credential file.

// GSCAuthMode is the operator's choice at the "Connect Search Console" step: connect
// via a service-account JSON key, connect via an OAuth2 installed-app refresh token,
// or skip (connect later). It is a pure enum so the routing/state decision is
// unit-testable independent of the huh.Select that collects it (mirrors
// AlertChannel).
type GSCAuthMode int

const (
	// GSCAuthUnset is the zero value: no choice has been made yet. It is NOT an
	// explicit terminal state — the cli loop re-prompts until the operator picks a
	// concrete option, so a silent skip is impossible (the alerts-step contract).
	GSCAuthUnset GSCAuthMode = iota
	// GSCAuthService connects via a GCP service-account JSON key (headless, no browser
	// flow — the default and "the main lift" the walkthrough centers on). Maps to
	// config.GSCAuthServiceAccount.
	GSCAuthService
	// GSCAuthOAuth connects via an OAuth2 installed-app refresh token captured by a
	// one-time `rabbot gsc auth` consent. Maps to config.GSCAuthOAuth2.
	GSCAuthOAuth
	// GSCAuthSkip is the deliberate "skip — connect Search Console later"
	// acknowledgment. It is an EXPLICIT terminal state the operator actively selects
	// (not a silent skip), and it is LOSSLESS: it writes no GSC block, so the site is
	// simply "not GSC-connected" (config.GSCConfig zero value) and can be connected
	// any time by re-running `rabbot init` or hand-editing config.yaml.
	GSCAuthSkip
)

// gscAuthValues maps the huh option string carried by the Select to a mode. It is the
// SINGLE source of truth shared by GSCAuthOptions (the menu) and ResolveGSCAuth (the
// reverse mapping), so the screen and the parser can never drift out of sync. The two
// concrete values intentionally equal config/gsc.go's exact seam strings; "skip" is
// the wizard-only terminal value.
var gscAuthValues = map[string]GSCAuthMode{
	config.GSCAuthServiceAccount: GSCAuthService, // "service_account"
	config.GSCAuthOAuth2:         GSCAuthOAuth,   // "oauth2"
	"skip":                       GSCAuthSkip,
}

// GSCAuthOption is one selectable row of the step: a friendly Label and the stable
// Value the huh.Select carries (and that ResolveGSCAuth maps back).
type GSCAuthOption struct {
	Label string
	Value string
}

// gscAuthOrder fixes the menu order: service-account FIRST (the headless default and
// the walkthrough's main lift), then OAuth2, then the explicit lossless skip — so
// GSCAuthOptions is deterministic rather than ranging a map.
var gscAuthOrder = []GSCAuthOption{
	{Label: "Service account (recommended) — a GCP JSON key, no browser; best for a server", Value: config.GSCAuthServiceAccount},
	{Label: "OAuth2 — a one-time browser consent (run `rabbot gsc auth`); for a property you own", Value: config.GSCAuthOAuth2},
	{Label: "Skip — connect Search Console later", Value: "skip"},
}

// GSCAuthOptions returns the auth-mode options in menu order. The cli huh.Select
// builds its options from this list and feeds the chosen Value back to
// ResolveGSCAuth, so the two stay in lockstep. It returns a defensive copy so a
// caller mutating the slice cannot corrupt the canonical order.
func GSCAuthOptions() []GSCAuthOption {
	out := make([]GSCAuthOption, len(gscAuthOrder))
	copy(out, gscAuthOrder)
	return out
}

// ResolveGSCAuth maps a choice string (the value carried by the huh.Select option) to
// a GSCAuthMode, returning an error for an unknown choice. Keeping this pure lets the
// cli layer drive the production huh.Select while tests assert the mapping directly.
// Note "oauth" (without the 2) is rejected — the seam value is "oauth2", matching
// config/gsc.go.
func ResolveGSCAuth(s string) (GSCAuthMode, error) {
	if m, ok := gscAuthValues[s]; ok {
		return m, nil
	}
	return GSCAuthUnset, fmt.Errorf("wizard: unknown Search Console auth choice %q", s)
}

// IsExplicit reports whether this mode represents a deliberate choice that ends the
// step. Every concrete option — INCLUDING the lossless skip (GSCAuthSkip) — is
// explicit; only GSCAuthUnset is not, which forces the cli loop to re-prompt (the
// alerts-step anti-silent-skip rule).
func (m GSCAuthMode) IsExplicit() bool {
	switch m {
	case GSCAuthService, GSCAuthOAuth, GSCAuthSkip:
		return true
	default:
		return false
	}
}

// IsConnect reports whether this mode actually connects a property (and therefore
// needs a property + credential path collected and a GSC block written). The skip and
// unset modes return false — the caller writes no block for them.
func (m GSCAuthMode) IsConnect() bool {
	return m == GSCAuthService || m == GSCAuthOAuth
}

// ConfigValue returns the config.yaml `gsc.auth` string this mode writes — the settled
// seam contract: "service_account" or "oauth2". GSCAuthSkip (and GSCAuthUnset) write
// no block and return "".
func (m GSCAuthMode) ConfigValue() string {
	switch m {
	case GSCAuthService:
		return config.GSCAuthServiceAccount
	case GSCAuthOAuth:
		return config.GSCAuthOAuth2
	default:
		return ""
	}
}

// errGSCNotConnect is returned by BuildGSCConfig when asked to assemble a block for a
// non-connect mode (skip/unset). It lets the caller distinguish "operator chose not to
// connect" (skip the write) from a real validation failure.
var errGSCNotConnect = errors.New("wizard: Search Console auth mode is not a connect (skip/unset writes no block)")

// ValidateGSCPropertyField is the wizard property-field validator. It DELEGATES to
// config/gsc.go's property validator (via ValidateGSC against a minimal synthetic
// site) so the step accepts EXACTLY the two GSC property forms the loader accepts — a
// URL-prefix property ("https://ex.com/") or a Domain property ("sc-domain:ex.com") —
// and rejects everything else with the same friendly message. Naming the wizard
// function here keeps it unit-tested alongside validateContact / validateSite.
func ValidateGSCPropertyField(property string) error {
	// Drive the property through the real config validator with a placeholder
	// credential so only the property check can fire (the mode↔credential checks are
	// satisfied). ValidateGSC annotates with the site URL; the property message is the
	// signal we surface.
	err := config.ValidateGSC([]config.SiteConfig{{
		URL: "https://placeholder.example",
		GSC: config.GSCConfig{
			Property:              property,
			Auth:                  config.GSCAuthServiceAccount,
			ServiceAccountKeyFile: "placeholder",
		},
	}})
	return err
}

// ValidateGSCKeyFileField is the wizard credential-path validator. The credential
// lives in a 0600 FILE referenced BY PATH (never inline), so the step collects only a
// path; this checks PRESENCE — a non-empty path — and never reads, opens, or echoes
// the file body. (Existence/0600 are verified later by the doctor GSC check, which the
// step offers to run.) The path may carry an ${ENV} reference, which config.Load
// interpolates, so it is accepted verbatim.
func ValidateGSCKeyFileField(path string) error {
	if path == "" {
		return errors.New("a credential file path is required (the path to your 0600 service-account key or OAuth token file)")
	}
	return nil
}

// BuildGSCConfig assembles the per-site config.GSCConfig the step attaches to the
// SiteConfig being built, from the chosen mode, the property, and the single
// credential PATH (the service-account key for GSCAuthService, the OAuth token file
// for GSCAuthOAuth — only ONE is ever set, honoring the mutual-exclusion the loader
// enforces). It validates the assembled block through the REAL config validator
// (config.ValidateGSC) so a block the wizard writes is exactly a block the daemon
// loads — property shape, mode↔credential match, and mutual exclusion all checked by
// the single source of truth.
//
// For a non-connect mode (skip/unset) it returns errGSCNotConnect WITHOUT a block, so
// the caller writes nothing. It never logs or echoes the credential path body.
func BuildGSCConfig(mode GSCAuthMode, property, credPath string) (config.GSCConfig, error) {
	if !mode.IsConnect() {
		return config.GSCConfig{}, errGSCNotConnect
	}
	g := config.GSCConfig{
		Property: property,
		Auth:     mode.ConfigValue(),
	}
	switch mode {
	case GSCAuthService:
		g.ServiceAccountKeyFile = credPath
	case GSCAuthOAuth:
		g.OAuthTokenFile = credPath
	}
	// Validate via the real config validator so the wizard and the loader agree
	// exactly (property shape + mode↔credential mutual-exclusion). The site URL here
	// is only for the validator's error annotation.
	if err := config.ValidateGSC([]config.SiteConfig{{URL: "https://placeholder.example", GSC: g}}); err != nil {
		return config.GSCConfig{}, err
	}
	return g, nil
}
