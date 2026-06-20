package gsc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rsaGenerate(t *testing.T) (*rsa.PrivateKey, error) {
	t.Helper()
	return rsa.GenerateKey(rand.Reader, 2048)
}

func pemEncodePKCS1(key *rsa.PrivateKey) string {
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestIsRetryable_5xxAndNonAPI exercises the 5xx (retryable) branch and the
// non-APIError branch (not retryable).
func TestIsRetryable_5xxAndNonAPI(t *testing.T) {
	if !IsRetryable(&APIError{HTTPStatus: 503, Code: 503, Status: "UNAVAILABLE"}) {
		t.Error("IsRetryable(503) = false, want true")
	}
	if !IsRetryable(&APIError{HTTPStatus: 500, Code: 500}) {
		t.Error("IsRetryable(500) = false, want true")
	}
	if IsRetryable(&APIError{HTTPStatus: 403, Code: 403, Status: "PERMISSION_DENIED"}) {
		t.Error("IsRetryable(403) = true, want false (permanent)")
	}
	if IsRetryable(errors.New("plain error")) {
		t.Error("IsRetryable(non-APIError) = true, want false")
	}
}

// TestAPIError_ErrorString covers both the with-status and no-status branches.
func TestAPIError_ErrorString(t *testing.T) {
	withStatus := (&APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota"}).Error()
	if !strings.Contains(withStatus, "RESOURCE_EXHAUSTED") || !strings.Contains(withStatus, "429") {
		t.Errorf("with-status Error() = %q", withStatus)
	}
	noStatus := (&APIError{Code: 500, Message: "boom"}).Error()
	if strings.Contains(noStatus, "()") || !strings.Contains(noStatus, "500") {
		t.Errorf("no-status Error() = %q", noStatus)
	}
}

// TestScrubTransportErr removes a bearer that leaks into a transport error.
func TestScrubTransportErr(t *testing.T) {
	const bearer = "ya29.SECRETLEAK"
	leaky := errors.New("dial failed with token " + bearer + " attached")
	got := scrubTransportErr(leaky, bearer)
	if strings.Contains(got.Error(), bearer) {
		t.Errorf("scrubTransportErr left the bearer in: %s", got.Error())
	}
	if !strings.Contains(got.Error(), "<redacted>") {
		t.Errorf("scrubTransportErr did not redact: %s", got.Error())
	}
	// Empty bearer is a passthrough (identity-preserving).
	orig := errors.New("some error")
	if !errors.Is(scrubTransportErr(orig, ""), orig) {
		t.Error("empty bearer should pass the error through unchanged")
	}
	// A non-leaky error is returned unchanged (identity-preserving).
	clean := errors.New("clean transport error")
	if !errors.Is(scrubTransportErr(clean, bearer), clean) {
		t.Error("non-leaky error should be returned unchanged")
	}
}

// TestOAuthErrorScrub collapses an oauth2 *RetrieveError into a status-only error.
func TestOAuthErrorScrub(t *testing.T) {
	// A non-RetrieveError passes through unchanged (identity-preserving).
	plain := errors.New("network down")
	if !errors.Is(oauthErrorScrub(plain), plain) {
		t.Error("non-RetrieveError should pass through unchanged")
	}
	// Drive a real *oauth2.RetrieveError through a failing refresh and assert the
	// scrubbed error carries only the status code, not the endpoint body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// The body would carry "invalid_grant" detail; the scrub must drop it.
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked. REFRESHSECRET"}`)
	}))
	t.Cleanup(srv.Close)

	p, err := NewOAuthProvider(OAuthConfig{
		ClientID:     "id.apps.googleusercontent.com",
		ClientSecret: "shh",
		TokenURL:     srv.URL,
		HTTPClient:   &http.Client{},
	}, &StoredToken{RefreshToken: "1//REFRESHSECRET"})
	if err != nil {
		t.Fatalf("NewOAuthProvider: %v", err)
	}
	_, terr := p.Token(context.Background())
	if terr == nil {
		t.Fatal("expected a refresh failure")
	}
	if strings.Contains(terr.Error(), "REFRESHSECRET") {
		t.Errorf("oauth refresh error leaked secret detail: %s", terr.Error())
	}
	if !strings.Contains(terr.Error(), "400") {
		t.Errorf("scrubbed oauth error should report the status code, got: %s", terr.Error())
	}
}

