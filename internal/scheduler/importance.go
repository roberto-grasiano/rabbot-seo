package scheduler

import "time"

const (
	wDepth   = 0.6
	wSitemap = 0.4
	growth   = 1.5
)

// ColdStartImportance computes the graph-free cold-start importance (§3.2):
// homepage => 1.0; else clamp(0.6/(1+depth) + 0.4*sitemapPriority, 0, 0.99).
func ColdStartImportance(homepage bool, depth int, sitemapPriority float64) float64 {
	if homepage {
		return 1.0
	}
	score := wDepth/float64(1+depth) + wSitemap*sitemapPriority
	if score < 0 {
		return 0
	}
	if score > 0.99 {
		return 0.99
	}
	return score
}

// RecomputeNextCheck returns the new adaptive interval (seconds) and next_check_at.
// On change the interval shrinks toward minInterval; while stable it grows ~1.5x
// toward maxInterval. Always clamped to [minInterval, maxInterval].
func RecomputeNextCheck(prevInterval int64, changed bool, minInterval, maxInterval int64, now time.Time) (int64, time.Time) {
	if prevInterval <= 0 {
		prevInterval = minInterval
	}
	var next int64
	if changed {
		next = prevInterval / 2
	} else {
		next = int64(float64(prevInterval) * growth)
	}
	if next < minInterval {
		next = minInterval
	}
	if next > maxInterval {
		next = maxInterval
	}
	return next, now.Add(time.Duration(next) * time.Second)
}
