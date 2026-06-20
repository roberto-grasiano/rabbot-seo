package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
)

// TestRunGSCAuthExchange_PersistsCredFile0600 exercises the testable core of
// `rabbot gsc auth`: given an authorization code, it exchanges it (against a mocked
// token endpoint) and writes the BYO client creds + the refresh token to a 0600
// file. No live consent, no browser.
func TestRunGSCAuthExchange_PersistsCredFile0600(t *testing.T) {
	t.Parallel()
	// Mock token endpoint returning a refresh token for the code exchange.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-123","refresh_token":"rt-456","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := gsc.OAuthConfig{
		ClientID:     "client.apps.googleusercontent.com",
		ClientSecret: "secret-shh",
		TokenURL:     srv.URL + "/token",
		AuthURL:      srv.URL + "/auth",
		RedirectURL:  "http://127.0.0.1:0/callback",
		HTTPClient:   srv.Client(),
	}
	flow, err := gsc.NewConsentFlow(cfg)
	if err != nil {
		t.Fatalf("NewConsentFlow: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "gsc-oauth.json")
	if err := runGSCAuthExchange(context.Background(), flow, "the-code", cfg.ClientID, cfg.ClientSecret, outPath); err != nil {
		t.Fatalf("runGSCAuthExchange: %v", err)
	}

	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat persisted cred: %v", err)
	}
	// Unix file modes only — Windows has no 0600 bit to assert (file existence above
	// already proves the exchange persisted the cred).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("persisted cred perms = %o, want 0600", fi.Mode().Perm())
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read persisted cred: %v", err)
	}
	var cred oauthCredFile
	if err := json.Unmarshal(body, &cred); err != nil {
		t.Fatalf("unmarshal cred: %v", err)
	}
	if cred.ClientID != cfg.ClientID || cred.ClientSecret != cfg.ClientSecret {
		t.Errorf("client creds not persisted: %+v", cred)
	}
	if cred.RefreshToken != "rt-456" {
		t.Errorf("refresh token = %q, want rt-456", cred.RefreshToken)
	}

	// The persisted file must be re-readable by the runtime provider factory.
	prov, err := providerForSite(context.Background(), config.GSCConfig{
		Property: "https://ex.com/", Auth: config.GSCAuthOAuth2, OAuthTokenFile: outPath,
	})
	if err != nil {
		t.Fatalf("providerForSite over persisted cred: %v", err)
	}
	if prov.Mode() != "oauth" {
		t.Errorf("provider mode = %q, want oauth", prov.Mode())
	}
}

// TestRunGSCAuthExchange_ExchangeFailureNoFileWritten asserts a failed exchange does
// NOT leave a partial/empty cred file behind.
func TestRunGSCAuthExchange_ExchangeFailureNoFileWritten(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code"}`))
	}))
	defer srv.Close()
	cfg := gsc.OAuthConfig{
		ClientID: "c", ClientSecret: "s",
		TokenURL: srv.URL + "/token", AuthURL: srv.URL + "/auth",
		RedirectURL: "http://127.0.0.1:0/callback", HTTPClient: srv.Client(),
	}
	flow, _ := gsc.NewConsentFlow(cfg)
	outPath := filepath.Join(t.TempDir(), "gsc-oauth.json")
	if err := runGSCAuthExchange(context.Background(), flow, "bad", cfg.ClientID, cfg.ClientSecret, outPath); err == nil {
		t.Fatal("want an error on a failed exchange")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("a failed exchange must not write the cred file; stat err = %v", err)
	}
}

