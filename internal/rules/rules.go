// Package rules evaluates snapshots (and their diffs) against a default,
// zero-config SEO rule set, opening/closing model.Issue records keyed
// (url_id, rule_id) with importance-weighted impact_points.
package rules

import (
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// EvalContext is everything a Rule needs to decide pass/fail for one URL.
type EvalContext struct {
	URLID      int64
	Importance float64 // 0..1 cold-start heuristic from model.URL
	New        model.Snapshot
	Old        model.Snapshot // zero on first fetch
	Changes    []model.Change // output of diff.Compare for this fetch
	// Truncated reports that the fetcher cut the response body at its size cap this
	// crawl (ProcessFetch's truncated flag). A truncated body can sever a JSON-LD
	// <script> mid-block, so the New snapshot's structured-data fields are
	// unreliable. Rules that read JSON-LD (the A4 rich-result family) must emit no
	// finding when this is set — the h1_issue "don't guess on unextractable input"
	// precedent. Rules that read only head-derived fields ignore it.
	Truncated bool
}

// Finding is a rule's verdict for one URL. Failed=false means the rule passes
// (any open issue for it must be closed/resolved).
type Finding struct {
	RuleID   string
	Failed   bool
	Severity model.Severity
	Detail   string // JSON-ish human detail; stored on Issue.Detail
}

// Rule is one SEO check. Eval is pure (no DB access).
type Rule interface {
	ID() string
	Eval(ctx EvalContext) Finding
}

// severityWeight maps a severity tier to its fraction of the 0..1000 impact scale.
func severityWeight(sev model.Severity) float64 {
	switch sev {
	case model.SeverityCritical:
		return 1.0
	case model.SeverityWarning:
		return 0.5
	default: // info
		return 0.2
	}
}

// ImpactPoints returns the 0..1000 health-score contribution for an issue:
// importance (clamped 0..1) * severity weight * 1000, rounded.
func ImpactPoints(importance float64, sev model.Severity) int {
	if importance < 0 {
		importance = 0
	}
	if importance > 1 {
		importance = 1
	}
	pts := importance * severityWeight(sev) * 1000.0
	return int(pts + 0.5)
}

// hasChange reports whether the given field appears in the change set.
func hasChange(changes []model.Change, field string) (model.Change, bool) {
	for _, c := range changes {
		if c.Field == field {
			return c, true
		}
	}
	return model.Change{}, false
}

// DefaultRuleSet is the zero-config rule set shipped per site (§5).
func DefaultRuleSet() []Rule {
	return []Rule{
		statusRegressionRule{},
		indexabilityFlipRule{},
		metaRobotsNoindexRule{},
		canonicalChangedRule{},
		titleChangedRule{},
		metaDescriptionChangedRule{},
		h1IssueRule{},
		brokenLinksSpikeRule{},
		hreflangInvalidRule{},
		// A3 — SERP-fit pixel-overflow rules (warning).
		titlePixelOverflowRule{},
		metaDescriptionPixelOverflowRule{},
		// A5 — activated dormant signals.
		externalLinkSpikeRule{},
		imageAltRegressionRule{},
		imageAltMissingRule{},
		redirectChainGrowthRule{},
		redirectLoopRule{},
		// A4 — structured-data validation (Google rich-result eligibility). One
		// parameterized rule struct instantiated per profiled type family, plus the
		// malformed-JSON-LD guard. All four self-suppress on a truncated body.
		richResultRule{typeName: "Product", ruleID: "rich_result_product"},
		richResultRule{typeName: "Article", ruleID: "rich_result_article"},
		richResultRule{typeName: "BreadcrumbList", ruleID: "rich_result_breadcrumb"},
		structuredDataInvalidJSONRule{},
		// A8 — render-mode monitoring honesty. Warns when the persisted render_mode
		// shows the page's SEO content is not fully recoverable from server HTML
		// (head_only_shell / client_shell); closes the issue on recovery to a
		// server_rendered / hydrated mode.
		needsRenderingRule{},
	}
}
