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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// genServiceAccountKey builds an in-memory service-account JSON key (PKCS#8 PEM)
// with an ephemeral RSA-2048 key, so no real credential is ever needed.
func genServiceAccountKey(t *testing.T, tokenURI string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	doc := map[string]string{
		"type":           "service_account",
		"client_email":   "rabbot@proj.iam.gserviceaccount.com",
		"private_key":    string(pemBytes),
		"private_key_id": "kid-1",
		"token_uri":      tokenURI,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal SA doc: %v", err)
	}
	return raw, key
}

// decodeJWTSegment base64url-decodes one JWT segment.
func decodeJWTSegment(t *testing.T, seg string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode JWT segment: %v", err)
	}
	return b
}

func TestServiceAccountProvider_MintsAndExchangesJWT(t *testing.T) {
	var gotGrant, gotAssertion string
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotAssertion = r.Form.Get("assertion")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.fresh","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	keyJSON, pub := genServiceAccountKey(t, srv.URL)
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}
	if p.Mode() != "service_account" {
		t.Errorf("Mode() = %q, want service_account", p.Mode())
	}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ya29.fresh" {
		t.Errorf("token = %q, want ya29.fresh", tok)
	}

	// Exchange used the JWT-bearer grant.
	if gotGrant != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("grant_type = %q", gotGrant)
	}

	// The assertion must be a valid RS256 JWT signed by the SA key with the right claims.
	parts := strings.Split(gotAssertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(decodeJWTSegment(t, parts[0]), &hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if hdr.Alg != "RS256" || hdr.Typ != "JWT" {
		t.Errorf("header alg/typ = %q/%q, want RS256/JWT", hdr.Alg, hdr.Typ)
	}
	if hdr.Kid != "kid-1" {
		t.Errorf("header kid = %q, want kid-1 (from private_key_id)", hdr.Kid)
	}
	var claims struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
		Iat   int64  `json:"iat"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal(decodeJWTSegment(t, parts[1]), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Iss != "rabbot@proj.iam.gserviceaccount.com" {
		t.Errorf("iss = %q", claims.Iss)
	}
	if claims.Scope != scopeWebmastersReadonly {
		t.Errorf("scope = %q, want %q", claims.Scope, scopeWebmastersReadonly)
	}
	if claims.Aud != srv.URL {
		t.Errorf("aud = %q, want %q (token_uri)", claims.Aud, srv.URL)
	}
	wantIat := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC).Unix()
	if claims.Iat != wantIat {
		t.Errorf("iat = %d, want %d", claims.Iat, wantIat)
	}
	// exp must be clamped to <= 1h after iat (Google's cap).
	if claims.Exp <= claims.Iat || claims.Exp-claims.Iat > 3600 {
		t.Errorf("exp-iat = %d, want (0, 3600]", claims.Exp-claims.Iat)
	}

	// Verify the signature with the SA public key (RS256 = SHA256 + PKCS1v15).
	signingInput := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signingInput))
	sig := decodeJWTSegment(t, parts[2])
	if err := rsa.VerifyPKCS1v15(&pub.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify against the SA key: %v", err)
	}
}

func TestServiceAccountProvider_CachesUntilExpiry(t *testing.T) {
	var exchanges int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges++
		_, _ = io.WriteString(w, `{"access_token":"ya29.cached","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	keyJSON, _ := genServiceAccountKey(t, srv.URL)
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := p.Token(context.Background()); err != nil {
			t.Fatalf("Token #%d: %v", i, err)
		}
	}
	if exchanges != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cached until expiry)", exchanges)
	}
}

