package richresult

import (
	"reflect"
	"strconv"
	"testing"
)

// findEntity returns the first report entity whose RawType matches rawType, or
// nil. Tests use RawType (the markup's literal @type) so alias cases stay
// unambiguous.
func findEntity(r Report, rawType string) *EntityResult {
	for i := range r.Entities {
		if r.Entities[i].RawType == rawType {
			return &r.Entities[i]
		}
	}
	return nil
}

// --- Acceptance criterion 1: entity discovery + eligibility table ---

func TestValidate_TopLevelObject_ProductEligible(t *testing.T) {
	jsonld := `{"@context":"https://schema.org","@type":"Product","name":"Widget","offers":{"@type":"Offer","price":"9.99"}}`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 1 {
		t.Fatalf("want 1 entity, got %d (%+v)", len(r.Entities), r.Entities)
	}
	e := r.Entities[0]
	if e.Type != "Product" || e.RawType != "Product" {
		t.Fatalf("Type/RawType = %q/%q, want Product/Product", e.Type, e.RawType)
	}
	if !e.Eligible {
		t.Fatalf("Product with name+offers should be eligible: %+v", e)
	}
	if len(e.Missing) != 0 || len(e.MissingAnyOf) != 0 {
		t.Fatalf("eligible entity should have no Missing/MissingAnyOf: %+v", e)
	}
}

func TestValidate_TopLevelArray(t *testing.T) {
	jsonld := `[{"@type":"Product","name":"A","offers":{"price":"1"}},{"@type":"BreadcrumbList","itemListElement":[{"@type":"ListItem","position":1,"name":"Home"}]}]`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 2 {
		t.Fatalf("want 2 entities from top-level array, got %d (%+v)", len(r.Entities), r.Entities)
	}
	if p := findEntity(r, "Product"); p == nil || !p.Eligible {
		t.Fatalf("Product should be discovered and eligible: %+v", p)
	}
	if b := findEntity(r, "BreadcrumbList"); b == nil || !b.Eligible {
		t.Fatalf("BreadcrumbList should be discovered and eligible: %+v", b)
	}
}

func TestValidate_Graph(t *testing.T) {
	jsonld := `{"@context":"https://schema.org","@graph":[` +
		`{"@type":"WebSite","name":"s"},` +
		`{"@type":"Product","name":"Widget","review":{"@type":"Review"}}` +
		`]}`
	r := Validate(jsonld, GRR202606)
	// WebSite is unprofiled (counts neutral); Product is profiled.
	if p := findEntity(r, "Product"); p == nil || !p.Eligible {
		t.Fatalf("@graph Product should be discovered and eligible (name+review): %+v", p)
	}
	if r.Unprofiled != 1 {
		t.Fatalf("WebSite should count as 1 unprofiled entity, got %d", r.Unprofiled)
	}
}

func TestValidate_TypeAsArray(t *testing.T) {
	// Multi-type entity: ["NewsArticle","Thing"] resolves to the Article family
	// via the NewsArticle alias.
	jsonld := `{"@type":["NewsArticle","Thing"],"headline":"Breaking"}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "NewsArticle")
	if e == nil {
		t.Fatalf("entity with @type array should be discovered: %+v", r.Entities)
	}
	if e.Type != "Article" {
		t.Fatalf("NewsArticle should resolve to canonical Article, got %q", e.Type)
	}
	if !e.Eligible {
		t.Fatalf("NewsArticle with headline should be eligible: %+v", e)
	}
}

func TestValidate_AliasBlogPosting(t *testing.T) {
	jsonld := `{"@type":"BlogPosting","headline":"Hello"}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "BlogPosting")
	if e == nil || e.Type != "Article" {
		t.Fatalf("BlogPosting should resolve to Article: %+v", r.Entities)
	}
	if !e.Eligible {
		t.Fatalf("BlogPosting with headline should be eligible: %+v", e)
	}
}

