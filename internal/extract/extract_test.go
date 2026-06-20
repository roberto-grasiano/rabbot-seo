package extract

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// extractSchemaTypes runs Extract over the given HTML body (text/html) and
// returns the decoded snap.SchemaTypes slice.
func extractSchemaTypes(t *testing.T, html string) []string {
	t.Helper()
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	var types []string
	if snap.SchemaTypes != "" {
		if uerr := json.Unmarshal([]byte(snap.SchemaTypes), &types); uerr != nil {
			t.Fatalf("SchemaTypes not valid JSON: %v (%q)", uerr, snap.SchemaTypes)
		}
	}
	return types
}

func containsType(types []string, want string) bool {
	for _, tp := range types {
		if tp == want {
			return true
		}
	}
	return false
}

// extractSnapshot runs Extract over an HTML body (text/html) and returns the
// full snapshot, failing on a non-nil error. Used by the JSON-LD hardening
// tests that assert on both snap.JSONLD and snap.JSONLDInvalidCount.
func extractSnapshot(t *testing.T, html string) model.Snapshot {
	t.Helper()
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	return snap
}

// decodeLDBlocks decodes snap.JSONLD (a JSON array of raw blocks) back into its
// element strings so a test can assert which sibling blocks survived.
func decodeLDBlocks(t *testing.T, jsonld string) []string {
	t.Helper()
	if jsonld == "" || jsonld == "null" {
		return nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(jsonld), &raws); err != nil {
		t.Fatalf("snap.JSONLD not a JSON array: %v (%q)", err, jsonld)
	}
	out := make([]string, len(raws))
	for i, r := range raws {
		out[i] = string(r)
	}
	return out
}

// TestExtractJSONLDMalformedBlockDoesNotVoidColumn (A4 prerequisite 1): one
// malformed JSON-LD block must no longer void the whole jsonld column. Before
// the fix, json.Marshal of a []json.RawMessage holding invalid JSON errored, so
// the `if err == nil` guard left snap.JSONLD == "" and silently dropped EVERY
// valid sibling block. After the fix only parseable blocks are appended, the
// valid sibling is retained, and the reject is counted into JSONLDInvalidCount.
func TestExtractJSONLDMalformedBlockDoesNotVoidColumn(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">{"@type":"Product","name":"Good"}</script>
<script type="application/ld+json">{ this is not valid json }</script>
</head><body><p>hi</p></body></html>`
	snap := extractSnapshot(t, html)

	blocks := decodeLDBlocks(t, snap.JSONLD)
	if len(blocks) != 1 {
		t.Fatalf("JSONLD blocks = %d (%q), want 1 valid sibling retained", len(blocks), snap.JSONLD)
	}
	if !strings.Contains(blocks[0], `"Product"`) {
		t.Errorf("retained block = %q, want the valid Product block", blocks[0])
	}
	if snap.JSONLDInvalidCount != 1 {
		t.Errorf("JSONLDInvalidCount = %d, want 1", snap.JSONLDInvalidCount)
	}
	// The valid block's @type must still reach schema_types.
	var types []string
	if snap.SchemaTypes != "" {
		_ = json.Unmarshal([]byte(snap.SchemaTypes), &types)
	}
	if !containsType(types, "Product") {
		t.Errorf("SchemaTypes = %v, want it to contain Product", types)
	}
}

// TestExtractJSONLDAllValidZeroInvalidCount: a page with only well-formed
// blocks records JSONLDInvalidCount == 0 (no false positives).
func TestExtractJSONLDAllValidZeroInvalidCount(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">{"@type":"Article","headline":"x"}</script>
</head><body><p>hi</p></body></html>`
	snap := extractSnapshot(t, html)
	if snap.JSONLDInvalidCount != 0 {
		t.Errorf("JSONLDInvalidCount = %d, want 0 for all-valid markup", snap.JSONLDInvalidCount)
	}
	if len(decodeLDBlocks(t, snap.JSONLD)) != 1 {
		t.Errorf("JSONLD = %q, want one retained block", snap.JSONLD)
	}
}

