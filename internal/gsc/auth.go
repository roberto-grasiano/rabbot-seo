package gsc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// TokenProvider yields a fresh OAuth2 access token (bearer) for the GSC API,
// transparently minting/refreshing as needed. Implementations must be safe for
// concurrent use. Mode reports a non-secret label ("service_account" | "oauth")
// for doctor/log output.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
	Mode() string
}

// ---------------------------------------------------------------------------
// Service-account (SA-JWT) provider — hand-rolled RS256, no oauth2/google.
// ---------------------------------------------------------------------------

// serviceAccountKey is the relevant subset of a GCP service-account JSON key.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// ServiceAccountOptions configures the SA provider.
type ServiceAccountOptions struct {
	// HTTPClient is used for the token-exchange POST. A nil client defaults to a
	// 30s-timeout client (never http.DefaultClient, which has no timeout).
	HTTPClient *http.Client
	// Now is the injectable clock for the JWT iat/exp and the token cache. Nil
	// defaults to time.Now().UTC().
	Now func() time.Time
}

// ServiceAccountProvider mints short-lived access tokens by signing a JWT
// assertion with the SA private key and exchanging it at the token endpoint
// (the OAuth2 jwt-bearer grant). It caches the access token until shortly before
// expiry, re-signing on demand — no refresh token is involved for an SA.
//
// The private key and the signed assertion are secrets and are never logged or
// embedded in errors. Default formatting is redacted via String/GoString.
type ServiceAccountProvider struct {
	clientEmail  string
	privateKeyID string
	tokenURI     string
	key          *rsa.PrivateKey
	httpClient   *http.Client
	now          func() time.Time

	mu     sync.Mutex
	cached *oauth2.Token
}

