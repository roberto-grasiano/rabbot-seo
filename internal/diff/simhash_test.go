package diff

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestHammingDistance(t *testing.T) {
	tests := []struct {
		name string
		a, b uint64
		want int
	}{
		{"identical", 0xABCDEF0123456789, 0xABCDEF0123456789, 0},
		{"one bit", 0x0000000000000000, 0x0000000000000001, 1},
		{"all bits", 0x0000000000000000, 0xFFFFFFFFFFFFFFFF, 64},
		{"three bits", 0x0000000000000000, 0x0000000000000007, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HammingDistance(tc.a, tc.b); got != tc.want {
				t.Errorf("HammingDistance(%x,%x) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestClassifyContentChange(t *testing.T) {
	tests := []struct {
		name      string
		oldHash   uint64
		newHash   uint64
		threshold int
		want      model.ChangeClass
	}{
		{"identical is cosmetic", 0xFF, 0xFF, 4, model.ChangeCosmetic},
		{"within threshold cosmetic", 0x00, 0x07, 4, model.ChangeCosmetic},       // distance 3 <= 4
		{"at threshold cosmetic", 0x00, 0x0F, 4, model.ChangeCosmetic},           // distance 4 <= 4
		{"beyond threshold substantive", 0x00, 0x1F, 4, model.ChangeSubstantive}, // distance 5 > 4
		{"far apart substantive", 0x00, 0xFFFFFFFFFFFFFFFF, 4, model.ChangeSubstantive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyContentChange(tc.oldHash, tc.newHash, tc.threshold); got != tc.want {
				t.Errorf("ClassifyContentChange(%x,%x,%d) = %q, want %q",
					tc.oldHash, tc.newHash, tc.threshold, got, tc.want)
			}
		})
	}
}
