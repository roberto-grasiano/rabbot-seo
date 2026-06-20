package scheduler

// ruleFieldForBridge maps a rule_id to the diff-field/change_type the rule
// bridge (Feature A) uses as the alert change_type. Keeping the bridged
// change_type in the diff-field namespace lets ProcessFetch dedup a finding
// against a change-stream event on the same field, so a field that trips BOTH a
// rule and a diff event alerts exactly once.
//
// This is the single source of truth: the production ApplyRules adapter
// (supervisor) and the e2e test both resolve through BridgeFieldForRule, so the
// mapping can never silently drift between them.
var ruleFieldForBridge = map[string]string{
	"status_regression":        "http_status",
	"indexability_flip":        "indexable",
	"meta_robots_noindex":      "meta_robots",
	"canonical_changed":        "canonical",
	"title_changed":            "title",
	"meta_description_changed": "meta_description",
	"h1_issue":                 "headings",
	"broken_links_spike":       "internal_link_count",
	"hreflang_invalid":         "hreflang",

	// A5 — activated dormant signals. Each maps to the diff field whose change
	// records the same fact, so a finding dedups against any standalone change-stream
	// event on that field in the same crawl (one alert per field per crawl).
	"external_link_spike":   "external_link_count",
	"image_alt_regression":  "missing_alt_count",
	"image_alt_missing":     "missing_alt_count",
	"redirect_chain_growth": "redirect_chain",

	// A8 — needs_rendering bridges to the render_mode diff field. The render_mode
	// change event itself routes Info (severityForField default) AND is SKIPPED by
	// ProcessFetch's change-stream ingest loop (so it never raises a standalone
	// alert — render-mode flips are history, not noise). The needs_rendering WARNING
	// finding owns the single alert, bridged here under render_mode so it still
	// dedups against the (skipped) change event: exactly one alert per crawl.
	"needs_rendering": "render_mode",
	// redirect_loop is deliberately UNMAPPED: it bridges under its own rule_id via
	// the BridgeFieldForRule fallback (the same mechanism A3's overflow rules use).
	// Mapping it to redirect_chain is wrong on two counts — the standalone
	// redirect_chain alert is retired (ProcessFetch skips it; the parsed rules own
	// redirect alerting), and a critical loop must surface as its own change_type.
	//
	// A3's title_pixel_overflow / meta_description_pixel_overflow are likewise
	// unmapped on purpose: mapping them to title / meta_description would let a
	// same-crawl title/description change dedup the overflow alert away. A title
	// edited INTO overflow is two distinct facts (it changed AND it no longer fits),
	// so both alert — the overflow under its own change_type carrying the px numbers.
}

// BridgeFieldForRule returns the diff-field/change_type a rule_id bridges to, and
// whether the rule_id is mapped. Callers fall back to the rule_id itself when ok
// is false (an unmapped rule bridges under its own id).
func BridgeFieldForRule(ruleID string) (string, bool) {
	field, ok := ruleFieldForBridge[ruleID]
	return field, ok
}