// TestParseLoopbackCallback validates the loopback redirect parsing: it extracts the
// code only when the state matches and surfaces an OAuth error param.
func TestParseLoopbackCallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		rawquery  string
		wantState string
		wantCode  string
		wantErr   bool
	}{
		{"happy", "code=abc&state=xyz", "xyz", "abc", false},
		{"state mismatch", "code=abc&state=zzz", "xyz", "", true},
		{"oauth error", "error=access_denied&state=xyz", "xyz", "", true},
		{"missing code", "state=xyz", "xyz", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vals, _ := url.ParseQuery(tc.rawquery)
			code, err := parseLoopbackCallback(vals, tc.wantState)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for %q", tc.rawquery)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestResolveGSCAuthClientCreds prefers flags, falls back to env, and errors when
// neither supplies the BYO client id/secret.
func TestResolveGSCAuthClientCreds(t *testing.T) {
	// not parallel: mutates env
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_ID", "")
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_SECRET", "")

	if _, _, err := resolveGSCAuthClientCreds("", ""); err == nil {
		t.Fatal("want error when neither flag nor env supplies creds")
	}

	id, secret, err := resolveGSCAuthClientCreds("flag-id", "flag-secret")
	if err != nil || id != "flag-id" || secret != "flag-secret" {
		t.Fatalf("flags should win: id=%q secret=%q err=%v", id, secret, err)
	}

	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_ID", "env-id")
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_SECRET", "env-secret")
	id, secret, err = resolveGSCAuthClientCreds("", "")
	if err != nil || id != "env-id" || secret != "env-secret" {
		t.Fatalf("env fallback: id=%q secret=%q err=%v", id, secret, err)
	}
}

// TestGSCAuthCmd_RequiresClientCreds verifies the cobra command errors clearly when
// no client creds are provided (it must not start a browser flow).
func TestGSCAuthCmd_RequiresClientCreds(t *testing.T) {
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_ID", "")
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_SECRET", "")
	cmd := newGSCCmd()
	cmd.SetArgs([]string{"auth", "--out", filepath.Join(t.TempDir(), "t.json")})
	cmd.SetOut(os.NewFile(0, os.DevNull))
	cmd.SetErr(os.NewFile(0, os.DevNull))
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "client") {
		t.Fatalf("want a clear missing-client-creds error, got %v", err)
	}
}

// TestRandomState asserts the anti-CSRF state is a 32-char hex string (128 bits),
// uses only lowercase-hex runes, and is unique across calls (never a constant).
func TestRandomState(t *testing.T) {
	t.Parallel()
	const wantLen = 32 // 16 bytes hex-encoded
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		s, err := randomState()
		if err != nil {
			t.Fatalf("randomState: %v", err)
		}
		if len(s) != wantLen {
			t.Fatalf("state len = %d, want %d (128-bit hex)", len(s), wantLen)
		}
		for _, c := range s {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("state %q has a non-hex rune %q", s, c)
			}
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("randomState produced a duplicate %q — not random", s)
		}
		seen[s] = struct{}{}
	}
}

// TestResolveGSCAuthOutPath covers both branches: an explicit --out passes through
// verbatim, and the empty default resolves under the config dir as gsc-oauth.json.
func TestResolveGSCAuthOutPath(t *testing.T) {
	t.Parallel()

	// Explicit path wins, untouched.
	const explicit = "/tmp/some/where/cred.json"
	if got, err := resolveGSCAuthOutPath(explicit); err != nil || got != explicit {
		t.Fatalf("explicit out: got %q err %v, want %q", got, err, explicit)
	}

	// Default path: <config-dir>/gsc-oauth.json. We don't pin the dir (OS-specific),
	// only that the basename is right and the dir is non-empty.
	got, err := resolveGSCAuthOutPath("")
	if err != nil {
		t.Fatalf("default out: %v", err)
	}
	if filepath.Base(got) != "gsc-oauth.json" {
		t.Errorf("default basename = %q, want gsc-oauth.json", filepath.Base(got))
	}
	if filepath.Dir(got) == "" || filepath.Dir(got) == "." {
		t.Errorf("default out path has no config dir: %q", got)
	}
}

