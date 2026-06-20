package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestRichResultRulesUnmappedInBridge pins that the four A4 rule_ids are
// DELIBERATELY unmapped in ruleFieldForBridge: each bridges under its OWN rule_id
// via the BridgeFieldForRule fallback. Mapping rich_result_product to schema_types
// would let a same-crawl schema_types warning dedup the marquee critical away — the
// exact bug the unmapped fallback exists to avoid (A4 alert-path design).
func TestRichResultRulesUnmappedInBridge(t *testing.T) {
	for _, ruleID := range []string{
		"rich_result_product",
		"rich_result_article",
		"rich_result_breadcrumb",
		"structured_data_invalid_json",
	} {
		field, ok := BridgeFieldForRule(ruleID)
		if ok {
			t.Errorf("BridgeFieldForRule(%q): ok=true (mapped to %q), want UNMAPPED so it bridges under its own id", ruleID, field)
		}
	}
}

// TestRichResultCriticalNotDedupedBehindSchemaWarning is the alert-path assertion of
// A4: the marquee Product-eligibility critical must reach Slack under change_type
// "rich_result_product" EVEN WHEN a schema_types warning was ingested the same crawl.
// Because rich_result_product is unmapped (bridges under its own id, NOT schema_types),
// the ingestedTypes dedup does not swallow it. This guards against a regression that
// maps the rule to schema_types and silences the critical behind the warning.
func TestRichResultCriticalNotDedupedBehindSchemaWarning(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// The rules engine newly opens the Product eligibility critical, bridged under its
	// own change_type "rich_result_product" (unmapped fallback in the real adapter).
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{
			Field:    "rich_result_product",
			Severity: model.SeverityCritical,
			Detail:   `{"profile":"grr-2026.06","type":"Product","entities":1,"ineligible":1,"missing":["offers"]}`,
		}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A schema_types change ALSO fires this crawl (a sibling @type churned). It ingests
	// a warning event under change_type "schema_types". The critical must NOT be folded
	// into / deduped behind it.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01, SchemaTypes: "Product"}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01, SchemaTypes: "Product,Breadcrumb"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	var sawSchema, sawRich bool
	var richSev model.Severity
	for _, e := range deps.ingested {
		switch e.ChangeType {
		case "schema_types":
			sawSchema = true
		case "rich_result_product":
			sawRich = true
			richSev = e.Severity
		}
	}
	if !sawSchema {
		t.Errorf("expected the schema_types warning event; got %+v", deps.ingested)
	}
	if !sawRich {
		t.Fatalf("the marquee rich_result_product CRITICAL must reach Slack under its own change_type, NOT be deduped behind schema_types; got %+v", deps.ingested)
	}
	if richSev != model.SeverityCritical {
		t.Errorf("bridged rich_result_product severity = %q, want critical", richSev)
	}
}

// TestProcessFetchPassesTruncatedToApplyRules guards that the truncated flag is
// THREADED into the rules engine: ProcessFetch must forward its truncated argument
// to ProcessorDeps.ApplyRules so the A4 rich-result rules can self-suppress on a
// severed JSON-LD <script>. Without the thread, a truncated body would be evaluated
// as if its (possibly mangled) JSON-LD were authoritative.
func TestProcessFetchPassesTruncatedToApplyRules(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "b", ContentSimhash: 0x0F}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", true); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	if len(deps.appliedTruncated) != 1 {
		t.Fatalf("ApplyRules must be called once; got truncated-args %v", deps.appliedTruncated)
	}
	if !deps.appliedTruncated[0] {
		t.Errorf("ProcessFetch must forward truncated=true into ApplyRules; got %v", deps.appliedTruncated[0])
	}
}
