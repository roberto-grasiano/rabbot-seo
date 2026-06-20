package store

import (
	"math"
	"testing"
)

// TestRequiredCoverageMatchesConstant pins the integer formula in
// requiredCoverage to MinScoreCoverage (PR #76 review nit): the formula is
// hardcoded for 0.5, and this test fails if either side drifts without the
// other — bumping the constant silently un-gates cold-start scores otherwise.
func TestRequiredCoverageMatchesConstant(t *testing.T) {
	for known := 0; known <= 1000; known++ {
		want := int(math.Ceil(MinScoreCoverage * float64(known)))
		if known <= 0 {
			want = 0
		}
		if got := requiredCoverage(known); got != want {
			t.Fatalf("requiredCoverage(%d) = %d, want ceil(MinScoreCoverage*known) = %d — formula and constant drifted", known, got, want)
		}
	}
}
