package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// GSCConfig is the per-site Google Search Console block in config.yaml. It joins a
// monitored site to its GSC property and the credentials Rabbot uses to read it.
//
// Property is the GSC property identifier and is one of exactly two forms:
//   - a URL-prefix property: "https://example.com/" (an http/https URL), or
//   - a Domain property:     "sc-domain:example.com".
//
// Auth selects the credential model (both are first-class, owner-locked):
//   - "service_account" — a GCP service-account JSON key (SA-JWT, headless), read
//     from ServiceAccountKeyFile.
//   - "oauth2"          — an OAuth2 installed-app refresh token captured by a
//     one-time consent, read from OAuthTokenFile.
//
// SECRET HANDLING — the credentials live in 0600 FILES referenced BY PATH, never
// inline (mirroring control.token, not the inline notifier secrets). The two *File
// fields hold only a path; the credential CONTENT (the JSON key body, the private
// RSA key, the refresh/access tokens) is read at runtime from that path and is
// NEVER stored in config, logged, echoed into errors, or returned by `config get`.
// The path strings MAY carry a ${ENV} reference, which config.Load interpolates
// (interpolateSecrets) so the path can live outside config.yaml — only the path is
// interpolated, never the credential body. The credential-path keys are also
// secret-adjacent and are deliberately NOT settable over the control plane
// (allowlist.go): an agent must not be able to repoint Rabbot at an arbitrary
// credential file.
//
// The scope is webmasters.readonly (W2/the gsc client concern). A site that omits
// the block (the zero value) is simply not GSC-connected — GSC is opt-in per site.
type GSCConfig struct {
	Property              string `koanf:"property"                 yaml:"property,omitempty"`                 // "https://ex.com/" OR "sc-domain:ex.com"
	Auth                  string `koanf:"auth"                     yaml:"auth,omitempty"`                     // "service_account" | "oauth2"
	ServiceAccountKeyFile string `koanf:"service_account_key_file" yaml:"service_account_key_file,omitempty"` // path to a 0600 JSON key (secret by path)
	OAuthTokenFile        string `koanf:"oauth_token_file"         yaml:"oauth_token_file,omitempty"`         // path to a 0600 refresh-token file (secret by path)
}

// The two supported auth modes. These exact strings are the seam W2 keys on, so
// they are validated strictly here (an empty or unknown value is rejected, and
// "oauth" is NOT accepted — the value is "oauth2").
const (
	GSCAuthServiceAccount = "service_account"
	GSCAuthOAuth2         = "oauth2"
)

// gscDomainPrefix is the Domain-property scheme marker ("sc-domain:example.com").
const gscDomainPrefix = "sc-domain:"

// IsConfigured reports whether the site has opted into GSC. A block is active once
// any field is set; an entirely zero block means "not GSC-connected" and is a
// validation no-op. The puller and the doctor check use this to cheaply skip
// unconfigured sites.
func (g GSCConfig) IsConfigured() bool {
	return g.Property != "" || g.Auth != "" ||
		g.ServiceAccountKeyFile != "" || g.OAuthTokenFile != ""
}

// GSCForBaseURL returns the per-site GSCConfig for the SiteConfig whose URL matches
// baseURL and whether that site has an ACTIVE (configured) GSC block. It mirrors
// AccessForBaseURL and is the seam that bridges a DB site (model.Site, keyed by
// BaseURL) to its GSC config, since model.Site carries no GSC field. ok is false
// when no site matches baseURL OR the matched site has no GSC block, so a caller
// can branch on a single boolean.
func (c *Config) GSCForBaseURL(baseURL string) (GSCConfig, bool) {
	for _, s := range c.Sites {
		if s.URL == baseURL {
			return s.GSC, s.GSC.IsConfigured()
		}
	}
	return GSCConfig{}, false
}

// ValidateGSC validates the GSC block of every site. An unconfigured (zero) block
// is skipped; an active block must satisfy:
//   - Property is present and a well-formed URL-prefix (http/https) or sc-domain:
//     identifier;
//   - Auth is exactly "service_account" or "oauth2";
//   - the credential reference matches the mode and the two references are mutually
//     exclusive: service_account requires ServiceAccountKeyFile and forbids
//     OAuthTokenFile; oauth2 requires OAuthTokenFile and forbids
//     ServiceAccountKeyFile.
//
// Errors name the offending site URL (a non-secret identifier) for a clear
// operator message; the credential PATHS and CONTENT are never included.
func ValidateGSC(sites []SiteConfig) error {
	for _, s := range sites {
		if !s.GSC.IsConfigured() {
			continue
		}
		if err := s.GSC.validate(); err != nil {
			return fmt.Errorf("rabbot: site %q gsc: %w", s.URL, err)
		}
	}
	return nil
}

// validate checks one active GSC block. It is unexported; callers go through
// ValidateGSC (which annotates with the site URL) or Config.Validate.
func (g GSCConfig) validate() error {
	if err := validateGSCProperty(g.Property); err != nil {
		return err
	}
	switch g.Auth {
	case GSCAuthServiceAccount:
		if g.ServiceAccountKeyFile == "" {
			return errors.New("auth service_account requires service_account_key_file")
		}
		if g.OAuthTokenFile != "" {
			return errors.New("auth service_account must not set oauth_token_file (mutually exclusive with service_account_key_file)")
		}
	case GSCAuthOAuth2:
		if g.OAuthTokenFile == "" {
			return errors.New("auth oauth2 requires oauth_token_file")
		}
		if g.ServiceAccountKeyFile != "" {
			return errors.New("auth oauth2 must not set service_account_key_file (mutually exclusive with oauth_token_file)")
		}
	case "":
		return errors.New("auth is required and must be \"service_account\" or \"oauth2\"")
	default:
		return fmt.Errorf("auth %q is invalid; must be \"service_account\" or \"oauth2\"", g.Auth)
	}
	return nil
}

// validateGSCProperty accepts the two GSC property forms and rejects everything
// else. A URL-prefix property must be an http/https absolute URL with a host; a
// Domain property is "sc-domain:" followed by a non-empty host (no scheme, no
// slash). The check is deliberately lightweight — it pins the shape the GSC client
// expects, not a full RFC validation of the host.
func validateGSCProperty(property string) error {
	if property == "" {
		return errors.New("property is required (\"https://ex.com/\" or \"sc-domain:ex.com\")")
	}
	if rest, ok := strings.CutPrefix(property, gscDomainPrefix); ok {
		// Domain property: a bare host, no scheme, no slash.
		if rest == "" {
			return errors.New("property \"sc-domain:\" must be followed by a domain (e.g. \"sc-domain:ex.com\")")
		}
		if strings.ContainsAny(rest, "/ \t") || strings.Contains(rest, "://") {
			return fmt.Errorf("property %q is a malformed sc-domain: identifier (expected \"sc-domain:ex.com\")", property)
		}
		return nil
	}
	// Otherwise it must be a URL-prefix property (http/https absolute URL).
	u, err := url.Parse(property)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("property %q is invalid; expected a URL-prefix \"https://ex.com/\" or a Domain \"sc-domain:ex.com\"", property)
	}
	return nil
}
