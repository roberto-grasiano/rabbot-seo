package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// GSC credential wiring (the secret-sensitive surface). The credentials live in
// 0600 FILES referenced by path in the per-site config (never inline). This file
// reads them at runtime, re-tightens loosened perms (the control/token.go
// discipline), builds the gsc TokenProvider, and assembles the puller. The
// credential CONTENT — the SA private key, the OAuth client secret, the refresh /
// access tokens — is NEVER logged, echoed into an error, or returned by any command.

// oauthCredFile is the on-disk 0600 file `rabbot gsc auth` writes and the daemon
// reads for an oauth2 site. It carries the BYO installed-app client creds alongside
// the persisted token: the gsc OAuth provider needs the client_id + client_secret
// to refresh the access token at runtime, so the natural home for them is the same
// owner-only credential file as the refresh token (mirroring how installed-app
// credential files bundle the client creds with the token). The embedded
// gsc.StoredToken contributes its access_token/refresh_token/token_type/expiry
// fields at the same JSON level; gsc.ParseStoredToken can read the token half of the
// same bytes (it ignores the extra client fields).
//
// SECURITY: every field here is a secret. The whole file is written 0600 via
// fsatomic and is never logged. The struct deliberately has no String()/redaction
// because it is never formatted — it lives only between disk and the provider.
type oauthCredFile struct {
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	gsc.StoredToken        // access_token, refresh_token, token_type, expiry
}

// marshalOAuthCred serializes the cred file for 0600 persistence. The client secret
// and tokens are INTENTIONALLY serialized — this is the on-disk credential file
// (written 0600 via fsatomic by `rabbot gsc auth`, never logged); redacting them
// here would defeat the persisted-credential purpose (mirrors gsc.StoredToken.Marshal).
//
//nolint:gosec // G117: deliberate secret serialization to the 0600 oauth credential file
func marshalOAuthCred(c oauthCredFile) ([]byte, error) { return json.Marshal(c) }

// parseOAuthCred deserializes a persisted oauth cred file.
func parseOAuthCred(b []byte) (oauthCredFile, error) {
	var c oauthCredFile
	if err := json.Unmarshal(b, &c); err != nil {
		return oauthCredFile{}, fmt.Errorf("gsc: parse oauth credential file: %w", err)
	}
	return c, nil
}

// loadGSCSecret reads a 0600 credential file and, mirroring control.LoadOrCreateToken,
// re-tightens a loosened file back to 0600 on read (a restore/umask/misconfigured
// deploy can leave a credential group/world-readable). It returns the bytes and the
// permission bits AS FOUND on disk (so the doctor can warn about drift) — the
// re-tighten happens regardless. The credential bytes are never logged; an IO error
// is returned verbatim (it carries the path, not the content).
func loadGSCSecret(path string) (data []byte, foundMode os.FileMode, err error) {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		return nil, 0, fmt.Errorf("gsc: read credential %s: %w", path, statErr)
	}
	foundMode = fi.Mode()
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, foundMode, fmt.Errorf("gsc: read credential %s: %w", path, readErr)
	}
	// Re-tighten if the file ended up looser than 0600. Best-effort: the read
	// succeeded, so a chmod failure must not turn a good load into an error.
	if foundMode.Perm() != 0o600 {
		_ = os.Chmod(path, 0o600)
	}
	return b, foundMode, nil
}