func TestValidate_ProductMissingOffers_Ineligible(t *testing.T) {
	// offers removed; name present, but none of offers|review|aggregateRating.
	jsonld := `{"@type":"Product","name":"Widget"}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "Product")
	if e == nil {
		t.Fatal("Product not discovered")
	}
	if e.Eligible {
		t.Fatalf("Product without offers/review/aggregateRating should be ineligible: %+v", e)
	}
	if len(e.Missing) != 0 {
		t.Fatalf("name is present so Missing should be empty, got %v", e.Missing)
	}
	wantAnyOf := []string{"offers", "review", "aggregateRating"}
	if len(e.MissingAnyOf) != 1 || !reflect.DeepEqual(e.MissingAnyOf[0], wantAnyOf) {
		t.Fatalf("MissingAnyOf should name the unsatisfied any-of group %v, got %v", wantAnyOf, e.MissingAnyOf)
	}
}

func TestValidate_ProductMissingName_Ineligible(t *testing.T) {
	jsonld := `{"@type":"Product","offers":{"price":"1"}}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "Product")
	if e == nil || e.Eligible {
		t.Fatalf("Product without name should be ineligible: %+v", e)
	}
	if len(e.Missing) != 1 || e.Missing[0] != "name" {
		t.Fatalf("Missing should be [name], got %v", e.Missing)
	}
	// offers satisfied → no MissingAnyOf.
	if len(e.MissingAnyOf) != 0 {
		t.Fatalf("offers present so MissingAnyOf should be empty, got %v", e.MissingAnyOf)
	}
}

