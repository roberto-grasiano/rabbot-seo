package gsc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// secretSentinels are unique tokens we plant in credentials; if any appears in a
// log line, an error string, or a struct's default formatting, the no-secret-log
// invariant (CLAUDE.md security surface) is violated.
const (
	secretAccessToken  = "ACCESSTOKENSENTINEL12345"
	secretRefreshToken = "REFRESHTOKENSENTINEL67890"
	secretClientSecret = "CLIENTSECRETSENTINELabcde"
	secretPrivateKeyID = "PRIVATEKEYIDSENTINELfghij"
)

func saKeyWithSecrets(t *testing.T, tokenURI string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	doc := map[string]string{
		"type":           "service_account",
		"client_email":   "sa@proj.iam.gserviceaccount.com",
		"private_key":    string(pemBytes),
		"private_key_id": secretPrivateKeyID,
		"token_uri":      tokenURI,
	}
	raw, _ := json.Marshal(doc)
	return raw
}

// assertNoSecret fails if s contains any planted secret sentinel or the PEM
// private-key marker.
func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	for _, secret := range []string{
		secretAccessToken, secretRefreshToken, secretClientSecret, secretPrivateKeyID,
		"PRIVATE KEY", "BEGIN PRIVATE",
	} {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaked a secret (%q) in: %s", where, secret, s)
		}
	}
}

func TestServiceAccountProvider_NeverFormatsSecret(t *testing.T) {
	keyJSON := saKeyWithSecrets(t, "https://oauth2.googleapis.com/token")
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}
	// Default %v / %+v / %#v formatting must not expose the PEM, key id, etc.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assertNoSecret(t, "ServiceAccountProvider "+verb, fmt.Sprintf(verb, p))
	}
}

func TestOAuthProvider_NeverFormatsSecret(t *testing.T) {
	p, err := NewOAuthProvider(OAuthConfig{
		ClientID:     "id.apps.googleusercontent.com",
		ClientSecret: secretClientSecret,
		TokenURL:     "https://oauth2.googleapis.com/token",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
	}, &StoredToken{RefreshToken: secretRefreshToken, AccessToken: secretAccessToken})
	if err != nil {
		t.Fatalf("NewOAuthProvider: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assertNoSecret(t, "OAuthProvider "+verb, fmt.Sprintf(verb, p))
	}
}

func TestStoredToken_NeverFormatsSecret(t *testing.T) {
	tok := &StoredToken{
		AccessToken:  secretAccessToken,
		RefreshToken: secretRefreshToken,
		TokenType:    "Bearer",
		Expiry:       time.Now(),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assertNoSecret(t, "StoredToken "+verb, fmt.Sprintf(verb, tok))
	}
}

func TestServiceAccountProvider_ExchangeErrorScrubsAssertion(t *testing.T) {
	// On a token-exchange failure the returned error must NOT embed the JWT
	// assertion (it is signed with the private key and is bearer-equivalent).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(http.StatusUnauthorized)
		// Echo nothing secret back; the failure body is generic.
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(srv.Close)

	keyJSON := saKeyWithSecrets(t, srv.URL)
	p, err := NewServiceAccountProvider(keyJSON, ServiceAccountOptions{HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	if err != nil {
		t.Fatalf("NewServiceAccountProvider: %v", err)
	}
	_, terr := p.Token(context.Background())
	if terr == nil {
		t.Fatal("expected an exchange error")
	}
	// The error must not carry the signed assertion or any private-key material.
	assertNoSecret(t, "exchange error", terr.Error())
}

func TestClient_ErrorDoesNotEmbedBearer(t *testing.T) {
	// Even on an API error, the client error must not echo the Authorization
	// bearer it sent (the access token is a live secret).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, secretAccessToken)
	_, err := c.ListSites(context.Background())
	if err == nil {
		t.Fatal("expected an API error")
	}
	if strings.Contains(err.Error(), secretAccessToken) {
		t.Errorf("client error leaked the bearer token: %s", err.Error())
	}
}
