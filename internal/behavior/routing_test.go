package behavior

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestChangeStreamRoutingContract locks the documented severityForField bucketing
// that several scenarios depend on for their change-stream alert (the cases whose
// VALUE is a field change, not a rule finding). It pins the load-bearing routing
// facts from the engine contract, including the two deliberate coherence wrinkles
// the scenarios call out:
//
//   - hreflang routes WARNING on the change stream, AGREEING with the hreflang_invalid
//     RULE (also WARNING) — the prior severity mismatch was the #16 bug, now FIXED.
//   - a meta_robots change routes CRITICAL even with no indexable flip (so a benign
//     index,nofollow addition surfaces critical via the change stream).
//   - content/headings/schema_types/internal_link_count route WARNING; render_mode
//     routes INFO (history-only; the needs_rendering rule owns its alerting).
//
// FIXED (#16): hreflang now routes WARNING on the change stream, AGREEING with the
// hreflang_invalid RULE (also WARNING) — the prior critical routing paged on any
// hreflang churn while the rule stayed warning, and the bridge dedup then swallowed
// the rule finding. meta_robots still routes CRITICAL even with no indexable flip.
//
// This is the read-only mirror of the scheduler's bucket map; a drift in the real
// scheduler would make a scenario's documented routing claim stale, which this test
// surfaces directly.
func TestChangeStreamRoutingContract(t *testing.T) {
	cases := []struct {
		field string
		want  model.Severity
	}{
		{"indexable", model.SeverityCritical},
		{"indexability_reason", model.SeverityCritical},
		{"canonical", model.SeverityCritical},
		{"meta_robots", model.SeverityCritical},
		{"x_robots_tag", model.SeverityCritical},
		{"http_status", model.SeverityCritical},
		{"hreflang", model.SeverityWarning}, // FIXED (#16): now AGREES with the warning rule
		{"title", model.SeverityWarning},
		{"meta_description", model.SeverityWarning},
		{"headings", model.SeverityWarning},
		{"schema_types", model.SeverityWarning},
		{"redirect_chain", model.SeverityWarning},
		{"internal_link_count", model.SeverityWarning},
		{"content", model.SeverityWarning},
		{"render_mode", model.SeverityInfo}, // history-only; needs_rendering rule alerts
		{"word_count", model.SeverityInfo},
		{"external_link_count", model.SeverityInfo},
	}
	for _, c := range cases {
		if got := severityForField(c.field); got != c.want {
			t.Errorf("severityForField(%q) = %q, want %q", c.field, got, c.want)
		}
	}
}

// TestSubstantiveFieldsRouteToKnownSeverity is a cross-check over EVERY scenario
// that pins a substantive change-stream field set: each such field must route to a
// known severity bucket via the mirror. This ties the mirror to the scenario tables
// (so it is exercised, not dead) and guards against a scenario pinning a field the
// routing table has no opinion on.
func TestSubstantiveFieldsRouteToKnownSeverity(t *testing.T) {
	for _, group := range allScenarioGroups() {
		for _, sc := range group() {
			if sc.wantSubstantive == nil || sc.skip != "" {
				continue
			}
			for _, field := range sc.wantSubstantive {
				sev := severityForField(field)
				switch sev {
				case model.SeverityCritical, model.SeverityWarning, model.SeverityInfo:
				default:
					t.Errorf("[%s] substantive field %q routes to unknown severity %q", sc.name, field, sev)
				}
			}
		}
	}
}
