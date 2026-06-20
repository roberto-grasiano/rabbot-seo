// Package diff compares a new snapshot to the latest stored snapshot and emits
// model.Change records, classifying main-content changes as cosmetic vs
// substantive via 64-bit SimHash Hamming distance.
package diff

import (
	"math/bits"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// DefaultSimhashThreshold is the starting Hamming-distance cutoff; <= threshold
// is treated as cosmetic churn. Calibrated against the author's sites in Task 12.
const DefaultSimhashThreshold = 4

// HammingDistance returns the number of differing bits between two 64-bit SimHashes.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// ClassifyContentChange returns ChangeCosmetic when the SimHash Hamming distance
// is within threshold (inclusive), else ChangeSubstantive.
func ClassifyContentChange(oldHash, newHash uint64, threshold int) model.ChangeClass {
	if HammingDistance(oldHash, newHash) <= threshold {
		return model.ChangeCosmetic
	}
	return model.ChangeSubstantive
}
