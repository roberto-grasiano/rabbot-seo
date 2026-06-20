package diff

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestCalibrateThresholdPicksSeparating(t *testing.T) {
	// Cosmetic pairs differ by 1-3 bits; substantive pairs by >= 8 bits.
	samples := []CalibrationSample{
		{Old: 0x00, New: 0x01, Expected: model.ChangeCosmetic},
		{Old: 0x00, New: 0x03, Expected: model.ChangeCosmetic},
		{Old: 0x00, New: 0x07, Expected: model.ChangeCosmetic},
		{Old: 0x00, New: 0xFF, Expected: model.ChangeSubstantive},   // 8 bits
		{Old: 0x00, New: 0xFFFF, Expected: model.ChangeSubstantive}, // 16 bits
	}
	got := CalibrateThreshold(samples, 1, 7)
	if got < 3 || got >= 8 {
		t.Errorf("calibrated threshold = %d, want a value that separates (3..7)", got)
	}
	// Verify zero misclassifications at the chosen threshold.
	for _, s := range samples {
		if ClassifyContentChange(s.Old, s.New, got) != s.Expected {
			t.Errorf("threshold %d misclassifies %+v", got, s)
		}
	}
}

func TestCalibrateAccuracy(t *testing.T) {
	samples := []CalibrationSample{
		{Old: 0x00, New: 0x01, Expected: model.ChangeCosmetic},
		{Old: 0x00, New: 0xFFFF, Expected: model.ChangeSubstantive},
	}
	acc := CalibrationAccuracy(samples, 4)
	if acc != 1.0 {
		t.Errorf("accuracy at threshold 4 = %v, want 1.0", acc)
	}
}
