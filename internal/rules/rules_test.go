package rules

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestImpactPoints(t *testing.T) {
	tests := []struct {
		name       string
		importance float64
		severity   model.Severity
		want       int
	}{
		{"critical homepage", 1.0, model.SeverityCritical, 1000},
		{"critical mid", 0.5, model.SeverityCritical, 500},
		{"warning homepage", 1.0, model.SeverityWarning, 500},
		{"info homepage", 1.0, model.SeverityInfo, 200},
		{"zero importance floor", 0.0, model.SeverityCritical, 0},
		{"clamp over 1", 2.0, model.SeverityCritical, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImpactPoints(tc.importance, tc.severity); got != tc.want {
				t.Errorf("ImpactPoints(%v,%q) = %d, want %d", tc.importance, tc.severity, got, tc.want)
			}
		})
	}
}

func TestDefaultRuleSetIsRegistered(t *testing.T) {
	rs := DefaultRuleSet()
	wantIDs := []string{
		"status_regression", "indexability_flip", "meta_robots_noindex",
		"canonical_changed", "title_changed", "meta_description_changed",
		"h1_issue", "broken_links_spike", "hreflang_invalid",
	}
	have := make(map[string]bool)
	for _, r := range rs {
		have[r.ID()] = true
	}
	for _, id := range wantIDs {
		if !have[id] {
			t.Errorf("default rule set missing rule %q", id)
		}
	}
	if len(rs) < len(wantIDs) {
		t.Errorf("default rule set too small: %d rules", len(rs))
	}
}