// providerForSite builds the gsc.TokenProvider for one site's GSC config by reading
// its 0600 credential file. It dispatches on the validated Auth mode. The credential
// content never appears in a returned error (the gsc constructors redact key/PEM
// material; we add only the non-secret path/mode). The context param satisfies the
// scheduler.GSCPuller.ProviderForSite seam; the synchronous file read does not use
// it (no in-flight cancellation to honor for a local open).
func providerForSite(_ context.Context, gc config.GSCConfig) (gsc.TokenProvider, error) {
	switch gc.Auth {
	case config.GSCAuthServiceAccount:
		keyJSON, _, err := loadGSCSecret(gc.ServiceAccountKeyFile)
		if err != nil {
			return nil, err
		}
		// NewServiceAccountProvider validates type/PEM and never echoes the key body.
		prov, perr := gsc.NewServiceAccountProvider(keyJSON, gsc.ServiceAccountOptions{})
		if perr != nil {
			return nil, perr
		}
		return prov, nil

	case config.GSCAuthOAuth2:
		body, _, err := loadGSCSecret(gc.OAuthTokenFile)
		if err != nil {
			return nil, err
		}
		cred, perr := parseOAuthCred(body)
		if perr != nil {
			return nil, perr
		}
		st := cred.StoredToken
		prov, nerr := gsc.NewOAuthProvider(gsc.OAuthConfig{
			ClientID:     cred.ClientID,
			ClientSecret: cred.ClientSecret,
		}, &st)
		if nerr != nil {
			return nil, nerr
		}
		return prov, nil

	default:
		// Validation (config.ValidateGSC) should have rejected this already; guard
		// anyway so a hand-edited config fails clearly rather than silently.
		return nil, fmt.Errorf("gsc: site has unsupported auth %q", gc.Auth)
	}
}

// productionGSCClient adapts gsc.NewClient to the scheduler.GSCClient seam, building
// the package's own timeout-bounded HTTP client (NOT the SSRF-guarded crawl
// fetcher). It is the puller's API factory in production.
func productionGSCClient(tp gsc.TokenProvider) (scheduler.GSCClient, error) {
	return gsc.NewClient(gsc.Options{Token: tp})
}

// anySiteHasGSC reports whether any site in cfg has an active GSC block — the gate
// for constructing the puller at all (the severability seam).
func anySiteHasGSC(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for _, s := range cfg.Sites {
		if s.GSC.IsConfigured() {
			return true
		}
	}
	return false
}

// buildGSCPuller assembles the per-site GSC puller, or returns nil when no site has
// a GSC block (so registerGSCPull registers nothing and the feature is off). The
// puller holds the store (write sinks + candidate source), a cfgMu-guarded
// BaseURL→GSCConfig resolver (safe against live config reload, the
// perHostUserAgentFunc precedent), and the production client/provider factories.
// logger may be nil (skip-logging is a no-op).
func buildGSCPuller(cfgMu *sync.Mutex, cfg *config.Config, db *store.DB, logger *slog.Logger) *scheduler.GSCPuller {
	if !anySiteHasGSC(cfg) {
		return nil
	}
	return &scheduler.GSCPuller{
		ResolveGSC:      guardedGSCResolver(cfgMu, cfg),
		Metrics:         db,
		Index:           db,
		Candidates:      &storeURLCandidates{db: db},
		API:             productionGSCClient,
		ProviderForSite: providerForSite,
		Logger:          logger,
	}
}

// buildGSCSignals assembles the W2 GSC signal evaluator (index_status_discrepancy /
// google_canonical_mismatch). It reads Google's ground truth and Rabbot's verdict
// through the store, evaluates the SAME importance-ordered URL set the puller
// inspected (storeURLCandidates), and emits/resolves alerts through the live incident
// pipeline (alerts.Pipeline satisfies GSCAlertSink). run.go builds it ONLY when the
// puller is non-nil (a GSC-configured site) and passes the live pipeline as the sink;
// the gating therefore lives at the call site (mirroring buildGSCPuller). A nil sink
// makes every Evaluate a clean no-op, so an accidental construction can never alert.
func buildGSCSignals(db *store.DB, sink scheduler.GSCAlertSink) *scheduler.GSCSignals {
	return &scheduler.GSCSignals{
		Reader:     db,
		Candidates: &storeURLCandidates{db: db},
		Alerts:     sink,
	}
}

// guardedGSCResolver returns a BaseURL→GSCConfig resolver that snapshots cfg under
// cfgMu before reading, so a concurrent reload (which mutates *cfg under the same
// lock) cannot race the puller's per-site lookup. Mirrors perHostUserAgentFunc.
func guardedGSCResolver(cfgMu *sync.Mutex, cfg *config.Config) func(string) (config.GSCConfig, bool) {
	return func(baseURL string) (config.GSCConfig, bool) {
		cfgMu.Lock()
		snapshot := *cfg
		cfgMu.Unlock()
		return snapshot.GSCForBaseURL(baseURL)
	}
}