// TestExtractJSONLDTopLevelArrayTypes (A4 prerequisite 2): a legal top-level
// array block [{...},{...}] must contribute each member's @type to
// schema_types. Before the fix the loop unmarshalled into map[string]any only,
// so an array block contributed zero schema_types. The retained block count is
// 1 (the array is one <script> block); the member @types are collected.
func TestExtractJSONLDTopLevelArrayTypes(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">[{"@type":"Product","name":"p"},{"@type":["Article","NewsArticle"],"headline":"h"}]</script>
</head><body><p>hi</p></body></html>`
	snap := extractSnapshot(t, html)
	var types []string
	if snap.SchemaTypes != "" {
		_ = json.Unmarshal([]byte(snap.SchemaTypes), &types)
	}
	if !containsType(types, "Product") {
		t.Errorf("top-level array: got %v, want it to contain Product", types)
	}
	if !containsType(types, "Article") || !containsType(types, "NewsArticle") {
		t.Errorf("top-level array member array @type: got %v, want Article and NewsArticle", types)
	}
	if snap.JSONLDInvalidCount != 0 {
		t.Errorf("JSONLDInvalidCount = %d, want 0 for a valid array block", snap.JSONLDInvalidCount)
	}
	if got := len(decodeLDBlocks(t, snap.JSONLD)); got != 1 {
		t.Errorf("JSONLD blocks = %d, want 1 (the array is one block)", got)
	}
}

// TestExtractJSONLDScalarType: a top-level scalar "@type":"Article" is collected.
func TestExtractJSONLDScalarType(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">{"@type":"Article","headline":"x"}</script>
</head><body><p>hi</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Article") {
		t.Errorf("scalar @type: got %v, want it to contain Article", types)
	}
}

// TestExtractJSONLDArrayType: a multi-type "@type":["Article","NewsArticle"]
// must yield both types, not none.
func TestExtractJSONLDArrayType(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">{"@type":["Article","NewsArticle"],"headline":"x"}</script>
</head><body><p>hi</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "Article") || !containsType(types, "NewsArticle") {
		t.Errorf("array @type: got %v, want both Article and NewsArticle", types)
	}
}

// TestExtractJSONLDGraphMemberTypes: the Yoast/WordPress {"@graph":[...]} shape
// has no top-level @type; the member @type values must still be collected.
func TestExtractJSONLDGraphMemberTypes(t *testing.T) {
	const html = `<html><head><title>t</title>
<script type="application/ld+json">{"@context":"https://schema.org","@graph":[{"@type":"WebSite","name":"s"},{"@type":["Organization","LocalBusiness"],"name":"o"}]}</script>
</head><body><p>hi</p></body></html>`
	types := extractSchemaTypes(t, html)
	if !containsType(types, "WebSite") {
		t.Errorf("@graph member scalar @type: got %v, want it to contain WebSite", types)
	}
	if !containsType(types, "Organization") || !containsType(types, "LocalBusiness") {
		t.Errorf("@graph member array @type: got %v, want Organization and LocalBusiness", types)
	}
}

// TestExtractDeepDOMReturnsSentinel (deep-DOM): x/net/html caps the open-element
// stack at 512 nodes and returns an error even for well-formed fully-closed
// nesting. Extract must surface a typed sentinel so the scheduler can degrade
// honestly.
func TestExtractDeepDOMReturnsSentinel(t *testing.T) {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>t</title></head><body>")
	const depth = 600
	for i := 0; i < depth; i++ {
		b.WriteString("<div>")
	}
	b.WriteString("deep content")
	for i := 0; i < depth; i++ {
		b.WriteString("</div>")
	}
	b.WriteString("</body></html>")
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(b.String()),
	}
	_, _, err := NewExtractor().Extract(res, "")
	if err == nil {
		t.Fatalf("Extract() on 600-deep DOM: error = nil, want %v", ErrDOMTooDeep)
	}
	if !errors.Is(err, ErrDOMTooDeep) {
		t.Fatalf("Extract() on 600-deep DOM: err = %v, want errors.Is(err, ErrDOMTooDeep)", err)
	}
}

// TestExtractNormalDOMNotTooDeep: a normally-nested document must NOT be flagged
// ErrDOMTooDeep.
func TestExtractNormalDOMNotTooDeep(t *testing.T) {
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(fullHTML),
	}
	_, _, err := NewExtractor().Extract(res, "")
	if errors.Is(err, ErrDOMTooDeep) {
		t.Fatalf("normal DOM wrongly flagged ErrDOMTooDeep: %v", err)
	}
}

func httpHeader(ct string) http.Header {
	return http.Header{"Content-Type": {ct}}
}

// extractRes runs Extract over a fetcher.Result and returns the snapshot,
// failing on a non-nil error. Lets a test supply a full Header (multiple lines,
// Link headers) rather than the single-Content-Type httpHeader helper.
func extractRes(t *testing.T, res fetcher.Result) model.Snapshot {
	t.Helper()
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	return snap
}

// TestExtractXRobotsTagMultiLine (#1): an origin may emit X-Robots-Tag as
// SEPARATE header lines (one per directive), which is semantically equivalent to
// a single comma-joined line. http.Header.Get returns only the FIRST line, so a
// "noindex" on line 2+ was silently dropped and the page wrongly judged
// indexable. Extract must join ALL X-Robots-Tag lines so every directive is seen.
func TestExtractXRobotsTagMultiLine(t *testing.T) {
	const html = `<html><head><title>t</title></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header: http.Header{
			"Content-Type": {"text/html"},
			// Two lines: a bot UA on line 1, the noindex on line 2.
			"X-Robots-Tag": {"unavailable_after: 25 Jun 2030 00:00:00 GMT", "noindex"},
		},
		Body: []byte(html),
	}
	snap := extractRes(t, res)
	// The stored value must contain BOTH lines (joined), not just the first.
	if !strings.Contains(snap.XRobotsTag, "noindex") {
		t.Errorf("XRobotsTag = %q, want it to include the 2nd-line noindex", snap.XRobotsTag)
	}
	if snap.Indexable || snap.IndexabilityReason != "x_robots_tag_noindex" {
		t.Errorf("multi-line X-Robots-Tag noindex: Indexable=%v reason=%q, want x_robots_tag_noindex",
			snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractGooglebotMetaNoindex (#2/#3): a page with no generic meta-robots
// but a googlebot-specific <meta name="googlebot" content="noindex"> must be
// judged noindex. The old code read only meta[name="robots"] via .First(), so a
// googlebot directive (and any 2nd robots tag) was invisible.
func TestExtractGooglebotMetaNoindex(t *testing.T) {
	const html = `<html><head><title>t</title>
<meta name="googlebot" content="noindex"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Indexable || snap.IndexabilityReason != "meta_robots_noindex" {
		t.Errorf("googlebot meta noindex: Indexable=%v reason=%q, want meta_robots_noindex",
			snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractSecondRobotsMetaNoindex (#3): when two <meta name="robots"> tags are
// present and the SECOND carries noindex, the page must be judged noindex. The
// old .First() read only the first tag.
func TestExtractSecondRobotsMetaNoindex(t *testing.T) {
	const html = `<html><head><title>t</title>
<meta name="robots" content="index, follow">
<meta name="robots" content="noindex"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Indexable || snap.IndexabilityReason != "meta_robots_noindex" {
		t.Errorf("second robots meta noindex: Indexable=%v reason=%q, want meta_robots_noindex",
			snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractCaseInsensitiveMetaRobotsName (#3): the meta name match must be
// case-insensitive — <meta name="ROBOTS" content="noindex"> is a valid robots
// tag. The old CSS attribute selector meta[name="robots"] is case-sensitive on
// the attribute value, so an uppercased name slipped through as indexable.
func TestExtractCaseInsensitiveMetaRobotsName(t *testing.T) {
	const html = `<html><head><title>t</title>
<meta name="ROBOTS" content="noindex"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Indexable || snap.IndexabilityReason != "meta_robots_noindex" {
		t.Errorf("uppercase ROBOTS meta noindex: Indexable=%v reason=%q, want meta_robots_noindex",
			snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractCanonicalFromLinkHeader (#4): when the DOM head has NO canonical, a
// rel="canonical" in the HTTP Link header must be honored, resolved relative to
// the final URL, and recorded with CanonicalType="header".
func TestExtractCanonicalFromLinkHeader(t *testing.T) {
	const html = `<html><head><title>t</title></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header: http.Header{
			"Content-Type": {"text/html"},
			"Link":         {`</canonical-target>; rel="canonical"`},
		},
		Body: []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Canonical != "https://example.com/canonical-target" {
		t.Errorf("Canonical = %q, want resolved header canonical https://example.com/canonical-target", snap.Canonical)
	}
	if snap.CanonicalType != "header" {
		t.Errorf("CanonicalType = %q, want header", snap.CanonicalType)
	}
}

// TestExtractDOMCanonicalWinsOverLinkHeader (#4): when BOTH a DOM head canonical
// and an HTTP Link-header canonical are present, the DOM (verifiable ground
// truth) wins and CanonicalType stays "link".
func TestExtractDOMCanonicalWinsOverLinkHeader(t *testing.T) {
	const html = `<html><head><title>t</title>
<link rel="canonical" href="https://example.com/dom-canonical"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header: http.Header{
			"Content-Type": {"text/html"},
			"Link":         {`<https://example.com/header-canonical>; rel="canonical"`},
		},
		Body: []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Canonical != "https://example.com/dom-canonical" {
		t.Errorf("Canonical = %q, want DOM canonical to win", snap.Canonical)
	}
	if snap.CanonicalType != "link" {
		t.Errorf("CanonicalType = %q, want link (DOM wins)", snap.CanonicalType)
	}
}

// TestExtractLinkHeaderCanonicalMultipleRels (#4): the Link header may carry
// several link relations across lines/entries; only the rel="canonical" one is
// used, and rel tokens are matched case-insensitively / space-tolerantly.
func TestExtractLinkHeaderCanonicalMultipleRels(t *testing.T) {
	const html = `<html><head><title>t</title></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/list",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header: http.Header{
			"Content-Type": {"text/html"},
			"Link": {
				`<https://example.com/style.css>; rel=preload; as=style`,
				`<https://example.com/list>; rel="canonical", <https://example.com/next>; rel="next"`,
			},
		},
		Body: []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Canonical != "https://example.com/list" {
		t.Errorf("Canonical = %q, want the rel=canonical entry", snap.Canonical)
	}
	if snap.CanonicalType != "header" {
		t.Errorf("CanonicalType = %q, want header", snap.CanonicalType)
	}
}

// TestExtractQueryDifferingCanonicalIsCanonicalizedAway (#15): a paginated page
// /list?page=2 whose canonical points to /list?page=1 is canonicalized away — the
// query string differs, so it is NOT self-referential. The old host+path-only
// sameURL check ignored the query and wrongly judged it self/indexable.
func TestExtractQueryDifferingCanonicalIsCanonicalizedAway(t *testing.T) {
	const html = `<html><head><title>t</title>
<link rel="canonical" href="https://example.com/list?page=1"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/list?page=2",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap := extractRes(t, res)
	if snap.Indexable || snap.IndexabilityReason != "canonicalized_away" {
		t.Errorf("query-differing canonical: Indexable=%v reason=%q, want canonicalized_away",
			snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractSelfCanonicalWithQueryIsIndexable (#15): a page whose canonical
// matches its own URL INCLUDING the query (same params, any order) stays
// self/indexable.
func TestExtractSelfCanonicalWithQueryIsIndexable(t *testing.T) {
	const html = `<html><head><title>t</title>
<link rel="canonical" href="https://example.com/list?b=2&a=1"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/list?a=1&b=2",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap := extractRes(t, res)
	if !snap.Indexable || snap.IndexabilityReason != "indexable" {
		t.Errorf("self-canonical with reordered query: Indexable=%v reason=%q, want indexable",
			snap.Indexable, snap.IndexabilityReason)
	}
}

const fullHTML = `<!DOCTYPE html><html lang="en"><head>
<title>Page Title</title>
<meta name="description" content="A description">
<meta name="robots" content="index, follow">
<link rel="canonical" href="https://example.com/page">
<link rel="alternate" hreflang="fr" href="https://example.com/fr/page">
<meta property="og:title" content="OG Title">
<meta name="twitter:card" content="summary">
<script type="application/ld+json">{"@type":"Article","headline":"x"}</script>
</head><body>
<h1>Main Heading</h1><h2>Sub</h2>
<p>Some article content with a number of words present here for counting purposes today.</p>
<a href="/internal">internal</a>
<a href="https://other.com/x">external</a>
<img src="a.jpg" alt="has alt">
<img src="b.jpg">
</body></html>`

func TestExtractSignals(t *testing.T) {
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		StatusType: model.StatusPage,
		Header:     http.Header{"X-Robots-Tag": []string{""}},
		Body:       []byte(fullHTML),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if snap.Title != "Page Title" {
		t.Errorf("Title = %q", snap.Title)
	}
	if snap.MetaDescription != "A description" {
		t.Errorf("MetaDescription = %q", snap.MetaDescription)
	}
	if snap.MetaRobots != "index, follow" {
		t.Errorf("MetaRobots = %q", snap.MetaRobots)
	}
	if snap.Canonical != "https://example.com/page" {
		t.Errorf("Canonical = %q", snap.Canonical)
	}
	if snap.InternalLinkCount != 1 {
		t.Errorf("InternalLinkCount = %d, want 1", snap.InternalLinkCount)
	}
	if snap.ExternalLinkCount != 1 {
		t.Errorf("ExternalLinkCount = %d, want 1", snap.ExternalLinkCount)
	}
	if snap.ImageCount != 2 {
		t.Errorf("ImageCount = %d, want 2", snap.ImageCount)
	}
	if snap.MissingAltCount != 1 {
		t.Errorf("MissingAltCount = %d, want 1", snap.MissingAltCount)
	}
	if snap.WordCount == 0 {
		t.Errorf("WordCount should be > 0")
	}
	if snap.ContentSHA256 == "" {
		t.Errorf("ContentSHA256 empty")
	}
	if !snap.Indexable || snap.IndexabilityReason != "indexable" {
		t.Errorf("Indexable=%v reason=%q, want indexable", snap.Indexable, snap.IndexabilityReason)
	}
	if snap.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d", snap.HTTPStatus)
	}
	if len(snap.RawHTML) == 0 {
		t.Errorf("RawHTML should be populated for ok fetch")
	}
}

// TestExtractMissingAltSemantics (A5 prerequisite): MissingAltCount counts only
// images whose alt attribute is ABSENT. An explicit alt="" is the correct
// decorative-image convention (declares "this image is decorative, skip it") and
// must NOT count as missing; the spec's settled semantics count "only images with
// no alt attribute". A present-but-whitespace-only alt (alt="   ") likewise has
// the attribute declared and does not count. ImageCount is unaffected by the fix
// — every <img> still counts toward ImageCount regardless of its alt.
func TestExtractMissingAltSemantics(t *testing.T) {
	const html = `<html><head><title>t</title></head><body>
<img src="missing.jpg">
<img src="decorative.jpg" alt="">
<img src="described.jpg" alt="a real description">
<img src="whitespace.jpg" alt="   ">
</body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	// All four <img> elements count toward ImageCount.
	if snap.ImageCount != 4 {
		t.Errorf("ImageCount = %d, want 4 (alt fix must not change image counting)", snap.ImageCount)
	}
	// Only the attribute-absent image counts as missing alt: the empty alt="",
	// the described alt, and the whitespace-only alt all DECLARE the attribute.
	if snap.MissingAltCount != 1 {
		t.Errorf("MissingAltCount = %d, want 1 (only the <img> with no alt attribute; empty/whitespace/real alt all declared)", snap.MissingAltCount)
	}
}

// TestExtractEmptyAltNotMissing pins the single decision behind the fix in
// isolation: a page whose only image carries an explicit empty alt="" reports
// ZERO missing alts (decorative convention), even though ImageCount is 1.
func TestExtractEmptyAltNotMissing(t *testing.T) {
	const html = `<html><head><title>t</title></head><body>
<img src="spacer.gif" alt="">
</body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if snap.ImageCount != 1 {
		t.Errorf("ImageCount = %d, want 1", snap.ImageCount)
	}
	if snap.MissingAltCount != 0 {
		t.Errorf("MissingAltCount = %d, want 0 (explicit alt=\"\" is decorative, not missing)", snap.MissingAltCount)
	}
}

func TestClassifyLink(t *testing.T) {
	const host = "example.com"
	cases := []struct {
		href string
		want linkKind
	}{
		{"/about", linkInternal},
		{"about.html", linkInternal},
		{"https://example.com/x", linkInternal},
		{"https://other.com/x", linkExternal},
		{"http://example.com/x", linkInternal},
		// protocol-relative to another host must count as external, not internal.
		{"//cdn.othersite.com/app.js", linkExternal},
		{"//example.com/self.js", linkInternal},
		// non-navigational schemes must be skipped, never counted as internal.
		{"mailto:a@b.com", linkSkip},
		{"tel:+15551234", linkSkip},
		{"javascript:void(0)", linkSkip},
	}
	for _, c := range cases {
		if got := classifyLink(c.href, host); got != c.want {
			t.Errorf("classifyLink(%q) = %d, want %d", c.href, got, c.want)
		}
	}
}

// TestExtractRelativeCanonicalSelfReferential (F8): a relative self-referential
// canonical (href="/page") must resolve against FinalURL and stay Indexable=true,
// not be flagged canonicalized_away.
func TestExtractRelativeCanonicalSelfReferential(t *testing.T) {
	const html = `<html><head><title>t</title>
<link rel="canonical" href="/page"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !snap.Indexable || snap.IndexabilityReason != "indexable" {
		t.Errorf("relative self-canonical: Indexable=%v reason=%q, want indexable", snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractSchemeOnlyCanonicalSelfReferential (F8): http->https self-canonical
// (migration) must stay Indexable=true.
func TestExtractSchemeOnlyCanonicalSelfReferential(t *testing.T) {
	const html = `<html><head><title>t</title>
<link rel="canonical" href="http://example.com/page"></head><body><p>hi</p></body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !snap.Indexable || snap.IndexabilityReason != "indexable" {
		t.Errorf("scheme-only self-canonical: Indexable=%v reason=%q, want indexable", snap.Indexable, snap.IndexabilityReason)
	}
}

// TestExtractMixedCaseHostLinks (F52): a mixed-case live host with a lowercase
// absolute internal link must count the link as internal, not external.
func TestExtractMixedCaseHostLinks(t *testing.T) {
	const html = `<html><head><title>t</title></head><body>
<a href="https://example.com/x">internal lowercase</a>
<a href="https://other.com/x">external</a>
</body></html>`
	res := fetcher.Result{
		FinalURL:   "https://Example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if snap.InternalLinkCount != 1 {
		t.Errorf("InternalLinkCount = %d, want 1 (mixed-case host)", snap.InternalLinkCount)
	}
	if snap.ExternalLinkCount != 1 {
		t.Errorf("ExternalLinkCount = %d, want 1", snap.ExternalLinkCount)
	}
}

// TestExtractNonHTMLBodyGated (F25): a 200 response with a non-HTML Content-Type
// (e.g. application/json) must not be parsed as HTML and stored as an empty SEO
// snapshot; Extract must return a non-nil error so the caller skips persistence.
func TestExtractNonHTMLBodyGated(t *testing.T) {
	res := fetcher.Result{
		FinalURL:   "https://example.com/data.json",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       []byte(`{"error":"not found","title":"should not become an SEO title"}`),
	}
	_, _, err := NewExtractor().Extract(res, "")
	if err == nil {
		t.Fatalf("Extract() on application/json body: error = nil, want non-nil (non-HTML gate)")
	}
}

// TestExtractLinkCountsWithMixedHrefs reproduces the issue-7 scenario end to end:
// a protocol-relative external link plus a mailto must yield external>=1 and must
// not inflate internal counts.
func TestExtractLinkCountsWithMixedHrefs(t *testing.T) {
	const html = `<html><head><title>t</title></head><body>
<a href="/local">x</a>
<a href="//cdn.othersite.com/app.js">y</a>
<a href="mailto:a@b.com">z</a>
</body></html>`
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     http.Header{},
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if snap.ExternalLinkCount != 1 {
		t.Errorf("ExternalLinkCount = %d, want 1 (protocol-relative external)", snap.ExternalLinkCount)
	}
	if snap.InternalLinkCount != 1 {
		t.Errorf("InternalLinkCount = %d, want 1 (only /local; mailto excluded)", snap.InternalLinkCount)
	}
}

func TestExtractReturnsInternalLinks(t *testing.T) {
	t.Parallel()
	html := `<html><head><title>T</title></head><body>` +
		`<a href="/a">a</a><a href="/a#x">dup</a><a href="https://ex.com/b">b</a>` +
		`<a href="https://other.com/c">ext</a><a href="mailto:x@y.z">m</a><a href="#frag">f</a>` +
		`</body></html>`
	res := fetcher.Result{
		FinalURL:   "https://ex.com/",
		HTTPStatus: 200,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	_, links, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := map[string]bool{}
	for _, l := range links {
		got[l] = true
	}
	if !got["https://ex.com/a"] || !got["https://ex.com/b"] {
		t.Errorf("missing internal links: %v", links)
	}
	if got["https://other.com/c"] {
		t.Errorf("external link leaked into discovery set: %v", links)
	}
	if len(links) != 2 {
		t.Errorf("want 2 unique internal links (a,b; #-frag deduped, mailto/external/# excluded), got %d: %v", len(links), links)
	}
}
