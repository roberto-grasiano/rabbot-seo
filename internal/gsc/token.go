package gsc

import (
	"encoding/json"
	"time"

	"golang.org/x/oauth2"
)

// StoredToken is the on-disk OAuth2 token persisted (0600) after the one-time
// consent. Only the refresh token is strictly required at runtime; the access
// token + expiry are cached to avoid an unnecessary refresh on first use.
//
// SECURITY: AccessToken and RefreshToken are secrets. The custom String/GoString
// redact them so a stray %v/%+v/%#v in a log line cannot leak the token. The
// JSON form (Marshal) intentionally includes them — that is the persisted file,
// written 0600 by the caller via fsatomic, never logged.
type StoredToken struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Marshal serializes the token for 0600 persistence. The secret access/refresh
// tokens are INTENTIONALLY included — this is the on-disk credential file
// (written 0600 via fsatomic by the caller, never logged); redacting them here
// would defeat the persisted-token purpose.
//
//nolint:gosec // G117: deliberate secret serialization to the 0600 token file
func (t *StoredToken) Marshal() ([]byte, error) { return json.Marshal(t) }

// ParseStoredToken deserializes a persisted token file's content.
func ParseStoredToken(b []byte) (*StoredToken, error) {
	var t StoredToken
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// String redacts the secret token fields, reporting only whether each is present
// and the expiry. Used for any accidental log/format of the token.
func (t *StoredToken) String() string {
	return "gsc.StoredToken{access:" + presence(t.AccessToken) +
		", refresh:" + presence(t.RefreshToken) +
		", expiry:" + t.Expiry.Format(time.RFC3339) + "}"
}

// GoString redacts under %#v.
func (t *StoredToken) GoString() string { return t.String() }

func presence(s string) string {
	if s == "" {
		return "absent"
	}
	return "present"
}

// toOAuth2 converts to an oauth2.Token for the token source.
func (t *StoredToken) toOAuth2() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Expiry:       t.Expiry,
	}
}

// storedFromOAuth2 builds a StoredToken from an oauth2.Token (after exchange).
func storedFromOAuth2(tok *oauth2.Token) *StoredToken {
	return &StoredToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}
}
