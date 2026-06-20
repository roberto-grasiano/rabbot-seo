package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
)

// GSC connectivity check for `rabbot doctor <url>`. It mirrors runDoctorControl:
// best-effort, NEVER fatal, always finishes; an unconfigured site is an honest
// WARNING (not a failure), matching the zero-notifier section. It prints only
// non-secret facts — the property identifier, the auth MODE, the key-file
// BASENAME, and the connectivity verdict — never the key path contents, the token,
// or any bearer.

// gscDoctorClient is the narrow seam the GSC probe needs: a single lightweight
// authenticated call to prove the credential works and the property is reachable.
// *gsc.Client satisfies it; tests substitute a real client pointed at httptest.
type gscDoctorClient interface {
	ListSites(ctx context.Context) (*gsc.SitesListResponse, error)
}

// gscDoctorClientFactory builds a gscDoctorClient from a token provider. Production
// wraps gsc.NewClient with the package's own timeout-bounded HTTP client; tests
// substitute an httptest-backed client. It is the injection seam that keeps the
// probe runnable with no live credentials.
type gscDoctorClientFactory func(tp gsc.TokenProvider) (gscDoctorClient, error)

// gscReadinessInput carries the already-probed facts renderGSCReadiness turns into a
// report. Splitting the probe (which touches disk + the network) from the renderer
// keeps the renderer pure and table-testable (the runDoctorControl precedent). It
// holds ONLY non-secret fields — note keyBasename, never the full key path.
type gscReadinessInput struct {
	configured   bool        // the site has an active GSC block
	property     string      // the GSC property identifier (non-secret)
	authMode     string      // "service_account" | "oauth2"
	keyBasename  string      // basename of the credential file (never the full path)
	keyFound     bool        // the credential file exists
	keyMode      os.FileMode // the credential file's permission bits (when keyFound)
	probeErr     error       // result of the lightweight authenticated probe (sites.list)
	propertySeen bool        // the configured property appears in the credential's site list
}

// renderGSCReadiness writes the Search Console readiness section. An unconfigured
// site prints a single WARNING line; a configured site prints the property, auth
// mode, key-file presence/0600, and the connectivity/visibility verdict. It returns
// only the first write error; a failed check is reported in the text, never as a Go
// error (so doctor always prints a full report).
func renderGSCReadiness(w io.Writer, in gscReadinessInput) error {
	ew := &errWriter{w: w}
	ew.println("\nSearch Console:")

	if !in.configured {
		ew.println("  status:          not configured for this site — Search Console " +
			"intelligence (index status, search performance) is off; add a `gsc` block per " +
			"site to connect Google's ground truth")
		return ew.err
	}

	ew.printf("  property:        %s\n", in.property)
	ew.printf("  auth:            %s\n", in.authMode)

	// Credential file presence + 0600.
	credOK := in.keyFound && in.keyMode.Perm() == 0o600
	ew.printf("  [%s] credential file present (0600)\n", mark(credOK))
	switch {
	case !in.keyFound:
		ew.printf("        no credential file (%s) — create it (service-account key, or run "+
			"`rabbot gsc auth` for OAuth)\n", in.keyBasename)
	case in.keyMode.Perm() != 0o600:
		ew.printf("        %s is %o, want 0600 — run: chmod 600 <the credential file>\n",
			in.keyBasename, in.keyMode.Perm())
	default:
		ew.printf("        %s\n", in.keyBasename)
	}

	// Connectivity / property visibility (the lightweight authenticated probe).
	probeOK := in.probeErr == nil && in.propertySeen
	ew.printf("  [%s] authenticated and property reachable\n", mark(probeOK))
	switch {
	case in.probeErr != nil:
		ew.printf("        connectivity check failed: %v\n", in.probeErr)
	case !in.propertySeen:
		ew.printf("        authenticated, but %s is not in the credential's verified-site "+
			"list — grant the credential read access to the property in Search Console "+
			"(Settings → Users and permissions)\n", in.property)
	}

	return ew.err
}

// probeGSCReadiness assembles a gscReadinessInput from the live facts: the per-site
// GSC config, the credential file's existence+mode, and a single sites.list probe
// through a client built by factory. It never returns an error — a failed probe is
// recorded in probeErr for the renderer (the runDoctorControl contract). An
// unconfigured site short-circuits to configured=false.
func probeGSCReadiness(ctx context.Context, cfg *config.Config, baseURL string, factory gscDoctorClientFactory) gscReadinessInput {
	gc, configured := cfg.GSCForBaseURL(baseURL)
	if !configured {
		return gscReadinessInput{configured: false}
	}

	in := gscReadinessInput{
		configured: true,
		property:   gc.Property,
		authMode:   gc.Auth,
	}

	// Stat the credential file referenced by the mode (basename only is reported).
	credPath := gc.ServiceAccountKeyFile
	if gc.Auth == config.GSCAuthOAuth2 {
		credPath = gc.OAuthTokenFile
	}
	if credPath != "" {
		in.keyBasename = filepath.Base(credPath)
		if fi, err := os.Stat(credPath); err == nil {
			in.keyFound = true
			in.keyMode = fi.Mode()
		}
	}

	// Build the token provider from the on-disk credential, then the client, then
	// run the lightweight sites.list probe. Any failure is recorded as probeErr.
	prov, perr := providerForSite(ctx, gc)
	if perr != nil {
		in.probeErr = perr
		return in
	}
	client, cerr := factory(prov)
	if cerr != nil {
		in.probeErr = cerr
		return in
	}
	resp, lerr := client.ListSites(ctx)
	if lerr != nil {
		in.probeErr = lerr
		return in
	}
	in.propertySeen = propertyInList(gc.Property, resp)
	return in
}

// propertyInList reports whether the configured property is among the credential's
// verified Search Console sites.
func propertyInList(property string, resp *gsc.SitesListResponse) bool {
	if resp == nil {
		return false
	}
	for _, e := range resp.SiteEntry {
		if e.SiteURL == property {
			return true
		}
	}
	return false
}

// runDoctorGSC resolves the live GSC facts for the doctor target and renders the
// Search Console section to w. It is best-effort and never fatal, so doctor always
// finishes. It wires the production client factory (the gsc package's own
// timeout-bounded client); tests call probeGSCReadiness/renderGSCReadiness directly.
func runDoctorGSC(ctx context.Context, w io.Writer, cfg *config.Config, baseURL string) error {
	in := probeGSCReadiness(ctx, cfg, baseURL, productionDoctorGSCClient)
	return renderGSCReadiness(w, in)
}

// productionDoctorGSCClient is the doctor's production client factory: the gsc
// package's own HTTP client (NOT the SSRF-guarded crawl fetcher).
func productionDoctorGSCClient(tp gsc.TokenProvider) (gscDoctorClient, error) {
	return gsc.NewClient(gsc.Options{Token: tp})
}
