package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fsatomic"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
)

// `rabbot gsc auth` — the one-time OAuth2 installed-app consent. The operator brings
// their OWN Google Cloud OAuth client (no public client ships in Rabbot — that would
// be a ToS-violating extractable secret). This runs the consent, exchanges the code
// for a REFRESH token, and writes the BYO client creds + token to a 0600 file the
// daemon reads at runtime. The client secret + tokens are NEVER logged.
//
// W1 scope: this auth command + its token persistence is plumbing. The wizard step
// that walks an operator through creating the GCP client is W2.

// Env fallbacks for the BYO installed-app client (so the secret need not appear on
// the command line / in shell history). They hold the client id/secret, not a token.
const (
	envGSCOAuthClientID     = "RABBOT_GSC_OAUTH_CLIENT_ID"
	envGSCOAuthClientSecret = "RABBOT_GSC_OAUTH_CLIENT_SECRET" // #nosec G101 -- env var NAME, not a secret value
)

// newGSCCmd builds the `rabbot gsc` parent command. `auth` (W1) completes the
// one-time OAuth2 consent; `status` and `performance` (W2) are read-only surfaces
// over the pulled GSC data (they read the store directly, so they work daemon-down).
func newGSCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gsc",
		Short: "Google Search Console integration",
		Long: "Connect a monitored site to its Google Search Console property. Use the " +
			"`auth` subcommand to complete the one-time OAuth2 consent and store a refresh " +
			"token; service-account credentials need no consent (point the site's config at " +
			"the key file). `status <url>` and `performance --url <url>` read Google's index " +
			"status and search performance for a monitored URL (read-only).",
	}
	cmd.AddCommand(newGSCAuthCmd())
	cmd.AddCommand(newGSCStatusCmd())
	cmd.AddCommand(newGSCPerformanceCmd())
	return cmd
}

// newGSCAuthCmd builds `rabbot gsc auth`. It runs the installed-app consent and
// writes the refresh-token credential file.
func newGSCAuthCmd() *cobra.Command {
	var (
		clientID     string
		clientSecret string
		outPath      string
		port         int
	)
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Complete the one-time OAuth2 consent and store a refresh token (0600)",
		Long: "Run the OAuth2 installed-app consent for a Search Console property you own. " +
			"Bring your own Google Cloud OAuth client (--client-id/--client-secret, or the " +
			"RABBOT_GSC_OAUTH_CLIENT_ID / RABBOT_GSC_OAUTH_CLIENT_SECRET env vars). It captures " +
			"the consent redirect on a loopback port, so run it where you can open a browser. " +
			"On a headless server either use a SERVICE ACCOUNT (no consent flow — point the " +
			"site's config at the key file), or run `rabbot gsc auth` locally and copy/scp the " +
			"resulting credential file to the box. The refresh token is written 0600 and " +
			"referenced from a site's `gsc.oauth_token_file`.",
		RunE: func(c *cobra.Command, _ []string) error {
			id, secret, err := resolveGSCAuthClientCreds(clientID, clientSecret)
			if err != nil {
				return err
			}
			out, err := resolveGSCAuthOutPath(outPath)
			if err != nil {
				return err
			}
			return runGSCAuth(c.Context(), c.OutOrStdout(), gscAuthParams{
				clientID: id, clientSecret: secret, outPath: out, port: port,
			})
		},
	}
	cmd.Flags().StringVar(&clientID, "client-id", "", "BYO Google OAuth client id (or "+envGSCOAuthClientID+")")
	cmd.Flags().StringVar(&clientSecret, "client-secret", "", "BYO Google OAuth client secret (or "+envGSCOAuthClientSecret+")")
	cmd.Flags().StringVar(&outPath, "out", "", "path to write the 0600 refresh-token credential file (default: <config-dir>/gsc-oauth.json)")
	cmd.Flags().IntVar(&port, "port", 0, "loopback port for the redirect capture (0 = ephemeral)")
	return cmd
}

// resolveGSCAuthClientCreds resolves the BYO client id/secret: explicit flags win,
// then the env fallbacks. Both are required (the consent cannot run without a
// client). The secret is never echoed.
func resolveGSCAuthClientCreds(flagID, flagSecret string) (id, secret string, err error) {
	id = flagID
	if id == "" {
		id = os.Getenv(envGSCOAuthClientID)
	}
	secret = flagSecret
	if secret == "" {
		secret = os.Getenv(envGSCOAuthClientSecret)
	}
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("gsc auth: a BYO OAuth client is required — pass --client-id/--client-secret or set %s/%s",
			envGSCOAuthClientID, envGSCOAuthClientSecret)
	}
	return id, secret, nil
}

// resolveGSCAuthOutPath resolves where to write the credential file: the explicit
// --out, else <config-dir>/gsc-oauth.json.
func resolveGSCAuthOutPath(flagOut string) (string, error) {
	if flagOut != "" {
		return flagOut, nil
	}
	dir, err := config.ResolveConfigDir()
	if err != nil {
		return "", fmt.Errorf("gsc auth: resolve config dir for the default token path: %w", err)
	}
	return filepath.Join(dir, "gsc-oauth.json"), nil
}

// gscAuthParams bundles the resolved inputs for runGSCAuth.
type gscAuthParams struct {
	clientID     string
	clientSecret string
	outPath      string
	port         int

	// endpoint is an OPTIONAL test-only override of the OAuth token endpoint + HTTP
	// client used for the code exchange. The zero value means "use Google's default
	// token endpoint and the gsc default client" — production always leaves it zero.
	// It is the seam that keeps the loopback flow hermetically testable (the exchange
	// hits an httptest server, no live Google / browser).
	endpoint gscAuthEndpoint
}

