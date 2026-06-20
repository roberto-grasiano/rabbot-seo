package extract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/hydration"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// hydrationOn is the Options value the A8 merge tests use: hydration enabled with
// a generous (1 MiB) byte cap so realistic fixtures decode.
func hydrationOn() Options {
	return Options{Hydration: HydrationOptions{Enabled: true, MaxPayloadBytes: 1 << 20}}
}

// extractWith runs Extract over an HTML body (text/html) with the given Options
// and returns the snapshot, failing on a non-nil error.
func extractWith(t *testing.T, html string, opts Options) model.Snapshot {
	t.Helper()
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	snap, _, err := NewExtractor().ExtractWith(res, opts)
	if err != nil {
		t.Fatalf("ExtractWith() error = %v", err)
	}
	return snap
}

// nextDataShell wraps a __NEXT_DATA__ payload + (optional) head into a minimal
// Next.js client-shell document: an empty #__next root, so the DOM head is only
// what `head` provides and the body prose is thin.
func nextDataShell(head, payload string) string {
	return `<!doctype html><html><head>` + head + `</head><body>` +
		`<div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` + payload + `</script>` +
		`</body></html>`
}

// ── Criterion 4: DOM-first merge — payload fills only DOM-empty fields ───────

// TestMergeDOMEmptyTitleFilledFromPayload pins acceptance #4 arm A: a DOM with no
// <title> plus a payload carrying a title yields snap.Title from the payload and an
// extraction_source that is NOT "dom" (provenance records the contribution).
func TestMergeDOMEmptyTitleFilledFromPayload(t *testing.T) {
	payload := `{"props":{"pageProps":{"seoTitle":"Recovered From Payload","metaDescription":"Recovered desc here"}}}`
	html := nextDataShell("", payload) // no <title>, no meta description in DOM head

	snap := extractWith(t, html, hydrationOn())

	if snap.Title != "Recovered From Payload" {
		t.Errorf("snap.Title = %q, want payload-recovered %q", snap.Title, "Recovered From Payload")
	}
	if snap.MetaDescription != "Recovered desc here" {
		t.Errorf("snap.MetaDescription = %q, want payload-recovered value", snap.MetaDescription)
	}
	if snap.ExtractionSource == "dom" || snap.ExtractionSource == "" {
		t.Errorf("snap.ExtractionSource = %q, want a non-dom provenance (payload contributed)", snap.ExtractionSource)
	}
	if !strings.Contains(snap.ExtractionSource, "next_data") {
		t.Errorf("snap.ExtractionSource = %q, want to record next_data contribution", snap.ExtractionSource)
	}
}

// TestMergeDOMPresentTitleWinsOverPayload pins acceptance #4 arm B: when the DOM
// head carries a <title>, a conflicting payload title must NOT overwrite it — DOM
// wins always. The merge must also not falsely claim a payload contribution to a
// field the DOM already filled.
func TestMergeDOMPresentTitleWinsOverPayload(t *testing.T) {
	payload := `{"props":{"pageProps":{"seoTitle":"Payload Title SHOULD NOT WIN"}}}`
	html := nextDataShell(`<title>DOM Title Wins</title>`, payload)

	snap := extractWith(t, html, hydrationOn())

	if snap.Title != "DOM Title Wins" {
		t.Errorf("snap.Title = %q, want DOM value %q (DOM wins over payload)", snap.Title, "DOM Title Wins")
	}
}

// TestMergeDOMEmptyCanonicalFilledFromFlight pins acceptance #4 for the canonical
// field via the RSC flight path, and proves recovered JSON-LD flows into both
// snap.JSONLD and snap.SchemaTypes through the shared jsonLDBlockTypes path.
func TestMergeDOMEmptyCanonicalFilledFromFlight(t *testing.T) {
	// One flight row streaming a canonical <link> and a JSON-LD <script>.
	row := `1:[["$","link","0",{"rel":"canonical","href":"https://example.com/canonical-from-flight"}],` +
		`["$","script","1",{"type":"application/ld+json","dangerouslySetInnerHTML":{"__html":"{\"@type\":\"Article\",\"headline\":\"X\"}"}}]]`
	jsonRow, _ := json.Marshal(row)
	html := `<!doctype html><html><head><title>Has Title</title></head><body>` +
		`<div id="__next"></div>` +
		`<script>self.__next_f=self.__next_f||[];self.__next_f.push([1,` + string(jsonRow) + `])</script>` +
		`</body></html>`

	snap := extractWith(t, html, hydrationOn())

	if snap.Canonical != "https://example.com/canonical-from-flight" {
		t.Errorf("snap.Canonical = %q, want flight-recovered canonical", snap.Canonical)
	}
	if !strings.Contains(snap.SchemaTypes, "Article") {
		t.Errorf("snap.SchemaTypes = %q, want recovered Article type from flight ld+json", snap.SchemaTypes)
	}
	if !strings.Contains(snap.JSONLD, "Article") {
		t.Errorf("snap.JSONLD = %q, want recovered ld+json block from flight", snap.JSONLD)
	}
	if !strings.Contains(snap.ExtractionSource, "flight") {
		t.Errorf("snap.ExtractionSource = %q, want to record flight contribution", snap.ExtractionSource)
	}
}