// TestClientIDLabel covers the dotted, long, and short branches.
func TestClientIDLabel(t *testing.T) {
	if got := clientIDLabel("client-id.apps.googleusercontent.com"); got != "client-id.…" {
		t.Errorf("dotted label = %q, want client-id.…", got)
	}
	if got := clientIDLabel("0123456789abcdef"); got != "01234567…" {
		t.Errorf("long no-dot label = %q", got)
	}
	if got := clientIDLabel("short"); got != "short" {
		t.Errorf("short label = %q, want short", got)
	}
}

// TestNewClient_DefaultsAndNilToken covers the nil-HTTPClient default path and
// the missing-token guard.
func TestNewClient_DefaultsAndNilToken(t *testing.T) {
	if _, err := NewClient(Options{}); err == nil {
		t.Error("NewClient with no TokenProvider should error")
	}
	c, err := NewClient(Options{Token: &staticProvider{token: "t"}}) // nil HTTPClient → default
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.http == nil {
		t.Error("nil HTTPClient should default to a non-nil client")
	}
	if c.http.Timeout == 0 {
		t.Error("default client must have a non-zero timeout (project non-negotiable)")
	}
	// Default base URLs point at the real Google hosts.
	if c.baseURL != defaultWebmasters || c.inspectBaseURL != defaultInspect {
		t.Errorf("default base URLs = %q / %q", c.baseURL, c.inspectBaseURL)
	}
}

