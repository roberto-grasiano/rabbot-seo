package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"math/bits"
	"testing"
)

func TestContentSHA256(t *testing.T) {
	in := "hello world"
	got := ContentSHA256(in)
	sum := sha256.Sum256([]byte(in))
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("ContentSHA256() = %q, want %q", got, want)
	}
}

func TestSimHashIdenticalIsZeroDistance(t *testing.T) {
	a := SimHash("the quick brown fox jumps over the lazy dog and runs away fast")
	b := SimHash("the quick brown fox jumps over the lazy dog and runs away fast")
	if a != b {
		t.Errorf("identical text produced different simhashes: %x vs %x", a, b)
	}
}

func TestSimHashSimilarIsCloser(t *testing.T) {
	base := SimHash("the quick brown fox jumps over the lazy dog and runs away fast in the field")
	similar := SimHash("the quick brown fox jumps over the lazy dog and runs away fast in the meadow")
	different := SimHash("completely unrelated content about astronomy galaxies stars and cosmic radiation today")

	near := bits.OnesCount64(base ^ similar)
	far := bits.OnesCount64(base ^ different)
	if near >= far {
		t.Errorf("similar distance %d should be < different distance %d", near, far)
	}
	if near > 10 {
		t.Errorf("similar text Hamming distance %d unexpectedly large", near)
	}
}

func TestSimHashEmpty(t *testing.T) {
	if SimHash("") != 0 {
		t.Errorf("SimHash(empty) should be 0")
	}
}