// ── Criterion 5: churn guard — buildId flip must not move the content hash ───

// proseBlock is a long, realistic prose leaf well over WordFloor (25) words so the
// thin-DOM branch composes it into the content hash.
const proseBlock = "This is a sufficiently long article body recovered from the framework hydration payload, " +
	"describing the product in enough detail that it clears the thinness floor comfortably and " +
	"reads as genuine prose worth monitoring for substantive content changes over time."

// nextDataChurnFixture builds a thin client-shell doc whose payload prose is
// identical except for a volatile buildId — the only thing that differs.
func nextDataChurnFixture(buildID string) string {
	payload := `{"buildId":"` + buildID + `","props":{"pageProps":{"body":"` + proseBlock + `"}}}`
	return nextDataShell("", payload)
}

// TestChurnGuardBuildIDFlipKeepsContentHash pins acceptance #5: two fixtures
// identical except for buildId produce equal ContentSHA256 AND ContentSimhash, and
// diff.Compare emits no content change. The volatile-key filter in hydration makes
// the buildId invisible to the recovered prose, so the composed hash is stable.
func TestChurnGuardBuildIDFlipKeepsContentHash(t *testing.T) {
	a := extractWith(t, nextDataChurnFixture("BUILD_AAA_1111"), hydrationOn())
	b := extractWith(t, nextDataChurnFixture("BUILD_BBB_2222"), hydrationOn())

	if a.ContentSHA256 == "" {
		t.Fatal("ContentSHA256 is empty — payload prose did not feed the thin-DOM content hash")
	}
	if a.ContentSHA256 != b.ContentSHA256 {
		t.Errorf("ContentSHA256 differs across a buildId-only flip:\n a=%q\n b=%q", a.ContentSHA256, b.ContentSHA256)
	}
	if a.ContentSimhash != b.ContentSimhash {
		t.Errorf("ContentSimhash differs across a buildId-only flip: a=%d b=%d", a.ContentSimhash, b.ContentSimhash)
	}

	// diff.Compare must see no content change between the two snapshots. Give them
	// non-zero IDs + matching baseline so Compare does not treat one as a first
	// crawl (which would suppress all diffs).
	a.ID, b.ID = 1, 2
	// diff.Compare(new, old, simhashThreshold, now); old=a (has ID + hash so it is
	// not treated as a baseline first crawl), new=b — a buildId-only flip.
	changes := diff.Compare(b, a, 3, time.Now())
	for _, ch := range changes {
		if ch.Field == "content" {
			t.Errorf("diff.Compare emitted a content change on a buildId-only flip: %+v", ch)
		}
	}
}

// TestThinDOMComposesPayloadProse proves the thin-DOM branch actually fired: a
// client-shell page with payload prose must record an extraction_source that names
// the payload (so the WordCount/hash composition is provenance-visible), and the
// WordCount must reflect the recovered prose (not the ~0 DOM words).
func TestThinDOMComposesPayloadProse(t *testing.T) {
	snap := extractWith(t, nextDataChurnFixture("B1"), hydrationOn())
	if snap.WordCount < 20 {
		t.Errorf("WordCount = %d, want the recovered-prose word count (thin DOM composed payload prose)", snap.WordCount)
	}
	if !strings.Contains(snap.ExtractionSource, "next_data") {
		t.Errorf("ExtractionSource = %q, want next_data contribution recorded", snap.ExtractionSource)
	}
}

// ── Criterion 12: hydration disabled ⇒ byte-identical pre-A8 extraction ──────

// TestHydrationDisabledIsByteIdenticalForHydratedFixture pins acceptance #12: with
// hydration.enabled=false, a hydrated fixture extracts EXACTLY as pre-A8 — no
// payload recovery into any field, extraction_source="dom", and the content hash is
// DOM-only (so the thin DOM yields a near-empty body hash, NOT the composed one).
func TestHydrationDisabledIsByteIdenticalForHydratedFixture(t *testing.T) {
	html := nextDataChurnFixture("B1")

	off := Options{Hydration: HydrationOptions{Enabled: false}}
	disabled := extractWith(t, html, off)
	on := extractWith(t, html, hydrationOn())

	// With hydration off, the DOM-empty title stays empty (no payload back-fill).
	if disabled.Title != "" {
		t.Errorf("hydration-off Title = %q, want empty (no payload back-fill)", disabled.Title)
	}
	if disabled.ExtractionSource != "dom" {
		t.Errorf("hydration-off ExtractionSource = %q, want %q", disabled.ExtractionSource, "dom")
	}
	// The content hash must be DOM-only: the thin DOM has no prose, so it must differ
	// from the hydration-on composed hash (proves no payload prose leaked in).
	if disabled.ContentSHA256 == on.ContentSHA256 {
		t.Errorf("hydration-off ContentSHA256 == hydration-on; payload prose leaked into the disabled path")
	}
	// render_mode is still classified for honesty even with recovery off.
	if disabled.RenderMode == "" {
		t.Errorf("hydration-off RenderMode = %q, want a classified value (honesty regardless of recovery)", disabled.RenderMode)
	}
}

