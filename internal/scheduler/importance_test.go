package scheduler

import (
	"testing"
	"time"
)

func TestColdStartImportance(t *testing.T) {
	tests := []struct {
		name            string
		homepage        bool
		depth           int
		sitemapPriority float64
		want            float64
	}{
		{"homepage is 1.0", true, 0, 0.5, 1.0},
		{"depth0 nonhome", false, 0, 0.0, 0.6},      // 0.6/(1+0)+0.4*0
		{"depth1 with sitemap", false, 1, 0.5, 0.5}, // 0.6/2 + 0.4*0.5 = 0.3+0.2
		{"deep low prio", false, 5, 0.0, 0.1},       // 0.6/6 = 0.1
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ColdStartImportance(tc.homepage, tc.depth, tc.sitemapPriority)
			if got < tc.want-0.001 || got > tc.want+0.001 {
				t.Errorf("ColdStartImportance() = %v, want ~%v", got, tc.want)
			}
		})
	}
}

func TestColdStartImportanceClamped(t *testing.T) {
	got := ColdStartImportance(false, 0, 1.0) // 0.6 + 0.4 = 1.0 -> clamp to 0.99
	if got > 0.99 {
		t.Errorf("non-homepage importance = %v, must be clamped to <= 0.99", got)
	}
}

func TestRecomputeNextCheck(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	minI := int64(600)   // 10m
	maxI := int64(86400) // 24h

	// Changed => shrink toward min.
	prev := int64(7200) // 2h
	nextInterval, at := RecomputeNextCheck(prev, true, minI, maxI, now)
	if nextInterval >= prev {
		t.Errorf("changed should shrink interval: prev=%d next=%d", prev, nextInterval)
	}
	if nextInterval < minI {
		t.Errorf("interval below min: %d", nextInterval)
	}
	if !at.Equal(now.Add(time.Duration(nextInterval) * time.Second)) {
		t.Errorf("next_check_at mismatch: %v", at)
	}

	// Stable => grow ~1.5x toward max.
	stableInterval, _ := RecomputeNextCheck(prev, false, minI, maxI, now)
	if stableInterval <= prev {
		t.Errorf("stable should grow interval: prev=%d next=%d", prev, stableInterval)
	}
	if stableInterval > maxI {
		t.Errorf("interval above max: %d", stableInterval)
	}

	// Growth clamps at max.
	clamped, _ := RecomputeNextCheck(maxI, false, minI, maxI, now)
	if clamped != maxI {
		t.Errorf("growth should clamp at max: got %d", clamped)
	}
}