// gscAuthEndpoint optionally overrides the token URL + HTTP client for the code
// exchange. A zero value (TokenURL == "") leaves the consent flow on Google's
// defaults.
type gscAuthEndpoint struct {
	TokenURL   string
	HTTPClient *http.Client
}

// applyTo layers the optional endpoint override onto a base OAuthConfig.
func (e gscAuthEndpoint) applyTo(cfg gsc.OAuthConfig) gsc.OAuthConfig {
	if e.TokenURL != "" {
		cfg.TokenURL = e.TokenURL
	}
	if e.HTTPClient != nil {
		cfg.HTTPClient = e.HTTPClient
	}
	return cfg
}

// runGSCAuth drives the consent flow end-to-end. It binds a one-shot 127.0.0.1
// server (loopback only, mirroring the control-server bind discipline), opens the
// consent URL, captures the redirected code, then exchanges it and persists the
// credential. The client secret and tokens are never logged.
func runGSCAuth(ctx context.Context, out io.Writer, p gscAuthParams) error {
	state, err := randomState()
	if err != nil {
		return err
	}
	return runGSCAuthLoopback(ctx, out, p, state)
}

// runGSCAuthLoopback binds a one-shot loopback server, opens the consent URL, and
// captures the redirected code.
func runGSCAuthLoopback(ctx context.Context, out io.Writer, p gscAuthParams, state string) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", p.port)))
	if err != nil {
		return fmt.Errorf("gsc auth: bind loopback redirect listener: %w", err)
	}
	defer func() { _ = ln.Close() }()

	redirectURL := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	cfg := p.endpoint.applyTo(gsc.OAuthConfig{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		RedirectURL:  redirectURL,
	})
	flow, err := gsc.NewConsentFlow(cfg)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out, "Open this URL in a browser and approve access:")
	_, _ = fmt.Fprintln(out, "  "+flow.AuthCodeURL(state))
	_, _ = fmt.Fprintf(out, "Waiting for the redirect on %s ...\n", redirectURL)

	code, err := captureLoopbackCode(ctx, ln, state)
	if err != nil {
		return err
	}
	return runGSCAuthExchange(ctx, flow, code, p.clientID, p.clientSecret, p.outPath)
}

// captureLoopbackCode serves a single request on ln, validates the state, returns
// the authorization code, and shuts the server down. It honors ctx cancellation. A
// 5-minute timeout bounds a never-completed consent.
func captureLoopbackCode(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			code, err := parseLoopbackCallback(r.URL.Query(), state)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("Authorization failed. You can close this tab and check the terminal."))
				done <- result{err: err}
				return
			}
			_, _ = w.Write([]byte("Authorization received. You can close this tab and return to the terminal."))
			done <- result{code: code}
		}),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		res = result{err: fmt.Errorf("gsc auth: timed out waiting for the consent redirect: %w", ctx.Err())}
	case e := <-serveErr:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			res = result{err: fmt.Errorf("gsc auth: redirect server: %w", e)}
		}
	}
	// Best-effort graceful shutdown of the one-shot server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)

	return res.code, res.err
}

// parseLoopbackCallback extracts the authorization code from the redirect query,
// validating the anti-CSRF state and surfacing an OAuth error param. A state
// mismatch is a hard failure (possible CSRF), never silently accepted.
func parseLoopbackCallback(vals url.Values, wantState string) (string, error) {
	if e := vals.Get("error"); e != "" {
		desc := vals.Get("error_description")
		if desc != "" {
			return "", fmt.Errorf("gsc auth: consent denied: %s (%s)", e, desc)
		}
		return "", fmt.Errorf("gsc auth: consent denied: %s", e)
	}
	if vals.Get("state") != wantState {
		return "", errors.New("gsc auth: state mismatch on the redirect (possible CSRF); aborting")
	}
	code := vals.Get("code")
	if code == "" {
		return "", errors.New("gsc auth: redirect carried no authorization code")
	}
	return code, nil
}

// runGSCAuthExchange exchanges the authorization code for tokens and persists the
// BYO client creds + the refresh token to a 0600 file (atomically). It is the
// testable core of the command. A failed exchange writes NO file. The secret/tokens
// are never logged.
func runGSCAuthExchange(ctx context.Context, flow *gsc.ConsentFlow, code, clientID, clientSecret, outPath string) error {
	tok, err := flow.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("gsc auth: exchange authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		// Without a refresh token the daemon cannot pull unattended. This happens if
		// the user had already granted access without forcing re-consent; the flow
		// requests offline+force, so surface it clearly rather than persisting a
		// short-lived access-only credential.
		return errors.New("gsc auth: Google returned no refresh token — revoke the app's prior grant and retry (the consent must issue a refresh token for unattended pulls)")
	}
	cred := oauthCredFile{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		StoredToken:  *tok,
	}
	body, err := marshalOAuthCred(cred)
	if err != nil {
		return fmt.Errorf("gsc auth: encode credential: %w", err)
	}
	// 0600 file, 0700 dir — the secret-file discipline (control.token neighbours).
	if err := fsatomic.Write(outPath, body, 0o600, 0o700); err != nil {
		return fmt.Errorf("gsc auth: persist credential: %w", err)
	}
	return nil
}

// randomState returns a 128-bit hex anti-CSRF state.
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gsc auth: generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}