// TestServiceAccountProvider_ReMintsAfterExpiry is the falsifiable clock-axis test:
// the cached token's Expiry is computed against the SAME injectable clock the
// freshness check reads, so advancing the fake clock past expiry forces a fresh
// exchange. Before expiry, Token returns the cached value without a new endpoint hit.
// (With exchangeAssertion computing Expiry on a bare time.Now(), the cached Expiry
// would sit ~1h in the real wall-clock future while p.now() stayed frozen, so the
// re-mint branch could never be reached — this test would be impossible to write.)
func TestServiceAccountProvider_ReMintsAfterExpiry(t *testing.T) {
	var exchanges int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges++
		// expires_in is short so the fake clock can step past it deterministically.
		_, _ = io.WriteString(w, `{"access_token":"ya29.minted","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	keyJSON, _ := genServiceAccountKey(t, srv.URL)

	// A mutable fake clock the test advances by hand.
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}

	// 1) First mint hits the endpoint once.
	if _, err := p.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("first mint: endpoint hit %d times, want 1", exchanges)
	}

	// 2) Advance the clock a little (still well inside the 3600s lifetime, even
	// accounting for the ~60s freshness headroom): the cached token is returned
	// with NO new exchange.
	clock = clock.Add(30 * time.Minute)
	if _, err := p.Token(context.Background()); err != nil {
		t.Fatalf("cached Token: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("within lifetime: endpoint hit %d times, want still 1 (served from cache)", exchanges)
	}

	// 3) Advance the clock PAST the expiry (3600s + headroom): the next Token must
	// re-mint, hitting the endpoint a second time. This is the branch the clock fix
	// makes reachable — the expiry was computed on this same fake clock.
	clock = clock.Add(2 * time.Hour)
	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("re-mint Token: %v", err)
	}
	if tok != "ya29.minted" {
		t.Errorf("re-minted token = %q, want ya29.minted", tok)
	}
	if exchanges != 2 {
		t.Fatalf("after expiry: endpoint hit %d times, want 2 (re-minted)", exchanges)
	}
}

func TestServiceAccountProvider_RejectsNonServiceAccountJSON(t *testing.T) {
	_, err := NewServiceAccountProvider([]byte(`{"type":"authorized_user","client_id":"x"}`), ServiceAccountOptions{})
	if err == nil {
		t.Fatal("expected an error for a non-service_account key file")
	}
}

func TestServiceAccountProvider_RejectsBadPEM(t *testing.T) {
	doc := `{"type":"service_account","client_email":"a@b","private_key":"-----BEGIN PRIVATE KEY-----\nnotbase64\n-----END PRIVATE KEY-----\n"}`
	_, err := NewServiceAccountProvider([]byte(doc), ServiceAccountOptions{})
	if err == nil {
		t.Fatal("expected an error for an unparseable private key")
	}
}

func TestServiceAccountProvider_PropagatesExchangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
	}))
	t.Cleanup(srv.Close)

	keyJSON, _ := genServiceAccountKey(t, srv.URL)
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}
	if _, err := p.Token(context.Background()); err == nil {
		t.Fatal("expected the token-exchange failure to propagate")
	}
}

func TestOAuthProvider_RefreshesAccessToken(t *testing.T) {
	var gotGrant, gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotRefresh = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.oauth","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	p, err := NewOAuthProvider(OAuthConfig{
		ClientID:     "client-id.apps.googleusercontent.com",
		ClientSecret: "shh",
		TokenURL:     srv.URL,
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}, &StoredToken{RefreshToken: "1//refresh-tok"})
	if err != nil {
		t.Fatalf("NewOAuthProvider: %v", err)
	}
	if p.Mode() != "oauth" {
		t.Errorf("Mode() = %q, want oauth", p.Mode())
	}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ya29.oauth" {
		t.Errorf("token = %q, want ya29.oauth", tok)
	}
	if gotGrant != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotGrant)
	}
	if gotRefresh != "1//refresh-tok" {
		t.Errorf("refresh_token sent = %q", gotRefresh)
	}
}

func TestOAuthProvider_RequiresRefreshToken(t *testing.T) {
	_, err := NewOAuthProvider(OAuthConfig{ClientID: "x", ClientSecret: "y"}, &StoredToken{})
	if err == nil {
		t.Fatal("expected an error when no refresh token is present")
	}
}

func TestOAuthConsentURL_HasOfflineAndPKCE(t *testing.T) {
	cfg := OAuthConfig{
		ClientID:    "client-id.apps.googleusercontent.com",
		AuthURL:     "https://accounts.google.com/o/oauth2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		RedirectURL: "http://127.0.0.1:0/callback",
	}
	flow, err := NewConsentFlow(cfg)
	if err != nil {
		t.Fatalf("NewConsentFlow: %v", err)
	}
	consentURL := flow.AuthCodeURL("state-123")
	u, err := url.Parse(consentURL)
	if err != nil {
		t.Fatalf("parse consent URL: %v", err)
	}
	q := u.Query()
	if q.Get("access_type") != "offline" {
		t.Errorf("access_type = %q, want offline (so a refresh token is returned)", q.Get("access_type"))
	}
	if q.Get("scope") != scopeWebmastersReadonly {
		t.Errorf("scope = %q, want %q", q.Get("scope"), scopeWebmastersReadonly)
	}
	if q.Get("state") != "state-123" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE challenge missing: challenge=%q method=%q", q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
	if q.Get("prompt") != "consent" && q.Get("approval_prompt") != "force" {
		t.Errorf("consent URL should force re-consent so a refresh token is always issued; query=%v", q)
	}
	// The verifier must be retained for Exchange (PKCE proof).
	if flow.Verifier() == "" {
		t.Error("Verifier() is empty; Exchange needs the PKCE verifier")
	}
}

func TestOAuthConsentFlow_ExchangesCode(t *testing.T) {
	var gotCode, gotVerifier, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotCode = r.Form.Get("code")
		gotVerifier = r.Form.Get("code_verifier")
		gotGrant = r.Form.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.first","refresh_token":"1//new-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	cfg := OAuthConfig{
		ClientID:     "client-id.apps.googleusercontent.com",
		ClientSecret: "shh",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     srv.URL,
		RedirectURL:  "http://127.0.0.1:0/callback",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}
	flow, err := NewConsentFlow(cfg)
	if err != nil {
		t.Fatalf("NewConsentFlow: %v", err)
	}
	_ = flow.AuthCodeURL("state-123") // sets/uses the verifier
	stored, err := flow.Exchange(context.Background(), "auth-code-xyz")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotGrant != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", gotGrant)
	}
	if gotCode != "auth-code-xyz" {
		t.Errorf("code = %q", gotCode)
	}
	if gotVerifier == "" {
		t.Errorf("code_verifier not sent on exchange (PKCE)")
	}
	if stored.RefreshToken != "1//new-refresh" {
		t.Errorf("stored refresh = %q, want 1//new-refresh", stored.RefreshToken)
	}
	if stored.AccessToken != "ya29.first" {
		t.Errorf("stored access = %q", stored.AccessToken)
	}
	if stored.Expiry.IsZero() {
		t.Error("stored expiry is zero; should be populated from expires_in")
	}
}

func TestStoredToken_RoundTripsJSON(t *testing.T) {
	in := &StoredToken{
		AccessToken:  "ya29.x",
		RefreshToken: "1//y",
		TokenType:    "Bearer",
		Expiry:       time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC),
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := ParseStoredToken(b)
	if err != nil {
		t.Fatalf("ParseStoredToken: %v", err)
	}
	if out.RefreshToken != in.RefreshToken || out.AccessToken != in.AccessToken || !out.Expiry.Equal(in.Expiry) {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, in)
	}
}
