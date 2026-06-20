// Package behavior is a synthetic behavioral golden suite that stress-tests the
// per-site-type SIGNAL/NOISE behavior of the Rabbot SEO change/regression engine.
//
// It is a TEST-ONLY package: the single non-test file here exists so that
// `go test ./internal/behavior/...` builds and so the shared harness types and
// the pure (old,new)->findings driver live in one place. It depends only on
// internal/diff, internal/rules, and internal/model (all read-only here).
//
// The contract under test (verified against the code, not guessed):
//
//	changes := diff.Compare(new, old, diff.DefaultSimhashThreshold, now)   // Stage 1
//	for _, r := range rules.DefaultRuleSet() {                              // Stage 2 (pure)
//	    if f := r.Eval(rules.EvalContext{...}); f.Failed { findings = append(findings, f) }
//	}
//
// The suite asserts the FINDING SET (rule_id -> severity) the engine produces for
// each scenario, plus — where a scenario's value is the change-stream alert rather
// than a rule finding (e.g. a substantive `content` change, a `meta_robots` field
// change that routes critical) — the SUBSTANTIVE change set and its severityForField
// routing. severityForField lives in internal/scheduler (not exported), so the
// suite reimplements the documented bucket map here as a read-only mirror used ONLY
// to assert change-stream routing expectations; the rule findings are read straight
// from the live engine.
package behavior

import (
	"sort"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
)

// FixedNow is the deterministic clock for the whole suite.
var FixedNow = time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)

// firstCrawlBaseline is the canonical first-crawl/baseline sentinel old snapshot
// (Old.ID == 0 AND empty content hash). diff.Compare returns nil for it.
var firstCrawlBaseline = model.Snapshot{ID: 0, ContentSHA256: ""}

// driveFindings runs the documented PURE path: diff.Compare then every rule's
// Eval, returning only the FAILED findings (the engine's open-issue set for this
// crawl, before the ProcessFetch Slack gate / triad collapse / dedup are applied —
// those are the scheduler's concern and are asserted separately via the
// change-stream mirror, since this package may not import the scheduler).
func driveFindings(old, nw model.Snapshot, truncated bool) (changes []model.Change, findings []rules.Finding) {
	changes = diff.Compare(nw, old, diff.DefaultSimhashThreshold, FixedNow)
	ec := rules.EvalContext{
		URLID:      nw.URLID,
		Importance: 0.5,
		New:        nw,
		Old:        old,
		Changes:    changes,
		Truncated:  truncated,
	}
	for _, r := range rules.DefaultRuleSet() {
		if f := r.Eval(ec); f.Failed {
			findings = append(findings, f)
		}
	}
	return changes, findings
}

// findingSet collapses []rules.Finding to a rule_id -> severity map. RuleIDs are
// unique within DefaultRuleSet, so one entry per failing rule.
func findingSet(fs []rules.Finding) map[string]model.Severity {
	out := make(map[string]model.Severity, len(fs))
	for _, f := range fs {
		out[f.RuleID] = f.Severity
	}
	return out
}

// ── change-stream severity mirror (read-only) ───────────────────────────────
//
// The scheduler's severityForField buckets EMITTED change-stream events. It is
// unexported in internal/scheduler, so the documented bucket table is mirrored
// here purely to assert change-stream ROUTING expectations in scenarios whose
// alert is a field change (not a rule). The map is asserted against the contract,
// not the live scheduler — a drift would surface as a scenario mismatch, which is
// exactly what this stress-test is for.
var criticalFields = map[string]bool{
	"indexable": true, "indexability_reason": true, "canonical": true,
	"meta_robots": true, "x_robots_tag": true, "http_status": true,
	"robots_txt": true, "robots_txt_status": true,
	"sitemap_xml_status": true,
}
var warningFields = map[string]bool{
	"title": true, "meta_description": true, "headings": true,
	// hreflang routes WARNING (FIXED, #16): the bare set-change detector has no validity
	// check and the hreflang_invalid RULE emits WARNING, so the change-stream severity must
	// AGREE — routing it critical paged CRITICAL on any hreflang churn while the rule stayed
	// WARNING, and the bridge dedup then swallowed the hreflang_invalid finding.
	"schema_types": true, "hreflang": true, "redirect_chain": true, "internal_link_count": true,
	"content": true, "sitemap_xml": true, "sitemap_coverage_drift": true,
}

// severityForField mirrors the scheduler's bucketing for change-stream events.
func severityForField(field string) model.Severity {
	if criticalFields[field] {
		return model.SeverityCritical
	}
	if warningFields[field] {
		return model.SeverityWarning
	}
	return model.SeverityInfo
}

// substantiveChangeFields returns the set of field names diff.Compare emitted as
// ChangeSubstantive (the paging-eligible change-stream events; cosmetic changes
// are suppressed from Slack and are excluded here).
func substantiveChangeFields(changes []model.Change) []string {
	var out []string
	for _, c := range changes {
		if c.ChangeClass == model.ChangeSubstantive {
			out = append(out, c.Field)
		}
	}
	sort.Strings(out)
	return out
}

// classification labels mirror the scenario matrix taxonomy.
type classification string

const (
	mustFire      classification = "must_fire"
	mustStayQuiet classification = "must_stay_quiet"
	typeNoise     classification = "type_noise_overlay_fixes"
	edge          classification = "edge"
)

// scenario is one row of the behavioral golden suite.
type scenario struct {
	name      string
	siteType  string
	class     classification
	old       model.Snapshot
	nw        model.Snapshot
	truncated bool
	// wantFindings is the EXACT expected failing-rule set (rule_id -> severity)
	// from the pure path. nil/empty means "no rule fires".
	wantFindings map[string]model.Severity
	// wantSubstantive, when non-nil, asserts the set of substantive change-stream
	// fields diff.Compare emits (sorted). Used for scenarios whose alert is a field
	// change. nil means "not asserted" (the finding set is the load-bearing claim).
	wantSubstantive []string
	// skip, when non-empty, marks a SUSPECTED DEFECT: the case is run with t.Skip
	// rather than blessing a wrong-fire/missed-fire as the baseline.
	skip string
}

// ScenarioCount reports how many scenarios the suite encodes (logged by the
// summary test so a dropped scenario is never silent).
func ScenarioCount() int {
	n := 0
	for _, g := range allScenarioGroups() {
		n += len(g())
	}
	return n
}

// allScenarioGroups lists every per-site-type scenario builder. Adding a site
// type means adding its builder here so ScenarioCount stays honest.
func allScenarioGroups() []func() []scenario {
	return []func() []scenario{
		publisherScenarios,
		ecommerceScenarios,
		marketplaceScenarios,
		blogScenarios,
		saasScenarios,
		localScenarios,
	}
}