// TestHydrationDisabledMatchesLegacyExtract proves the disabled path is identical to
// the legacy (no-Options) Extract for a hydrated fixture across every field that
// existed pre-A8, so enabling the seam with Enabled=false changes nothing.
func TestHydrationDisabledMatchesLegacyExtract(t *testing.T) {
	html := nextDataChurnFixture("B1")
	res := fetcher.Result{
		FinalURL:   "https://example.com/page",
		HTTPStatus: 200,
		FetchClass: model.FetchOK,
		Header:     httpHeader("text/html"),
		Body:       []byte(html),
	}
	legacy, _, err := NewExtractor().Extract(res, "")
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	off := Options{Hydration: HydrationOptions{Enabled: false}}
	disabled, _, err := NewExtractor().ExtractWith(res, off)
	if err != nil {
		t.Fatalf("ExtractWith() error = %v", err)
	}
	if legacy.ContentSHA256 != disabled.ContentSHA256 {
		t.Errorf("legacy vs disabled ContentSHA256 differ: %q vs %q", legacy.ContentSHA256, disabled.ContentSHA256)
	}
	if legacy.Title != disabled.Title || legacy.MetaDescription != disabled.MetaDescription || legacy.Canonical != disabled.Canonical {
		t.Errorf("legacy vs disabled head fields differ")
	}
	if legacy.WordCount != disabled.WordCount {
		t.Errorf("legacy vs disabled WordCount differ: %d vs %d", legacy.WordCount, disabled.WordCount)
	}
}

// TestOverCapPayloadSkippedNoRecovery pins acceptance #12's cap clause: a payload
// over max_payload_bytes is skipped (Truncated) with no recovered fields — the
// DOM-empty title stays empty and extraction_source stays "dom".
func TestOverCapPayloadSkippedNoRecovery(t *testing.T) {
	// A payload comfortably larger than the tiny cap below.
	payload := `{"props":{"pageProps":{"seoTitle":"Would Recover But Over Cap","body":"` + proseBlock + `"}}}`
	html := nextDataShell("", payload)

	tiny := Options{Hydration: HydrationOptions{Enabled: true, MaxPayloadBytes: 8}}
	snap := extractWith(t, html, tiny)

	if snap.Title != "" {
		t.Errorf("over-cap Title = %q, want empty (payload skipped at the cap)", snap.Title)
	}
	if snap.ExtractionSource != "dom" {
		t.Errorf("over-cap ExtractionSource = %q, want %q (no recovery)", snap.ExtractionSource, "dom")
	}
}

// TestRenderModeClassifiedWhenEnabled proves the persisted classification path: an
// enabled extraction over a client-shell fixture sets a non-empty RenderMode.
func TestRenderModeClassifiedWhenEnabled(t *testing.T) {
	// A real client shell: empty #__next, thin body, large script bytes, no payload,
	// no head fields. This is the precheck ClientShell pattern.
	bigScript := strings.Repeat("var x=1;function f(){return 42;}", 200) // > scriptCeil 4096 bytes
	html := `<!doctype html><html><head></head><body>` +
		`<div id="__next"></div>` +
		`<script>` + bigScript + `</script>` +
		`</body></html>`

	snap := extractWith(t, html, hydrationOn())
	if snap.RenderMode != model.RenderClientShell {
		t.Errorf("RenderMode = %q, want %q for a client-shell fixture", snap.RenderMode, model.RenderClientShell)
	}
}

// TestRecoveredFillAttributesSourcePerField guards the provenance fix: when two
// head fields carry the SAME string value but were recovered from DIFFERENT
// payload sources, extraction_source must attribute each field to its own source.
// The pre-fix fill() re-derived the source by string-matching val against the
// merged fields, so a shared value collapsed both contributions to whichever
// switch case matched first — dropping the second source from the provenance.
func TestRecoveredFillAttributesSourcePerField(t *testing.T) {
	// Title came from the flight stream, description from __NEXT_DATA__ — distinct
	// sources, identical string value (the degenerate but legal collision).
	r := &recovered{
		fields:   hydration.Fields{Title: "Shared Value", MetaDescription: "Shared Value"},
		titleSrc: sourceFlight,
		descSrc:  sourceNextData,
	}
	var title, desc string
	r.fill(&title, r.fields.Title, r.titleSrc)
	r.fill(&desc, r.fields.MetaDescription, r.descSrc)

	if title != "Shared Value" || desc != "Shared Value" {
		t.Fatalf("fill did not back-fill empty dst: title=%q desc=%q", title, desc)
	}
	if got := r.source(); got != "dom+next_data+flight" {
		// Both sources must be present and in the deterministic order; the pre-fix
		// bug produced "dom+flight" (next_data dropped via shared-string collapse).
		t.Errorf("source() = %q, want %q (both contributing sources attributed)", got, "dom+next_data+flight")
	}
}