// NewServiceAccountProvider parses a service-account JSON key and returns a
// provider. It validates type=="service_account" and that the private_key is a
// usable RSA PKCS#8 key. keyJSON is the credential CONTENT (read from the 0600
// file by the caller); it is never retained beyond the parsed fields.
func NewServiceAccountProvider(keyJSON []byte, opts ServiceAccountOptions) (*ServiceAccountProvider, error) {
	var sa serviceAccountKey
	if err := json.Unmarshal(keyJSON, &sa); err != nil {
		return nil, fmt.Errorf("gsc: parse service-account key: %w", err)
	}
	if sa.Type != "service_account" {
		return nil, fmt.Errorf("gsc: credential type is %q, want service_account", sa.Type)
	}
	if sa.ClientEmail == "" {
		return nil, newSentinel("gsc: service-account key missing client_email")
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &ServiceAccountProvider{
		clientEmail:  sa.ClientEmail,
		privateKeyID: sa.PrivateKeyID,
		tokenURI:     tokenURI,
		key:          key,
		httpClient:   hc,
		now:          nowFn,
	}, nil
}

// Mode reports the non-secret auth label.
func (p *ServiceAccountProvider) Mode() string { return "service_account" }

// String redacts the provider so a stray %v/%s never prints key material.
func (p *ServiceAccountProvider) String() string {
	return fmt.Sprintf("gsc.ServiceAccountProvider{email:%s, mode:service_account}", p.clientEmail)
}

// GoString redacts the provider under %#v.
func (p *ServiceAccountProvider) GoString() string { return p.String() }

// Token returns a cached access token if it is still valid, otherwise mints a
// fresh JWT assertion, exchanges it, caches the result, and returns it.
func (p *ServiceAccountProvider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil && p.cached.AccessToken != "" {
		// Treat as valid only with ~1 minute of headroom.
		if p.now().Add(60 * time.Second).Before(p.cached.Expiry) {
			return p.cached.AccessToken, nil
		}
	}

	assertion, err := p.signAssertion()
	if err != nil {
		return "", err
	}
	tok, err := exchangeAssertion(ctx, p.httpClient, p.tokenURI, assertion, p.now)
	if err != nil {
		return "", err
	}
	p.cached = tok
	return tok.AccessToken, nil
}

// signAssertion builds and RS256-signs the JWT bearer assertion.
func (p *ServiceAccountProvider) signAssertion() (string, error) {
	now := p.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if p.privateKeyID != "" {
		header["kid"] = p.privateKeyID
	}
	claims := map[string]any{
		"iss":   p.clientEmail,
		"scope": scopeWebmastersReadonly,
		"aud":   p.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(), // clamp to Google's 1h cap
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("gsc: marshal jwt header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("gsc: marshal jwt claims: %w", err)
	}
	signingInput := b64url(hb) + "." + b64url(cb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		// Never wrap with %w into a path that could expose key bytes; the stdlib
		// error here carries none, but keep it generic.
		return "", fmt.Errorf("gsc: sign jwt assertion: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

// parseRSAPrivateKey decodes a PEM PKCS#8 (or PKCS#1) RSA private key.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, newSentinel("gsc: service-account private_key is not valid PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, newSentinel("gsc: service-account private_key is not RSA")
		}
		return rk, nil
	}
	// Fall back to PKCS#1 (older keys).
	rk, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, newSentinel("gsc: service-account private_key could not be parsed")
	}
	return rk, nil
}

// ---------------------------------------------------------------------------
// Token exchange (jwt-bearer grant) — shared by the SA provider.
// ---------------------------------------------------------------------------

// exchangeAssertion POSTs the JWT bearer assertion to the token endpoint and
// returns the resulting access token with its computed expiry. On a non-200 it
// returns an error that NEVER includes the assertion. now is the provider's
// injectable clock: the expiry is computed against the SAME clock the provider's
// freshness check (Token) reads, so a frozen-clock test can advance time past the
// cached expiry and observe a re-mint (a bare time.Now() here would compute the
// expiry on the real wall clock and make that test impossible to write).
func exchangeAssertion(ctx context.Context, hc *http.Client, tokenURI, assertion string, now func() time.Time) (*oauth2.Token, error) {
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("gsc: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gsc: token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr tokenResponse
	// Cap the token-endpoint body like client.doJSON does (maxResponseBytes), so a hostile
	// or buggy endpoint cannot stream an unbounded body into the decoder.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&tr); err != nil && resp.StatusCode == http.StatusOK {
		return nil, fmt.Errorf("gsc: decode token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Do NOT echo the request body (the assertion). Surface only the endpoint's
		// own error code/description.
		return nil, fmt.Errorf("gsc: token exchange failed (%d): %s", resp.StatusCode, tr.errorSummary())
	}
	if tr.AccessToken == "" {
		return nil, newSentinel("gsc: token endpoint returned no access_token")
	}
	expiry := time.Time{}
	if tr.ExpiresIn > 0 {
		expiry = now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return &oauth2.Token{
		AccessToken: tr.AccessToken,
		TokenType:   tr.TokenType,
		Expiry:      expiry,
	}, nil
}

// tokenResponse decodes the token endpoint's JSON (success or error form).
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (t tokenResponse) errorSummary() string {
	switch {
	case t.ErrorCode != "" && t.ErrorDescription != "":
		return t.ErrorCode + ": " + t.ErrorDescription
	case t.ErrorCode != "":
		return t.ErrorCode
	default:
		return "no error detail"
	}
}

// ---------------------------------------------------------------------------
// OAuth2 installed-app provider — refresh token → access token (oauth2 core).
// ---------------------------------------------------------------------------

// OAuthConfig holds the BYO installed-app client config plus optional endpoint
// overrides (defaults to Google's). ClientSecret is a secret and is never
// logged.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	AuthURL      string // default: accounts.google.com/o/oauth2/auth
	TokenURL     string // default: oauth2.googleapis.com/token
	RedirectURL  string // for the consent flow (loopback or OOB)
	HTTPClient   *http.Client
}

func (c OAuthConfig) authURL() string {
	if c.AuthURL != "" {
		return c.AuthURL
	}
	return defaultAuthURL
}

func (c OAuthConfig) tokenURL() string {
	if c.TokenURL != "" {
		return c.TokenURL
	}
	return defaultTokenURI
}

func (c OAuthConfig) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Scopes:       []string{scopeWebmastersReadonly},
		RedirectURL:  c.RedirectURL,
		Endpoint: oauth2.Endpoint{
			AuthURL:  c.authURL(),
			TokenURL: c.tokenURL(),
		},
	}
}

// ctxWithClient binds the OAuthConfig's HTTPClient into the context so the
// oauth2 core uses it for token requests (important for httptest in tests). A
// nil client falls back to a 30s-timeout client.
func (c OAuthConfig) ctxWithClient(ctx context.Context) context.Context {
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return context.WithValue(ctx, oauth2.HTTPClient, hc)
}

// OAuthProvider yields access tokens from a stored refresh token, refreshing via
// the oauth2 core ReuseTokenSource. The refresh token and client secret are
// secrets and are never logged.
type OAuthProvider struct {
	cfg    OAuthConfig
	src    oauth2.TokenSource
	stored *StoredToken
}

