package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strings"
)

// ContentSHA256 returns the lowercase hex SHA-256 of the input text.
func ContentSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// SimHash computes a 64-bit Charikar SimHash over whitespace-tokenized text.
// Identical text -> identical hash; small edits -> small Hamming distance.
//
// Tokens are summed order-insensitively, so a pure block reorder (same words,
// rearranged) yields the same SimHash and reads as cosmetic to similarity-based
// diffing. That is intentional for near-duplicate detection — but it means
// SimHash alone cannot flag a reorder. The exact ContentSHA256 still differs on
// any reorder, so callers that must detect reordering should compare the SHA.
func SimHash(text string) uint64 {
	tokens := strings.Fields(strings.ToLower(text))
	if len(tokens) == 0 {
		return 0
	}
	var v [64]int
	for _, tok := range tokens {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		feature := h.Sum64()
		for i := 0; i < 64; i++ {
			if feature&(uint64(1)<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	var fingerprint uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			fingerprint |= uint64(1) << uint(i)
		}
	}
	return fingerprint
}