// mockTokenServer returns an httptest server that answers the OAuth token exchange
// with a refresh token, so the consent-flow code exchange succeeds offline.
func mockTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-xyz","refresh_token":"rt-xyz","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCaptureLoopbackCode_HappyPath starts the one-shot loopback server, fires the
// redirect with a matching state + code, and asserts the captured code. This drives
// the real http.Server + the goroutine/select machinery (no consent, no browser).
func TestCaptureLoopbackCode_HappyPath(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const state = "state-ok"

	type res struct {
		code string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		code, cerr := captureLoopbackCode(context.Background(), ln, state)
		done <- res{code: code, err: cerr}
	}()

	// Hit the callback the way Google's redirect would.
	redirect := "http://" + ln.Addr().String() + "/callback?code=the-code&state=" + state
	resp, err := http.Get(redirect) //nolint:noctx // test request to a local one-shot server
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	_ = resp.Body.Close()

	got := <-done
	if got.err != nil {
		t.Fatalf("captureLoopbackCode: %v", got.err)
	}
	if got.code != "the-code" {
		t.Errorf("captured code = %q, want the-code", got.code)
	}
}

// TestCaptureLoopbackCode_StateMismatchRejected proves a CSRF-mismatched state is a
// hard failure (the captured code is dropped and an error is returned), even though
// a code is present in the query.
func TestCaptureLoopbackCode_StateMismatchRejected(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	type res struct {
		code string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		code, cerr := captureLoopbackCode(context.Background(), ln, "expected-state")
		done <- res{code: code, err: cerr}
	}()

	// Wrong state → must be rejected as possible CSRF.
	redirect := "http://" + ln.Addr().String() + "/callback?code=attacker&state=WRONG"
	resp, err := http.Get(redirect) //nolint:noctx // test request to a local one-shot server
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("callback HTTP status = %d, want 400 for a bad state", resp.StatusCode)
	}
	_ = resp.Body.Close()

	got := <-done
	if got.err == nil {
		t.Fatal("a state mismatch must return an error (possible CSRF)")
	}
	if got.code != "" {
		t.Errorf("a rejected callback must capture no code, got %q", got.code)
	}
}

// TestCaptureLoopbackCode_ContextCancelTimesOut covers the ctx-cancellation arm: a
// cancelled context unblocks the wait with an error rather than hanging.
func TestCaptureLoopbackCode_ContextCancelled(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	code, cerr := captureLoopbackCode(ctx, ln, "s")
	if cerr == nil {
		t.Fatal("a cancelled context must end the wait with an error")
	}
	if code != "" {
		t.Errorf("no code on cancellation, got %q", code)
	}
}

// TestRunGSCAuthLoopback_CapturesAndPersists drives the FULL loopback path: it binds
// an ephemeral loopback listener, then (concurrently) fires the redirect the browser
// would, so captureLoopbackCode returns the code, the consent flow exchanges it at a
// mocked token endpoint (via the endpoint seam), and the 0600 cred file is written.
// No real browser/Google.
func TestRunGSCAuthLoopback_CapturesAndPersists(t *testing.T) {
	t.Parallel()
	tokenSrv := mockTokenServer(t)
	outPath := filepath.Join(t.TempDir(), "gsc-oauth.json")

	// A pipe lets the test scan the command's printed output to discover the ephemeral
	// loopback redirect URL, then play the browser and hit it with code+state.
	pr, pw := io.Pipe()

	p := gscAuthParams{
		clientID:     "client.apps.googleusercontent.com",
		clientSecret: "secret",
		port:         0, // ephemeral
		outPath:      outPath,
		endpoint:     gscAuthEndpoint{TokenURL: tokenSrv.URL + "/token", HTTPClient: tokenSrv.Client()},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- runGSCAuthLoopback(context.Background(), pw, p, "state-loop") }()

	// Find the redirect URL the command printed, then fire the matching code+state.
	redirect := scanForRedirect(t, pr)
	// Drain any remaining output so the writer side never blocks on the pipe.
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	full := redirect + "?code=loop-code&state=state-loop"
	resp, err := http.Get(full) //nolint:noctx // test request to a local one-shot server
	if err != nil {
		t.Fatalf("GET loopback redirect: %v", err)
	}
	_ = resp.Body.Close()

	if err := <-errCh; err != nil {
		t.Fatalf("runGSCAuthLoopback: %v", err)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat cred: %v", err)
	}
	// Unix file modes only — Windows has no 0600 bit to assert (file existence above
	// already proves the loopback flow persisted the cred).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("cred perms = %o, want 0600", fi.Mode().Perm())
	}
	body, _ := os.ReadFile(outPath)
	var cred oauthCredFile
	if err := json.Unmarshal(body, &cred); err != nil {
		t.Fatalf("unmarshal cred: %v", err)
	}
	if cred.RefreshToken != "rt-xyz" {
		t.Errorf("persisted refresh = %q, want rt-xyz", cred.RefreshToken)
	}
}

