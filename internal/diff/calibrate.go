package diff

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// CalibrationSample is a labeled before/after SimHash pair for tuning the
// cosmetic/substantive threshold against the author's own sites.
type CalibrationSample struct {
	Old      uint64
	New      uint64
	Expected model.ChangeClass
}

// CalibrationAccuracy returns the fraction of samples classified correctly at the
// given threshold (0..1). Empty input returns 0, which is indistinguishable from a
// genuine 0% score; callers that need to tell "no samples" apart must check
// len(samples) themselves.
func CalibrationAccuracy(samples []CalibrationSample, threshold int) float64 {
	if len(samples) == 0 {
		return 0
	}
	correct := 0
	for _, s := range samples {
		if ClassifyContentChange(s.Old, s.New, threshold) == s.Expected {
			correct++
		}
	}
	return float64(correct) / float64(len(samples))
}

// CalibrateThreshold sweeps thresholds in [lo,hi] and returns the one with the
// highest accuracy on the labeled samples (lowest threshold wins ties, favoring
// fewer false-cosmetic suppressions of real changes). With empty input every
// candidate scores 0 (see CalibrationAccuracy), so it silently returns lo.
func CalibrateThreshold(samples []CalibrationSample, lo, hi int) int {
	best, bestAcc := lo, -1.0
	for th := lo; th <= hi; th++ {
		if acc := CalibrationAccuracy(samples, th); acc > bestAcc {
			bestAcc, best = acc, th
		}
	}
	return best
}
