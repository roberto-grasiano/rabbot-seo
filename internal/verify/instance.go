package verify

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

// instanceKeyBytes is the length of the per-instance secret key: 32 bytes =
// 256 bits, mirroring control.LoadOrCreateToken. The key is the HMAC key behind
// DeriveToken; it is the one thing that makes a token unforgeable by any other
// instance, so it is local secret state — never placed publicly, never logged.
const instanceKeyBytes = 32

// LoadOrCreateInstanceKey returns the per-instance secret key from path, minting
// and persisting a new 256-bit key with 0600 perms on first run. It mirrors
// control.LoadOrCreateToken, but returns raw bytes (the HMAC key) and validates
// length. A present-but-malformed key file FAILS CLOSED (error) rather than
// silently minting a second key — a corrupted key must surface, not silently
// un-verify every site by rotating the identity behind their backs.
func LoadOrCreateInstanceKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		s := strings.TrimSpace(string(b))
		key, derr := hex.DecodeString(s)
		if derr == nil && len(key) == instanceKeyBytes {
			return key, nil
		}
		return nil, errors.New("verify: instance key file is malformed (expected 64 hex chars)")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := make([]byte, instanceKeyBytes)
	if _, rerr := rand.Read(key); rerr != nil {
		return nil, rerr
	}
	if werr := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); werr != nil {
		return nil, werr
	}
	return key, nil
}