// TestRunGSCAuthLoopback_BadPortErrors covers the listener-bind error arm: an invalid
// port makes net.Listen fail, surfacing a clear error (no panic, no file).
func TestRunGSCAuthLoopback_BadPortErrors(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := gscAuthParams{
		clientID:     "client.apps.googleusercontent.com",
		clientSecret: "secret",
		port:         -1, // invalid → bind fails
		outPath:      filepath.Join(t.TempDir(), "c.json"),
	}
	if err := runGSCAuthLoopback(context.Background(), &out, p, "s"); err == nil {
		t.Fatal("an invalid loopback port must surface a bind error")
	}
}

// TestRunGSCAuth_RoutesToLoopback asserts runGSCAuth runs the loopback path: an
// invalid port makes the loopback bind fail fast, proving that branch was taken.
func TestRunGSCAuth_RoutesToLoopback(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runGSCAuth(context.Background(), &out, gscAuthParams{
		clientID:     "client.apps.googleusercontent.com",
		clientSecret: "secret",
		port:         -1, // invalid → loopback bind fails immediately
		outPath:      filepath.Join(t.TempDir(), "c.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("runGSCAuth must run the loopback path (bind error expected), got %v", err)
	}
}

// TestRunGSCAuthExchange_NoRefreshTokenErrors covers the "Google returned no refresh
// token" guard: an exchange that yields an access token but NO refresh token must
// error (the daemon can't pull unattended) and write no file.
func TestRunGSCAuthExchange_NoRefreshTokenErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		// Access token only — no refresh_token.
		_, _ = w.Write([]byte(`{"access_token":"at-only","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	flow, err := gsc.NewConsentFlow(gsc.OAuthConfig{
		ClientID: "c.apps.googleusercontent.com", ClientSecret: "s",
		TokenURL: srv.URL + "/token", RedirectURL: "http://127.0.0.1:0/callback", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewConsentFlow: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "gsc-oauth.json")
	err = runGSCAuthExchange(context.Background(), flow, "code", "c.apps.googleusercontent.com", "s", outPath)
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("want a no-refresh-token error, got %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("no file should be written when no refresh token is returned; stat err = %v", statErr)
	}
}

// TestNewGSCAuthCmd_WiringResolvesAndDispatches drives the cobra RunE wiring far
// enough to cover resolve-creds → resolve-out → runGSCAuth: with creds supplied and
// an invalid --port, the loopback dispatch fails fast (no live Google), exercising the
// command's resolution + dispatch glue.
func TestNewGSCAuthCmd_WiringResolvesAndDispatches(t *testing.T) {
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_ID", "")
	t.Setenv("RABBOT_GSC_OAUTH_CLIENT_SECRET", "")
	cmd := newGSCCmd()
	cmd.SetArgs([]string{"auth",
		"--client-id", "c.apps.googleusercontent.com",
		"--client-secret", "s",
		"--out", filepath.Join(t.TempDir(), "c.json"),
		"--port", "-1", // invalid → loopback bind fails after the resolve glue ran
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(""))
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected the loopback bind to fail (after the resolve+dispatch wiring ran)")
	}
}

// scanForRedirect reads the command's printed output line by line until it finds the
// loopback redirect base URL ("http://127.0.0.1:PORT/callback") and returns it.
func scanForRedirect(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := r.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				line := string(buf)
				buf = buf[:0]
				if i := strings.Index(line, "http://127.0.0.1:"); i >= 0 {
					if j := strings.Index(line[i:], "/callback"); j >= 0 {
						return strings.TrimSpace(line[i : i+j+len("/callback")])
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	t.Fatal("never found the loopback redirect URL in the command output")
	return ""
}