// NewOAuthProvider builds a refresh-token-backed provider. The stored token must
// carry a refresh token (obtained once via the consent flow). The oauth2 core
// transparently refreshes the access token against TokenURL as needed.
func NewOAuthProvider(cfg OAuthConfig, stored *StoredToken) (*OAuthProvider, error) {
	if cfg.ClientID == "" {
		return nil, newSentinel("gsc: oauth config missing client_id")
	}
	if stored == nil || stored.RefreshToken == "" {
		return nil, newSentinel("gsc: oauth requires a stored refresh token (run the consent flow first)")
	}
	conf := cfg.oauth2Config()
	base := stored.toOAuth2()
	// Bind the HTTP client so refreshes hit the configured (test) endpoint.
	ctx := cfg.ctxWithClient(context.Background())
	src := oauth2.ReuseTokenSource(base, conf.TokenSource(ctx, base))
	return &OAuthProvider{cfg: cfg, src: src, stored: stored}, nil
}

// Mode reports the non-secret auth label.
func (p *OAuthProvider) Mode() string { return "oauth" }

// String redacts the provider.
func (p *OAuthProvider) String() string {
	return fmt.Sprintf("gsc.OAuthProvider{client:%s, mode:oauth}", clientIDLabel(p.cfg.ClientID))
}

// GoString redacts the provider under %#v.
func (p *OAuthProvider) GoString() string { return p.String() }

// Token returns a valid access token, refreshing transparently.
func (p *OAuthProvider) Token(_ context.Context) (string, error) {
	tok, err := p.src.Token()
	if err != nil {
		return "", fmt.Errorf("gsc: oauth token refresh: %w", oauthErrorScrub(err))
	}
	return tok.AccessToken, nil
}

// clientIDLabel keeps only the non-secret leading portion of an OAuth client ID
// for logs (the full ID is not a secret, but we trim defensively).
func clientIDLabel(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i] + ".…"
	}
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// oauthErrorScrub strips a refresh-token value out of an oauth2 *RetrieveError
// body if it somehow appears. The oauth2 core returns the raw token-endpoint
// body on a failed refresh; that body is the endpoint's error JSON (no secret),
// but we defensively redact any occurrence of the refresh token is not feasible
// here without the value, so we simply replace the verbose body with its status.
func oauthErrorScrub(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return fmt.Errorf("token endpoint returned %d", re.Response.StatusCode)
	}
	return err
}

// ---------------------------------------------------------------------------
// Consent flow (installed-app) — one-time authorization-code → refresh token.
// ---------------------------------------------------------------------------

// ConsentFlow runs the one-time OAuth2 installed-app consent: build the consent
// URL (offline access + PKCE), then exchange the returned code for a refresh
// token. It is used by `rabbot gsc auth` (a W2 CLI surface); W1 provides the
// reusable mechanics + tests.
type ConsentFlow struct {
	cfg      OAuthConfig
	conf     *oauth2.Config
	verifier string
}

// NewConsentFlow builds a consent flow with a fresh PKCE verifier.
func NewConsentFlow(cfg OAuthConfig) (*ConsentFlow, error) {
	if cfg.ClientID == "" {
		return nil, newSentinel("gsc: oauth config missing client_id")
	}
	return &ConsentFlow{
		cfg:      cfg,
		conf:     cfg.oauth2Config(),
		verifier: oauth2.GenerateVerifier(),
	}, nil
}

// Verifier returns the PKCE code verifier (needed by Exchange; also exposed so a
// caller can persist it across an out-of-band paste flow).
func (f *ConsentFlow) Verifier() string { return f.verifier }

// AuthCodeURL returns the consent URL to open in a browser. It forces offline
// access (so Google returns a refresh token) + re-consent + the PKCE challenge.
func (f *ConsentFlow) AuthCodeURL(state string) string {
	return f.conf.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(f.verifier),
	)
}

// Exchange swaps an authorization code for tokens (sending the PKCE verifier),
// returning a StoredToken ready for 0600 persistence.
func (f *ConsentFlow) Exchange(ctx context.Context, code string) (*StoredToken, error) {
	ctx = f.cfg.ctxWithClient(ctx)
	tok, err := f.conf.Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return nil, fmt.Errorf("gsc: code exchange: %w", oauthErrorScrub(err))
	}
	return storedFromOAuth2(tok), nil
}

// b64url is base64url without padding (JWT/JWS encoding).
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
