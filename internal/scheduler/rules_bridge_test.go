package scheduler

import "testing"

// TestBridgeFieldForRuleMappings is the single-source-of-truth guard for the
// rule_id -> bridged change_type map. It pins the A5 dormant-signal mappings and,
// crucially, the rule IDs that are DELIBERATELY unmapped: those bridge under their
// own rule_id via the BridgeFieldForRule fallback (ok=false), which the production
// ApplyRules adapter (supervisor) relies on. A regression that silently maps one of
// the unmapped IDs to a diff field would let the ingestedTypes dedup swallow its
// alert whenever the diff field changed in the same crawl — exactly the bug the
// unmapped fallback exists to avoid.
func TestBridgeFieldForRuleMappings(t *testing.T) {
	t.Run("mapped rules resolve to their bridged change_type", func(t *testing.T) {
		mapped := map[string]string{
			// Feature A (pre-A5) baseline mappings — pinned so a rename can't drift them.
			"status_regression":        "http_status",
			"indexability_flip":        "indexable",
			"meta_robots_noindex":      "meta_robots",
			"canonical_changed":        "canonical",
			"title_changed":            "title",
			"meta_description_changed": "meta_description",
			"h1_issue":                 "headings",
			"broken_links_spike":       "internal_link_count",
			"hreflang_invalid":         "hreflang",
			// A5 — the four genuine dormant-signal diff-field mappings.
			"external_link_spike":   "external_link_count",
			"image_alt_regression":  "missing_alt_count",
			"image_alt_missing":     "missing_alt_count",
			"redirect_chain_growth": "redirect_chain",
			// A8 — needs_rendering bridges to the render_mode diff field so the warning
			// finding dedups against any standalone render_mode change-stream event in
			// the same crawl: exactly one alert, no rule/change double-fire (acceptance #7).
			"needs_rendering": "render_mode",
		}
		for ruleID, wantField := range mapped {
			field, ok := BridgeFieldForRule(ruleID)
			if !ok {
				t.Errorf("BridgeFieldForRule(%q): ok=false, want mapped to %q", ruleID, wantField)
				continue
			}
			if field != wantField {
				t.Errorf("BridgeFieldForRule(%q) = %q, want %q", ruleID, field, wantField)
			}
		}
	})

	// The rules deliberately left unmapped: each bridges under its own rule_id (the
	// documented fallback). A3's two overflow rules MUST NOT map to title /
	// meta_description, otherwise the same-crawl change-stream event would dedup the
	// overflow alert away. redirect_loop (A5) MUST NOT map to redirect_chain — the
	// redirect_chain alert is retired, and a critical loop bridges under its own id.
	// A8 acceptance #7 (unit half): pin the exact tuple the bridge resolves for the
	// needs_rendering rule. This is the single mechanism that lets the render_mode
	// change event (ingested separately, then SKIPPED so the rule owns the alert) and
	// the needs_rendering warning finding collapse to ONE alert per crawl.
	t.Run("needs_rendering bridges to render_mode", func(t *testing.T) {
		field, ok := BridgeFieldForRule("needs_rendering")
		if !ok || field != "render_mode" {
			t.Errorf("BridgeFieldForRule(needs_rendering) = (%q, %v), want (render_mode, true)", field, ok)
		}
	})

	t.Run("deliberately unmapped rules fall back to their own id", func(t *testing.T) {
		for _, ruleID := range []string{
			"title_pixel_overflow",
			"meta_description_pixel_overflow",
			"redirect_loop",
		} {
			field, ok := BridgeFieldForRule(ruleID)
			if ok {
				t.Errorf("BridgeFieldForRule(%q): ok=true (mapped to %q), want UNMAPPED so it bridges under its own id", ruleID, field)
			}
		}
	})
}
