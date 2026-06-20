package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"net/url"
	"strings"
)

// DeriveToken returns the proof token for a site, bound to this instance's
// secret key: rab_ + base32(HMAC-SHA256(key, canonicalHost(host))). It is
// deterministic per (key, host) — so placement is idempotent and a re-run shows
// the same token — and unique per instance (different key) and per host. The
// token is opaque and reveals nothing about the key. Callers load the key via
// LoadOrCreateInstanceKey; an empty key must never reach production verification
// (Verify guards len(Key)==0 and fails safe).
func DeriveToken(key []byte, host string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonicalHost(host)))
	sum := mac.Sum(nil)
	body := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum)
	return TokenPrefix + body
}

// canonicalHost normalizes the host the token is bound to so every caller (the
// wizard, the verify command, the daemon re-verify) derives the SAME token for
// the same site: lowercase, no surrounding space, no trailing dot, default ports
// (443/80) dropped, a non-default port kept. The token value is method-agnostic
// — the same string is placed in the meta tag, the .well-known file, or the DNS
// TXT record; only the placement surface differs.
func canonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	u := &url.URL{Host: h}
	switch u.Port() {
	case "443", "80":
		return u.Hostname()
	default:
		return h
	}
}
