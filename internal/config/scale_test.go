package config

import (
	"testing"
	"time"
)

// TestScaleBySpeed pins the per-site speed dial arithmetic (spec D2):
// effective = base × 100 / pct. 100 = base, 200 = 2× faster (half spacing),
// 50 = half speed (double spacing); a zero/blank/negative pct is treated as 100
// (no scaling) — speed never produces an instant/zero rate.
func TestScaleBySpeed(t *testing.T) {
	tests := []struct {
		name string
		base time.Duration
		pct  int
		want time.Duration
	}{
		{"100 percent is unchanged", 2 * time.Second, 100, 2 * time.Second},
		{"200 percent halves the spacing", 2 * time.Second, 200, 1 * time.Second},
		{"50 percent doubles the spacing", 2 * time.Second, 50, 4 * time.Second},
		{"zero treated as 100", 2 * time.Second, 0, 2 * time.Second},
		{"negative treated as 100", 2 * time.Second, -10, 2 * time.Second},
		{"400 percent quarters the spacing", 2 * time.Second, 400, 500 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scaleBySpeed(tc.base, tc.pct); got != tc.want {
				t.Errorf("scaleBySpeed(%v, %d) = %v, want %v", tc.base, tc.pct, got, tc.want)
			}
		})
	}
}