func TestValidate_ArticleEmptyHeadline_Missing(t *testing.T) {
	jsonld := `{"@type":"Article","headline":"   "}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "Article")
	if e == nil || e.Eligible {
		t.Fatalf("Article with whitespace-only headline should be ineligible: %+v", e)
	}
	if len(e.Missing) != 1 || e.Missing[0] != "headline" {
		t.Fatalf("whitespace headline should count as missing, got Missing=%v", e.Missing)
	}
}

func TestValidate_BreadcrumbEmptyList_Missing(t *testing.T) {
	jsonld := `{"@type":"BreadcrumbList","itemListElement":[]}`
	r := Validate(jsonld, GRR202606)
	e := findEntity(r, "BreadcrumbList")
	if e == nil || e.Eligible {
		t.Fatalf("BreadcrumbList with empty itemListElement should be ineligible: %+v", e)
	}
	if len(e.Missing) != 1 || e.Missing[0] != "itemListElement" {
		t.Fatalf("empty array should count as missing, got %v", e.Missing)
	}
}

func TestValidate_UnprofiledType_NoVerdict(t *testing.T) {
	jsonld := `{"@type":"FAQPage","mainEntity":[]}`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 0 {
		t.Fatalf("unprofiled FAQPage must yield no eligibility entity, got %+v", r.Entities)
	}
	if r.Unprofiled != 1 {
		t.Fatalf("FAQPage should count as 1 unprofiled, got %d", r.Unprofiled)
	}
}

func TestValidate_EmptyAndNullInputs(t *testing.T) {
	for _, in := range []string{"", "   ", "null", "[]", "{}", `"null"`} {
		r := Validate(in, GRR202606)
		if len(r.Entities) != 0 {
			t.Fatalf("input %q should yield zero entities, got %+v", in, r.Entities)
		}
		if r.Unprofiled != 0 {
			t.Fatalf("input %q should yield zero unprofiled, got %d", in, r.Unprofiled)
		}
	}
}

func TestValidate_MalformedJSON_NoEntitiesNoPanic(t *testing.T) {
	for _, in := range []string{`{`, `[{`, `not json`, `{"@type":}`} {
		r := Validate(in, GRR202606) // must not panic
		if len(r.Entities) != 0 {
			t.Fatalf("malformed input %q should yield zero entities, got %+v", in, r.Entities)
		}
	}
}

func TestValidate_PresenceRule(t *testing.T) {
	// Each value form that must read as "absent" for the required name property.
	for _, name := range []string{`null`, `""`, `"   "`, `[]`, `{}`} {
		jsonld := `{"@type":"Product","name":` + name + `,"offers":{"price":"1"}}`
		r := Validate(jsonld, GRR202606)
		e := findEntity(r, "Product")
		if e == nil {
			t.Fatalf("name=%s: Product not discovered", name)
		}
		if e.Eligible {
			t.Fatalf("name=%s should read as absent → ineligible: %+v", name, e)
		}
		if len(e.Missing) != 1 || e.Missing[0] != "name" {
			t.Fatalf("name=%s should report Missing=[name], got %v", name, e.Missing)
		}
	}
	// And forms that must read as "present".
	for _, name := range []string{`"Widget"`, `0`, `false`, `["x"]`, `{"k":"v"}`} {
		jsonld := `{"@type":"Product","name":` + name + `,"offers":{"price":"1"}}`
		r := Validate(jsonld, GRR202606)
		e := findEntity(r, "Product")
		if e == nil || !e.Eligible {
			t.Fatalf("name=%s should read as present → eligible: %+v", name, e)
		}
	}
}

func TestValidate_PartialEligibility_PerEntity(t *testing.T) {
	// Two Products: one eligible, one not. Each entity judged independently.
	jsonld := `[{"@type":"Product","name":"A","offers":{"price":"1"}},{"@type":"Product","name":"B"}]`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 2 {
		t.Fatalf("want 2 Product entities, got %d", len(r.Entities))
	}
	var eligible, ineligible int
	for _, e := range r.Entities {
		if e.Eligible {
			eligible++
		} else {
			ineligible++
		}
	}
	if eligible != 1 || ineligible != 1 {
		t.Fatalf("want 1 eligible + 1 ineligible, got %d/%d", eligible, ineligible)
	}
}

// TestValidate_MultiProfiledType_FirstMatchWins pins the single-family-per-entity
// scope line: an entity whose @type array names MORE THAN ONE profiled family is
// validated only against the FIRST matching member; the remaining profiled facets
// are neither validated nor counted as Unprofiled. This locks the behavior so a
// future refactor of validateEntity (e.g. switching to validate-all-families)
// trips this test rather than silently changing the surfaces/severity contract.
// Recorded in ADR 0001 ("Single profiled family per entity").
func TestValidate_MultiProfiledType_FirstMatchWins(t *testing.T) {
	// Product first: eligible (name+offers). The Article facet — which WOULD be
	// ineligible (no headline) — must be invisible: not validated, not Unprofiled.
	jsonld := `{"@type":["Product","Article"],"name":"Widget","offers":{"price":"1"}}`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 1 {
		t.Fatalf("multi-profiled entity should yield exactly one EntityResult, got %d (%+v)", len(r.Entities), r.Entities)
	}
	if r.Entities[0].Type != "Product" {
		t.Fatalf("first profiled member (Product) should win, got Type=%q", r.Entities[0].Type)
	}
	if !r.Entities[0].Eligible {
		t.Fatalf("Product facet (name+offers) should be eligible: %+v", r.Entities[0])
	}
	if r.Unprofiled != 0 {
		t.Fatalf("the unvalidated Article facet must NOT count as Unprofiled, got %d", r.Unprofiled)
	}

	// Reversed order: Article first → that family is the one validated.
	rev := Validate(`{"@type":["Article","Product"],"name":"Widget","offers":{"price":"1"}}`, GRR202606)
	if len(rev.Entities) != 1 {
		t.Fatalf("reversed multi-profiled entity should yield exactly one EntityResult, got %d (%+v)", len(rev.Entities), rev.Entities)
	}
	if rev.Entities[0].Type != "Article" {
		t.Fatalf("first profiled member (Article) should win on reversed order, got Type=%q", rev.Entities[0].Type)
	}
	// Article requires headline (absent here) → ineligible, proving the family
	// actually evaluated flipped with @type order.
	if rev.Entities[0].Eligible {
		t.Fatalf("Article facet without headline should be ineligible: %+v", rev.Entities[0])
	}
	if rev.Unprofiled != 0 {
		t.Fatalf("the unvalidated Product facet must NOT count as Unprofiled, got %d", rev.Unprofiled)
	}
}

// TestArticleHeadline_RabbotPolicy_BehaviorAndVersion pins the locked decision
// for finding #11 ("relabel as a Rabbot policy"): Google lists Article.headline
// as recommended, not required, so the doc copy must NOT assert it as a Google
// requirement — but the RULE BEHAVIOR is kept (Rabbot still flags an Article that
// ships no headline). Because the relabel is wording-only, it must NOT bump the
// profile version. This test fails if a future "fix" either (a) drops the
// headline rule (Article-without-headline would wrongly become eligible) or
// (b) silently bumps the version while only the copy changed. The required
// property stays "headline" so an operator who strips it still gets paged.
func TestArticleHeadline_RabbotPolicy_BehaviorAndVersion(t *testing.T) {
	// (a) Behavior preserved: an Article with no headline is still ineligible and
	// reports the missing property by its raw name (the structured signal the
	// surfaces relabel as Rabbot policy).
	r := Validate(`{"@type":"Article","name":"no headline here"}`, GRR202606)
	e := findEntity(r, "Article")
	if e == nil {
		t.Fatalf("Article entity should be discovered: %+v", r.Entities)
	}
	if e.Eligible {
		t.Fatalf("Article without a headline must stay ineligible (Rabbot policy keeps the rule): %+v", e)
	}
	if len(e.Missing) != 1 || e.Missing[0] != "headline" {
		t.Fatalf("Article without a headline should report Missing=[headline], got %v", e.Missing)
	}

	// (b) Wording-only relabel ⇒ no version bump. The table and version are still
	// the v1 profile; the change lives in the doc copy, not the rule table.
	if GRR202606.Version != "grr-2026.06" {
		t.Fatalf("relabel must be wording-only: version drifted to %q (a behavior/table change would need a NEW constant, a copy change must not bump)", GRR202606.Version)
	}
	if got := GRR202606.Types["Article"]; len(got.Required) != 1 || got.Required[0] != "headline" {
		t.Fatalf("Article.Required should remain [headline] (rule kept), got %v", got.Required)
	}
}

// countEligible returns, for the given canonical type, the eligible and
// ineligible entity counts in r — mirroring exactly what the rules layer's
// richResultRule.classify reads off Validate to decide a lost-eligibility flip.
func countEligible(r Report, canonicalType string) (eligible, ineligible int) {
	for _, e := range r.Entities {
		if e.Type != canonicalType {
			continue
		}
		if e.Eligible {
			eligible++
		} else {
			ineligible++
		}
	}
	return eligible, ineligible
}

// --- MISS fix 1: nested @graph recursion ---

// TestValidate_NestedGraph_Recurses pins the fix for a @graph member that is
// itself a @graph container: the inner profiled entities must still be
// discovered. Before the fix, expandObject flattened only one level, so a
// nested @graph hid all its entities (entities=0) and eligibility never ran.
func TestValidate_NestedGraph_Recurses(t *testing.T) {
	// Outer array → object with @graph → member that is itself a @graph container
	// → inner Product (name+offers, eligible).
	jsonld := `[{"@graph":[{"@graph":[{"@type":"Product","name":"Widget","offers":{"price":"1"}}]}]}]`
	r := Validate(jsonld, GRR202606)
	p := findEntity(r, "Product")
	if p == nil {
		t.Fatalf("nested @graph Product must be discovered, got entities=%+v", r.Entities)
	}
	if !p.Eligible {
		t.Fatalf("nested @graph Product (name+offers) should be eligible: %+v", p)
	}
}

// TestValidate_NestedGraph_LostEligibilityFlip proves the nested-@graph fix
// restores the lost-eligibility signal the rules layer keys CRITICAL off: the
// OLD markup (eligible Product nested in a @graph-of-@graph) reports eligible>=1,
// and the NEW markup (same nesting, offers dropped) reports eligible==0 with the
// any-of group named. Before the fix BOTH reported eligible==0, so the flip was
// invisible and the CRITICAL alert never fired.
func TestValidate_NestedGraph_LostEligibilityFlip(t *testing.T) {
	old := `[{"@graph":[{"@graph":[{"@type":"Product","name":"Widget","offers":{"price":"1"}}]}]}]`
	neu := `[{"@graph":[{"@graph":[{"@type":"Product","name":"Widget"}]}]}]`

	oldElig, _ := countEligible(Validate(old, GRR202606), "Product")
	if oldElig < 1 {
		t.Fatalf("OLD nested-@graph Product should have >=1 eligible (flip baseline), got %d", oldElig)
	}
	newElig, newIneligible := countEligible(Validate(neu, GRR202606), "Product")
	if newElig != 0 {
		t.Fatalf("NEW nested-@graph Product (offers dropped) should have 0 eligible, got %d", newElig)
	}
	if newIneligible < 1 {
		t.Fatalf("NEW nested-@graph Product must be discovered-but-ineligible to fire the flip, got ineligible=%d", newIneligible)
	}
	// The any-of group must be named so the operator sees what to restore.
	e := findEntity(Validate(neu, GRR202606), "Product")
	wantAnyOf := []string{"offers", "review", "aggregateRating"}
	if e == nil || len(e.MissingAnyOf) != 1 || !reflect.DeepEqual(e.MissingAnyOf[0], wantAnyOf) {
		t.Fatalf("dropped-offers nested Product should report MissingAnyOf=[%v], got %+v", wantAnyOf, e)
	}
}

// --- MISS fix 2: single-object @graph ---

// TestValidate_SingleObjectGraph_CountsEntity pins the fix for a @graph whose
// value is a single object (not an array): the one node must be discovered.
// Before the fix the code type-asserted obj["@graph"].([]any) and silently
// dropped a map-valued @graph (entities=0).
func TestValidate_SingleObjectGraph_CountsEntity(t *testing.T) {
	jsonld := `{"@context":"https://schema.org","@graph":{"@type":"Product","name":"Widget","offers":{"price":"1"}}}`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 1 {
		t.Fatalf("single-object @graph should yield exactly 1 entity, got %d (%+v)", len(r.Entities), r.Entities)
	}
	p := findEntity(r, "Product")
	if p == nil || !p.Eligible {
		t.Fatalf("single-object @graph Product (name+offers) should be discovered and eligible: %+v", p)
	}
}

// TestValidate_SingleObjectGraph_LostEligibilityFlip proves the single-object
// @graph fix restores the lost-eligibility signal: OLD (eligible) → eligible>=1,
// NEW (offers dropped) → eligible==0 + discovered ineligible. Before the fix
// neither side discovered the entity, so the CRITICAL flip never fired.
func TestValidate_SingleObjectGraph_LostEligibilityFlip(t *testing.T) {
	old := `{"@graph":{"@type":"Product","name":"Widget","offers":{"price":"1"}}}`
	neu := `{"@graph":{"@type":"Product","name":"Widget"}}`

	oldElig, _ := countEligible(Validate(old, GRR202606), "Product")
	if oldElig < 1 {
		t.Fatalf("OLD single-object-@graph Product should have >=1 eligible (flip baseline), got %d", oldElig)
	}
	newElig, newIneligible := countEligible(Validate(neu, GRR202606), "Product")
	if newElig != 0 {
		t.Fatalf("NEW single-object-@graph Product (offers dropped) should have 0 eligible, got %d", newElig)
	}
	if newIneligible < 1 {
		t.Fatalf("NEW single-object-@graph Product must be discovered-but-ineligible to fire the flip, got ineligible=%d", newIneligible)
	}
}

// --- MISS fix 3: full-IRI / CURIE @type resolution ---

// TestValidate_FullIRIType_ResolvesToProduct pins the fix for a fully-qualified
// schema.org IRI @type. Before the fix resolve did exact-string matching, so
// "https://schema.org/Product" fell to Unprofiled and eligibility never ran.
func TestValidate_FullIRIType_ResolvesToProduct(t *testing.T) {
	for _, typ := range []string{
		"https://schema.org/Product",
		"http://schema.org/Product",
		"https://schema.org/Product/", // trailing slash tolerated
	} {
		jsonld := `{"@type":` + strconv.Quote(typ) + `,"name":"Widget","offers":{"price":"1"}}`
		r := Validate(jsonld, GRR202606)
		if len(r.Entities) != 1 {
			t.Fatalf("@type %q should resolve to a profiled entity, got %d entities (unprofiled=%d)", typ, len(r.Entities), r.Unprofiled)
		}
		e := r.Entities[0]
		if e.Type != "Product" {
			t.Fatalf("@type %q should resolve to canonical Product, got %q", typ, e.Type)
		}
		if !e.Eligible {
			t.Fatalf("@type %q with name+offers should be eligible: %+v", typ, e)
		}
		if r.Unprofiled != 0 {
			t.Fatalf("@type %q must not count as Unprofiled, got %d", typ, r.Unprofiled)
		}
	}
}

// TestValidate_CURIEType_ResolvesToProduct pins the fix for the schema: CURIE
// prefix form ("schema:Product"). Before the fix it fell to Unprofiled.
func TestValidate_CURIEType_ResolvesToProduct(t *testing.T) {
	jsonld := `{"@type":"schema:Product","name":"Widget","offers":{"price":"1"}}`
	r := Validate(jsonld, GRR202606)
	if len(r.Entities) != 1 {
		t.Fatalf("CURIE @type should resolve to a profiled entity, got %d (unprofiled=%d)", len(r.Entities), r.Unprofiled)
	}
	e := r.Entities[0]
	if e.Type != "Product" {
		t.Fatalf("schema:Product should resolve to canonical Product, got %q", e.Type)
	}
	if !e.Eligible {
		t.Fatalf("schema:Product with name+offers should be eligible: %+v", e)
	}
}

// TestValidate_ConsistentFormsWithExtract: the no-scheme IRI ("schema.org/Product")
// and the "schemaorg:" CURIE must resolve too, so normalizeType stays consistent with
// extract.bareSchemaType's accepted form set (the two are documented as kept consistent).
func TestValidate_ConsistentFormsWithExtract(t *testing.T) {
	for _, typ := range []string{"schema.org/Product", "schemaorg:Product"} {
		jsonld := `{"@type":` + strconv.Quote(typ) + `,"name":"Widget","offers":{"price":"1"}}`
		r := Validate(jsonld, GRR202606)
		if len(r.Entities) != 1 || r.Entities[0].Type != "Product" {
			t.Fatalf("@type %q should resolve to Product, got %+v (unprofiled=%d)", typ, r.Entities, r.Unprofiled)
		}
	}
}

// TestValidate_IRIType_AliasResolves checks the IRI/CURIE normalization also
// flows through aliases (schema.org/BlogPosting → canonical Article).
func TestValidate_IRIType_AliasResolves(t *testing.T) {
	r := Validate(`{"@type":"https://schema.org/BlogPosting","headline":"Hello"}`, GRR202606)
	if len(r.Entities) != 1 || r.Entities[0].Type != "Article" {
		t.Fatalf("IRI alias BlogPosting should resolve to Article: %+v (unprofiled=%d)", r.Entities, r.Unprofiled)
	}
	if !r.Entities[0].Eligible {
		t.Fatalf("IRI BlogPosting with headline should be eligible: %+v", r.Entities[0])
	}
}

// TestValidate_IRIType_LostEligibilityFlip proves the IRI/CURIE fix restores the
// lost-eligibility signal on schema.org-IRI markup: OLD (offers present) →
// eligible>=1, NEW (offers dropped) → eligible==0 + discovered ineligible.
// Before the fix both sides were Unprofiled, so the CRITICAL flip never fired.
func TestValidate_IRIType_LostEligibilityFlip(t *testing.T) {
	old := `{"@type":"https://schema.org/Product","name":"Widget","offers":{"price":"1"}}`
	neu := `{"@type":"https://schema.org/Product","name":"Widget"}`

	oldElig, _ := countEligible(Validate(old, GRR202606), "Product")
	if oldElig < 1 {
		t.Fatalf("OLD IRI Product should have >=1 eligible (flip baseline), got %d", oldElig)
	}
	newElig, newIneligible := countEligible(Validate(neu, GRR202606), "Product")
	if newElig != 0 {
		t.Fatalf("NEW IRI Product (offers dropped) should have 0 eligible, got %d", newElig)
	}
	if newIneligible < 1 {
		t.Fatalf("NEW IRI Product must be discovered-but-ineligible to fire the flip, got ineligible=%d", newIneligible)
	}
}

// TestValidate_NonSchemaOrgIRI_StaysUnprofiled is the conservatism guard: the
// normalization must only collapse RECOGNIZED schema.org IRIs/CURIEs. A
// lookalike host or an unrelated CURIE prefix must NOT be coerced to a profile
// key — it stays Unprofiled, exactly as a bare unprofiled @type would.
func TestValidate_NonSchemaOrgIRI_StaysUnprofiled(t *testing.T) {
	for _, typ := range []string{
		"https://example.com/Product",         // wrong host
		"https://schema.org.evil.com/Product", // lookalike host (must not be treated as schema.org)
		"ex:Product",                          // unrelated CURIE prefix
		"https://schema.org/FAQPage",          // schema.org IRI but unprofiled term
	} {
		jsonld := `{"@type":` + strconv.Quote(typ) + `,"name":"Widget","offers":{"price":"1"}}`
		r := Validate(jsonld, GRR202606)
		if len(r.Entities) != 0 {
			t.Fatalf("@type %q must NOT resolve to a profile key, got entities=%+v", typ, r.Entities)
		}
		if r.Unprofiled != 1 {
			t.Fatalf("@type %q should count as 1 unprofiled, got %d", typ, r.Unprofiled)
		}
	}
}

// --- Acceptance criterion 2: profile golden test ---

// TestProfileGolden pins the GRR202606 version string and the exact
// type/property table. ANY edit to a requirement (or to the version) breaks this
// test; per the spec, a requirement change must ship as a NEW version constant.
func TestProfileGolden(t *testing.T) {
	if GRR202606.Version != "grr-2026.06" {
		t.Fatalf("profile version drifted: %q (a requirement/version change needs a NEW version constant)", GRR202606.Version)
	}

	want := map[string]TypeProfile{
		"Product": {
			Aliases:  nil,
			Required: []string{"name"},
			AnyOf:    [][]string{{"offers", "review", "aggregateRating"}},
		},
		"Article": {
			Aliases:  []string{"NewsArticle", "BlogPosting"},
			Required: []string{"headline"},
			AnyOf:    nil,
		},
		"BreadcrumbList": {
			Aliases:  nil,
			Required: []string{"itemListElement"},
			AnyOf:    nil,
		},
	}

	if !reflect.DeepEqual(GRR202606.Types, want) {
		t.Fatalf("GRR202606 type/property table drifted.\n got: %#v\nwant: %#v\n"+
			"A requirement change must ship as a NEW version constant, not an edit.",
			GRR202606.Types, want)
	}
}

// TestValidate_StoredArrayFormBlock covers the PR #75 review finding: the
// stored snapshots.jsonld column is json.Marshal([]json.RawMessage{...}) — an
// outer array of raw <script> blocks. A block whose own content is a top-level
// array (legal JSON-LD) is therefore stored NESTED: [[{...},{...}]]. Entities
// inside such a block must still be discovered and validated.
func TestValidate_StoredArrayFormBlock(t *testing.T) {
	// One array-form block containing an ineligible Product (offers missing)
	// and an eligible Article — exactly as SaveSnapshot would store it.
	stored := `[[{"@type":"Product","name":"Widget"},{"@type":"Article","headline":"h"}]]`
	rep := Validate(stored, GRR202606)
	if len(rep.Entities) != 2 {
		t.Fatalf("stored array-form block: want 2 discovered entities, got %d (array-form blocks invisible to the validator)", len(rep.Entities))
	}
	var product *EntityResult
	for i := range rep.Entities {
		if rep.Entities[i].Type == "Product" {
			product = &rep.Entities[i]
		}
	}
	if product == nil {
		t.Fatalf("Product entity not discovered in array-form block")
	}
	if product.Eligible {
		t.Fatalf("Product without offers/review/aggregateRating must be ineligible")
	}
}
