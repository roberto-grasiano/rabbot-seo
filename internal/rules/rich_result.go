package rules

import (
	"encoding/json"
	"strconv"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/richresult"
)

// richResultRule validates one rich-result type family against the versioned
// GRR202606 profile, opening an eligibility issue when the page's markup for that
// type is present but ineligible. It is the A4 marquee check: a deploy that drops
// `offers` from Product markup (the @type set unchanged, so no schema_types diff
// fires) loses rich-result eligibility, and this rule pages the operator within one
// recheck interval — hours before Search Console notices.
//
// Semantics (spec, exact):
//   - Type absent from New.JSONLD => PASS. Absence is not a defect: a legitimately
//     retired Product page must not hold an issue open forever, and full-block
//     removal already fires the generic schema_types change alert.
//   - Any entity of the type ineligible => FAIL.
//   - Severity CRITICAL only on a LOST-eligibility flip: Old.ID != 0 (a real prior
//     baseline existed, the established transition guard, cf. indexabilityFlipRule)
//     AND Old had ≥1 eligible entity of the type AND New has none eligible. Every
//     other failing case — steady-state invalid, first-crawl invalid (Old.ID == 0),
//     Old-never-eligible, or partial (some entities still eligible) — is WARNING.
//   - Truncated body => no finding (the New JSON-LD may be a severed <script>;
//     don't guess, per the h1_issue precedent).
type richResultRule struct {
	typeName string // canonical profile key, e.g. "Product"
	ruleID   string // e.g. "rich_result_product"
}

func (r richResultRule) ID() string { return r.ruleID }

func (r richResultRule) Eval(ctx EvalContext) Finding {
	pass := Finding{RuleID: r.ruleID}

	// A truncated body can sever a JSON-LD <script> mid-block, reading as malformed
	// JSON or a vanished type. Emit nothing rather than guess.
	if ctx.Truncated {
		return pass
	}

	newEntities, newEligible, newIneligible := r.classify(ctx.New.JSONLD)
	// Type absent from the new markup: absence is not a defect.
	if newEntities == 0 {
		return pass
	}
	// All present entities are eligible: pass.
	if newIneligible == 0 {
		return pass
	}

	// At least one entity of the type is ineligible — this is a failing finding.
	// Decide the severity: CRITICAL only when eligibility was *lost* this baseline.
	severity := model.SeverityWarning
	if ctx.Old.ID != 0 {
		_, oldEligible, _ := r.classify(ctx.Old.JSONLD)
		// Lost eligibility: Old had ≥1 eligible entity and New now has none eligible.
		if oldEligible >= 1 && newEligible == 0 {
			severity = model.SeverityCritical
		}
	}

	return Finding{
		RuleID:   r.ruleID,
		Failed:   true,
		Severity: severity,
		Detail:   r.detail(newEntities, newIneligible, ctx.New.JSONLD),
	}
}

// classify validates the JSON-LD column and returns, for THIS rule's type family,
// the counts of (total entities, eligible entities, ineligible entities). Aliases
// resolve to the canonical type (e.g. BlogPosting -> Article), so an Article rule
// counts BlogPosting entities too.
func (r richResultRule) classify(jsonld string) (total, eligible, ineligible int) {
	rep := richresult.Validate(jsonld, richresult.GRR202606)
	for _, e := range rep.Entities {
		if e.Type != r.typeName {
			continue
		}
		total++
		if e.Eligible {
			eligible++
		} else {
			ineligible++
		}
	}
	return total, eligible, ineligible
}

// detail builds the Detail JSON the spec pins:
// {"profile":...,"type":...,"entities":N,"ineligible":N,"missing":[...]}. The
// missing list is the union of Required-missing + AnyOf-group members from the
// FIRST ineligible entity of this type — enough for the operator to see what to
// restore (e.g. ["offers","review","aggregateRating"]).
func (r richResultRule) detail(entities, ineligible int, jsonld string) string {
	missing := r.firstIneligibleMissing(jsonld)
	type detailJSON struct {
		Profile    string   `json:"profile"`
		Type       string   `json:"type"`
		Entities   int      `json:"entities"`
		Ineligible int      `json:"ineligible"`
		Missing    []string `json:"missing"`
	}
	b, err := json.Marshal(detailJSON{
		Profile:    richresult.GRR202606.Version,
		Type:       r.typeName,
		Entities:   entities,
		Ineligible: ineligible,
		Missing:    missing,
	})
	if err != nil {
		// json.Marshal of this fixed shape cannot fail; degrade to a minimal literal.
		return `{"profile":` + strconv.Quote(richresult.GRR202606.Version) +
			`,"type":` + strconv.Quote(r.typeName) + `}`
	}
	return string(b)
}

// firstIneligibleMissing returns the missing-property names for the first
// ineligible entity of this type: each missing Required property, then each AnyOf
// group flattened (a group is "missing" iff none of its members is present, so all
// its candidate members are named for the operator).
func (r richResultRule) firstIneligibleMissing(jsonld string) []string {
	rep := richresult.Validate(jsonld, richresult.GRR202606)
	for _, e := range rep.Entities {
		if e.Type != r.typeName || e.Eligible {
			continue
		}
		out := make([]string, 0, len(e.Missing))
		out = append(out, e.Missing...)
		for _, group := range e.MissingAnyOf {
			out = append(out, group...)
		}
		return out
	}
	return nil
}

// structuredDataInvalidJSONRule fails (warning) while the latest snapshot carries
// one or more JSON-LD blocks that failed to parse during extraction
// (Snapshot.JSONLDInvalidCount > 0). It self-suppresses on a truncated body: a cut
// <script> is the expected cause of a malformed block, not a real markup defect.
type structuredDataInvalidJSONRule struct{}

func (structuredDataInvalidJSONRule) ID() string { return "structured_data_invalid_json" }

func (structuredDataInvalidJSONRule) Eval(ctx EvalContext) Finding {
	pass := Finding{RuleID: "structured_data_invalid_json"}
	if ctx.Truncated {
		return pass
	}
	if ctx.New.JSONLDInvalidCount > 0 {
		return Finding{
			RuleID:   "structured_data_invalid_json",
			Failed:   true,
			Severity: model.SeverityWarning,
			Detail:   `{"invalid_blocks":` + strconv.Itoa(ctx.New.JSONLDInvalidCount) + `}`,
		}
	}
	return pass
}
