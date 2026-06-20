package extract

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
)

// decodeHreflang decodes snap.Hreflang (a JSON array of lang strings).
func decodeHreflang(t *testing.T, h string) []string {
	t.Helper()
	if h == "" || h == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(h), &out); err != nil {
		t.Fatalf("snap.Hreflang not a JSON array: %v (%q)", err, h)
	}
	return out
}

// decodeHeadings decodes snap.Headings (a JSON object tag->[]text).
func decodeHeadings(t *testing.T, h string) map[string][]string {
	t.Helper()
	if h == "" || h == "null" {
		return nil
	}
	var out map[string][]string
	if err := json.Unmarshal([]byte(h), &out); err != nil {
		t.Fatalf("snap.Headings not a JSON object: %v (%q)", err, h)
	}
	return out
}

// ── Fix 1: inert <template>/<noscript> subtrees must not contribute signals ──

// TestInertTemplateNoFalseDeindex pins the critical bug: a clean indexable page
// and the SAME page with a noindex meta-robots tag wrapped in an inert
// <template> must both extract as Indexable=true. goquery parses <template>
// children into the regular node tree (a browser does NOT), so before the fix
// collectRobotsMeta descended into the template and fabricated a false noindex.
func TestInertTemplateNoFalseDeindex(t *testing.T) {
	const clean = `<!doctype html><html><head><title>t</title></head>` +
		`<body><h1>Real</h1><p>hi there words words words</p></body></html>`
	const withTemplate = `<!doctype html><html><head><title>t</title></head>` +
		`<body><template><meta name="robots" content="noindex"></template>` +
		`<h1>Real</h1><p>hi there words words words</p></body></html>`

	cleanSnap := extractSnapshot(t, clean)
	tmplSnap := extractSnapshot(t, withTemplate)

	if !cleanSnap.Indexable {
		t.Fatalf("clean page Indexable = false, want true")
	}
	if !tmplSnap.Indexable {
		t.Errorf("page with noindex inside <template> Indexable = false, want true (template is inert)")
	}
	if tmplSnap.MetaRobots != "" {
		t.Errorf("MetaRobots = %q, want empty (the noindex lives in an inert <template>)", tmplSnap.MetaRobots)
	}

	// The two snapshots must produce neither an indexability flip nor a
	// meta_robots change when diffed (clean is the baseline old snapshot).
	cleanSnap.ID = 1
	tmplSnap.ID = 2
	for _, ch := range diff.Compare(tmplSnap, cleanSnap, 3, time.Now()) {
		if ch.Field == "indexable" {
			t.Errorf("indexability_flip fired on a template-only difference: %+v", ch)
		}
		if ch.Field == "meta_robots" {
			t.Errorf("meta_robots change fired on a template-only difference: %+v", ch)
		}
	}
}

// TestInertNoscriptNoFalseDeindex: a meta-robots noindex inside <noscript> is
// likewise inert (goquery parses its children as real nodes; the precheck and
// maintext precedents already strip noscript) and must not deindex the page.
func TestInertNoscriptNoFalseDeindex(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<noscript><meta name="robots" content="noindex"></noscript></head>` +
		`<body><h1>Real</h1><p>hi there words words words</p></body></html>`
	snap := extractSnapshot(t, html)
	if !snap.Indexable {
		t.Errorf("page with noindex inside <noscript> Indexable = false, want true")
	}
	if snap.MetaRobots != "" {
		t.Errorf("MetaRobots = %q, want empty (noindex lives in inert <noscript>)", snap.MetaRobots)
	}
}

// TestInertTemplateCanonicalIgnored: a head <template> wrapping a canonical link
// must not fabricate a canonical (which could fire a false canonicalized_away).
func TestInertTemplateCanonicalIgnored(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<template><link rel="canonical" href="https://other.example.com/elsewhere"></template>` +
		`</head><body><p>hi there words words words</p></body></html>`
	snap := extractSnapshot(t, html)
	if snap.Canonical != "" {
		t.Errorf("Canonical = %q, want empty (canonical lives in an inert <template>)", snap.Canonical)
	}
}

