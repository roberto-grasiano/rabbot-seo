package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

// LoadOrCreateToken returns the bearer token from path, generating and
// persisting a new 256-bit hex token with 0600 perms on first start.
func LoadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(b))
		if tok != "" {
			// Re-tighten an over-permissive existing token file. os.WriteFile's
			// 0600 only applies at create time, so a file that ended up
			// group/world-readable (restore, umask, misconfigured deploy) would
			// otherwise leak the bearer secret on every load (finding
			// Low-token-perms). Best-effort: the token was read successfully, so
			// a Stat/Chmod failure must never turn a good load into an error
			// (doctor separately reports any perm drift). Only perms change.
			if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm() != 0o600 {
				_ = os.Chmod(path, 0o600)
			}
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