// TestDecodeAPIError_NonJSONBody falls back to the HTTP status when the error
// body is not the expected envelope.
func TestDecodeAPIError_NonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>502 Bad Gateway</html>`) // not JSON
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	_, err := c.ListSites(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusBadGateway || apiErr.Code != http.StatusBadGateway {
		t.Errorf("APIError = %+v, want HTTP/Code 502", apiErr)
	}
	if apiErr.Message == "" {
		t.Error("APIError.Message should fall back to the HTTP status text")
	}
	// A 502 is a 5xx → retryable.
	if !IsRetryable(err) {
		t.Error("IsRetryable(502) = false, want true")
	}
}

// TestInspectURL_RequiresInspectionURL covers the empty-URL guard.
func TestInspectURL_RequiresInspectionURL(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "t")
	if _, err := c.InspectURL(context.Background(), "https://ex.com/", ""); err == nil {
		t.Error("InspectURL with empty inspection URL should error")
	}
}

// TestRedactURL strips a query string (defensive secret hygiene on errors).
func TestRedactURL(t *testing.T) {
	if got := redactURL("https://h/path?key=SECRET"); got != "https://h/path" {
		t.Errorf("redactURL = %q, want https://h/path", got)
	}
	if got := redactURL("https://h/path"); got != "https://h/path" {
		t.Errorf("redactURL(no query) = %q", got)
	}
}

// TestParseStoredToken_Malformed covers the decode-error branch.
func TestParseStoredToken_Malformed(t *testing.T) {
	if _, err := ParseStoredToken([]byte(`{not json`)); err == nil {
		t.Error("ParseStoredToken should error on malformed JSON")
	}
}

// TestServiceAccountProvider_AcceptsPKCS1Key covers the PKCS#1 fallback in
// parseRSAPrivateKey (older service-account keys, or RSA PRIVATE KEY blocks).
func TestServiceAccountProvider_AcceptsPKCS1Key(t *testing.T) {
	key, err := rsaGenerate(t)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	pkcs1 := pemEncodePKCS1(key)
	doc := `{"type":"service_account","client_email":"a@b.iam.gserviceaccount.com","private_key":` +
		jsonString(pkcs1) + `}`
	p, perr := NewServiceAccountProvider([]byte(doc), ServiceAccountOptions{})
	if perr != nil {
		t.Fatalf("NewServiceAccountProvider with a PKCS#1 key: %v", perr)
	}
	if p.Mode() != "service_account" {
		t.Errorf("Mode() = %q", p.Mode())
	}
}

// TestErrorSummary covers all three branches of the token-endpoint error summary:
// code+description, code-only, and the no-detail default. The summary must never echo
// the request (it only relays the endpoint's own error text).
func TestErrorSummary(t *testing.T) {
	both := tokenResponse{ErrorCode: "invalid_grant", ErrorDescription: "Invalid JWT Signature."}
	if got := both.errorSummary(); got != "invalid_grant: Invalid JWT Signature." {
		t.Errorf("code+desc summary = %q", got)
	}
	codeOnly := tokenResponse{ErrorCode: "invalid_client"}
	if got := codeOnly.errorSummary(); got != "invalid_client" {
		t.Errorf("code-only summary = %q", got)
	}
	none := tokenResponse{}
	if got := none.errorSummary(); got != "no error detail" {
		t.Errorf("empty summary = %q, want a non-empty placeholder", got)
	}
}

// TestOAuthConfig_TokenURLOverride covers tokenURL's override branch (default is
// already exercised elsewhere): a set TokenURL wins over the Google default.
func TestOAuthConfig_TokenURLOverride(t *testing.T) {
	cfg := OAuthConfig{TokenURL: "https://example.test/token"}
	if got := cfg.tokenURL(); got != "https://example.test/token" {
		t.Errorf("tokenURL override = %q, want the configured value", got)
	}
	if got := (OAuthConfig{}).tokenURL(); got != defaultTokenURI {
		t.Errorf("default tokenURL = %q, want %q", got, defaultTokenURI)
	}
	// authURL default + override too.
	if got := (OAuthConfig{}).authURL(); got != defaultAuthURL {
		t.Errorf("default authURL = %q, want %q", got, defaultAuthURL)
	}
	if got := (OAuthConfig{AuthURL: "https://a.test/auth"}).authURL(); got != "https://a.test/auth" {
		t.Errorf("authURL override = %q", got)
	}
}

// TestStoredToken_PresenceBothArms drives the present arm of presence() (the absent
// arm is exercised by the round-trip test) via the redacting String().
func TestStoredToken_PresenceBothArms(t *testing.T) {
	present := (&StoredToken{AccessToken: "a", RefreshToken: "r"}).String()
	if !strings.Contains(present, "access:present") || !strings.Contains(present, "refresh:present") {
		t.Errorf("present tokens must render as present: %q", present)
	}
	if strings.Contains(present, "a") && strings.Contains(present, "\"a\"") {
		t.Errorf("String must not leak the raw token value: %q", present)
	}
	absent := (&StoredToken{}).String()
	if !strings.Contains(absent, "access:absent") || !strings.Contains(absent, "refresh:absent") {
		t.Errorf("absent tokens must render as absent: %q", absent)
	}
}

// TestNewConsentFlow_RequiresClientID covers the missing-client-id guard.
func TestNewConsentFlow_RequiresClientID(t *testing.T) {
	if _, err := NewConsentFlow(OAuthConfig{}); err == nil {
		t.Error("NewConsentFlow with no client id must error")
	}
}

// TestNewServiceAccountProvider_RequiresClientEmail covers the missing-client_email
// guard (valid type, valid-looking JSON, but no client_email).
func TestNewServiceAccountProvider_RequiresClientEmail(t *testing.T) {
	doc := `{"type":"service_account","private_key":"x"}`
	if _, err := NewServiceAccountProvider([]byte(doc), ServiceAccountOptions{}); err == nil {
		t.Error("a service-account key with no client_email must error")
	}
}

// TestNewServiceAccountProvider_RejectsMalformedJSON covers the json.Unmarshal error.
func TestNewServiceAccountProvider_RejectsMalformedJSON(t *testing.T) {
	if _, err := NewServiceAccountProvider([]byte(`{not json`), ServiceAccountOptions{}); err == nil {
		t.Error("malformed JSON must error")
	}
}

// TestValidateProperty_EdgeCases covers validateProperty's empty, sc-domain (good and
// empty-host), and URL-prefix (good and bad-scheme) arms via the client's public path.
func TestValidateProperty_EdgeCases(t *testing.T) {
	good := []string{"https://ex.com/", "http://ex.com/", "sc-domain:ex.com"}
	for _, p := range good {
		if err := validateProperty(p); err != nil {
			t.Errorf("validateProperty(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{"", "sc-domain:", "ex.com", "ftp://ex.com/", "://nohost"}
	for _, p := range bad {
		if err := validateProperty(p); err == nil {
			t.Errorf("validateProperty(%q) = nil, want an error", p)
		}
	}
}