// TestInertTemplateContributesNothing: an inert <template> carrying an H1, a
// link, an image (missing alt), an hreflang alternate, and a JSON-LD block must
// contribute ZERO to the snapshot — no heading, no link/image count, no
// hreflang, no schema type.
func TestInertTemplateContributesNothing(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<template>` +
		`<link rel="alternate" hreflang="de" href="https://example.com/de">` +
		`</template></head><body>` +
		`<template>` +
		`<h1>Fake Heading In Template</h1>` +
		`<a href="https://example.com/fake-internal">fake</a>` +
		`<a href="https://external.example.org/x">ext</a>` +
		`<img src="/fake.png">` +
		`<script type="application/ld+json">{"@type":"Product","name":"fake"}</script>` +
		`</template>` +
		`<p>real visible words words words words</p>` +
		`</body></html>`
	snap := extractSnapshot(t, html)

	if h := decodeHeadings(t, snap.Headings); len(h["h1"]) != 0 {
		t.Errorf("h1 from template leaked: %v, want none", h["h1"])
	}
	if snap.InternalLinkCount != 0 {
		t.Errorf("InternalLinkCount = %d, want 0 (link is in an inert template)", snap.InternalLinkCount)
	}
	if snap.ExternalLinkCount != 0 {
		t.Errorf("ExternalLinkCount = %d, want 0 (link is in an inert template)", snap.ExternalLinkCount)
	}
	if snap.ImageCount != 0 {
		t.Errorf("ImageCount = %d, want 0 (img is in an inert template)", snap.ImageCount)
	}
	if hl := decodeHreflang(t, snap.Hreflang); len(hl) != 0 {
		t.Errorf("Hreflang from template leaked: %v, want none", hl)
	}
	var types []string
	if snap.SchemaTypes != "" {
		_ = json.Unmarshal([]byte(snap.SchemaTypes), &types)
	}
	if len(types) != 0 {
		t.Errorf("SchemaTypes from template leaked: %v, want none", types)
	}
}

// TestTemplateDoesNotBreakHydration: stripping <template>/<noscript> must NOT
// strip <script>, so the __NEXT_DATA__ hydration recovery path still works. A
// thin client-shell DOM with a real payload (NOT inside a template) must still
// recover its title.
func TestTemplateDoesNotBreakHydration(t *testing.T) {
	payload := `{"props":{"pageProps":{"seoTitle":"Recovered From Payload"}}}`
	html := `<!doctype html><html><head>` +
		`<template><meta name="robots" content="noindex"></template>` +
		`</head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` + payload + `</script>` +
		`</body></html>`
	snap := extractWith(t, html, hydrationOn())
	if snap.Title != "Recovered From Payload" {
		t.Errorf("snap.Title = %q, want payload-recovered title (template strip must not break hydration)", snap.Title)
	}
	if !snap.Indexable {
		t.Errorf("Indexable = false, want true (the noindex was in an inert template)")
	}
}

// ── Fix 2: hreflang reorder must not fire a false change ─────────────────────

// TestHreflangReorderNoFalseChange: a reordered-but-equal hreflang set must not
// diff. Before the fix the set was marshalled in DOM order with no sort/dedup,
// so [en,fr] vs [fr,en] raw-string-differed and fired a false hreflang change.
func TestHreflangReorderNoFalseChange(t *testing.T) {
	const pageEN = `<!doctype html><html><head><title>t</title>` +
		`<link rel="alternate" hreflang="en" href="https://example.com/en">` +
		`<link rel="alternate" hreflang="fr" href="https://example.com/fr">` +
		`</head><body><p>words words words</p></body></html>`
	const pageFR = `<!doctype html><html><head><title>t</title>` +
		`<link rel="alternate" hreflang="fr" href="https://example.com/fr">` +
		`<link rel="alternate" hreflang="en" href="https://example.com/en">` +
		`</head><body><p>words words words</p></body></html>`

	old := extractSnapshot(t, pageEN)
	neu := extractSnapshot(t, pageFR)
	if old.Hreflang != neu.Hreflang {
		t.Errorf("hreflang differs on reorder-only:\n old=%q\n new=%q", old.Hreflang, neu.Hreflang)
	}
	old.ID, neu.ID = 1, 2
	for _, ch := range diff.Compare(neu, old, 3, time.Now()) {
		if ch.Field == "hreflang" {
			t.Errorf("hreflang change fired on a reorder-only difference: %+v", ch)
		}
	}
}

// TestHreflangGenuineChangeStillFires: adding a new hreflang still fires (the
// sort/dedup must not suppress a real change).
func TestHreflangGenuineChangeStillFires(t *testing.T) {
	const pageOld = `<!doctype html><html><head><title>t</title>` +
		`<link rel="alternate" hreflang="en" href="https://example.com/en">` +
		`</head><body><p>words words words</p></body></html>`
	const pageNew = `<!doctype html><html><head><title>t</title>` +
		`<link rel="alternate" hreflang="en" href="https://example.com/en">` +
		`<link rel="alternate" hreflang="fr" href="https://example.com/fr">` +
		`</head><body><p>words words words</p></body></html>`
	old := extractSnapshot(t, pageOld)
	neu := extractSnapshot(t, pageNew)
	if old.Hreflang == neu.Hreflang {
		t.Fatalf("hreflang did not change when fr was added: %q", neu.Hreflang)
	}
}

// ── Fix 3: schema_types reorder must not fire a false change ─────────────────

// TestSchemaTypesReorderNoFalseChange: a reordered-but-equal schema_types set
// must not diff. Before the fix types were appended in traversal order and
// marshalled with no sort/dedup, so [Product,Offer] vs [Offer,Product] fired a
// false schema_types change.
func TestSchemaTypesReorderNoFalseChange(t *testing.T) {
	const pageA = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"Product","name":"p"}</script>` +
		`<script type="application/ld+json">{"@type":"Offer","price":"1"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	const pageB = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"Offer","price":"1"}</script>` +
		`<script type="application/ld+json">{"@type":"Product","name":"p"}</script>` +
		`</head><body><p>words words words</p></body></html>`

	old := extractSnapshot(t, pageA)
	neu := extractSnapshot(t, pageB)
	if old.SchemaTypes != neu.SchemaTypes {
		t.Errorf("schema_types differs on reorder-only:\n old=%q\n new=%q", old.SchemaTypes, neu.SchemaTypes)
	}
	old.ID, neu.ID = 1, 2
	for _, ch := range diff.Compare(neu, old, 3, time.Now()) {
		if ch.Field == "schema_types" {
			t.Errorf("schema_types change fired on a reorder-only difference: %+v", ch)
		}
	}
}

// TestSchemaTypesGenuineChangeStillFires: adding or removing a type still fires.
func TestSchemaTypesGenuineChangeStillFires(t *testing.T) {
	const pageOld = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"Product","name":"p"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	const pageNew = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"Product","name":"p"}</script>` +
		`<script type="application/ld+json">{"@type":"Offer","price":"1"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	old := extractSnapshot(t, pageOld)
	neu := extractSnapshot(t, pageNew)
	if old.SchemaTypes == neu.SchemaTypes {
		t.Fatalf("schema_types did not change when Offer was added: %q", neu.SchemaTypes)
	}
	old.ID, neu.ID = 1, 2
	fired := false
	for _, ch := range diff.Compare(neu, old, 3, time.Now()) {
		if ch.Field == "schema_types" {
			fired = true
		}
	}
	if !fired {
		t.Errorf("schema_types change did NOT fire when Offer was genuinely added")
	}
}

// TestSchemaTypesDedup: a type repeated across two blocks is collapsed to one
// entry (multiplicity flips must not churn).
func TestSchemaTypesDedup(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"Product","name":"a"}</script>` +
		`<script type="application/ld+json">{"@type":"Product","name":"b"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	count := 0
	for _, tp := range types {
		if tp == "Product" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Product appears %d times in %v, want 1 (deduped)", count, types)
	}
}

// ── Fix 4: @graph recursion (array OR single object) + IRI/CURIE @type ───────

// TestSchemaTypesNestedGraph: a @graph member that itself wraps a nested @graph
// must have its inner-member types collected (one-level @graph handling missed
// these).
func TestSchemaTypesNestedGraph(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@context":"https://schema.org","@graph":[` +
		`{"@type":"WebSite","name":"s"},` +
		`{"@graph":[{"@type":"Organization","name":"o"},{"@type":"LocalBusiness","name":"lb"}]}` +
		`]}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	for _, want := range []string{"WebSite", "Organization", "LocalBusiness"} {
		if !containsType(types, want) {
			t.Errorf("nested @graph: got %v, want it to contain %q", types, want)
		}
	}
}

// TestSchemaTypesSingleObjectGraph: @graph may be a single object (not only an
// array). Its @type must be collected.
func TestSchemaTypesSingleObjectGraph(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@context":"https://schema.org","@graph":{"@type":"Recipe","name":"r"}}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Recipe") {
		t.Errorf("single-object @graph: got %v, want it to contain Recipe", types)
	}
}

// TestSchemaTypesIRINormalized: a full-IRI @type (https://schema.org/Product)
// must be normalized to its bare term "Product".
func TestSchemaTypesIRINormalized(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"https://schema.org/Product","name":"p"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Product") {
		t.Errorf("full-IRI @type: got %v, want bare term Product", types)
	}
	if containsType(types, "https://schema.org/Product") {
		t.Errorf("full-IRI @type was stored un-normalized: %v", types)
	}
}

// TestSchemaTypesTrailingSlashIRI: a full-IRI @type with a trailing slash
// ("https://schema.org/Product/") must still normalize to the bare term, not be
// silently dropped by taking the empty final segment after the trailing slash.
func TestSchemaTypesTrailingSlashIRI(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"https://schema.org/Product/","name":"p"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Product") {
		t.Errorf("trailing-slash IRI @type: got %v, want bare term Product", types)
	}
}

// TestSchemaTypesCURIENormalized: a CURIE @type ("schema:Product") must be
// normalized to its bare term "Product".
func TestSchemaTypesCURIENormalized(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"schema:Article","headline":"h"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Article") {
		t.Errorf("CURIE @type: got %v, want bare term Article", types)
	}
	if containsType(types, "schema:Article") {
		t.Errorf("CURIE @type was stored un-normalized: %v", types)
	}
}

// TestSchemaTypesHTTPIRINormalized: the http:// (not https) IRI form normalizes
// too.
func TestSchemaTypesHTTPIRINormalized(t *testing.T) {
	const html = `<!doctype html><html><head><title>t</title>` +
		`<script type="application/ld+json">{"@type":"http://schema.org/Event","name":"e"}</script>` +
		`</head><body><p>words words words</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Event") {
		t.Errorf("http IRI @type: got %v, want bare term Event", types)
	}
}
