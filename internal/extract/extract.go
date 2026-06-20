package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/hydration"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// ErrNonHTML is returned by Extract when the response Content-Type is present
// and is not an HTML type. Callers should skip persistence rather than store a
// binary/JSON body parsed as an empty HTML snapshot.
var ErrNonHTML = errors.New("extract: non-HTML content type")

// ErrDOMTooDeep is returned (wrapped) by Extract when the HTML nests deeper than
// the golang.org/x/net/html open-element stack limit (512 nodes). The parser
// rejects such documents even when the nesting is well-formed and fully closed,
// so this is a hard parser limit, not a malformed-input signal. Extract wraps it
// (fmt.Errorf("...: %w", ErrDOMTooDeep)) so the scheduler can detect it via
// errors.Is and degrade honestly rather than treat it as an opaque failure.
var ErrDOMTooDeep = errors.New("extract: HTML DOM nesting exceeds parser limit (512 nodes)")

// domTooDeepMarker is the substring golang.org/x/net/html embeds in the error it
// returns when the open-element stack overflows (it recovers an internal panic
// into err via fmt.Errorf("%s", panicErr)). Matching the message is the only way
// to distinguish this limit from other parse failures, as the package exports no
// typed error for it.
const domTooDeepMarker = "open stack of elements exceeds 512 nodes"

// HydrationOptions controls A8 hydration-payload recovery during extraction.
// Enabled is the master switch (crawler.hydration.enabled): when false, Extract
// is byte-identical to its pre-A8 behavior — no payload back-fill, no payload
// prose in the content hash, extraction_source="dom" (render_mode is still
// classified for honesty). MaxPayloadBytes (crawler.hydration.max_payload_bytes)
// is the per-payload decode cap handed to the hydration decoders: a payload over
// the cap is skipped (Truncated) and recovers nothing — a DoS guard for
// multi-megabyte embedded state. A non-positive cap disables the byte check.
type HydrationOptions struct {
	Enabled         bool
	MaxPayloadBytes int
}

// Options bundles the per-crawl knobs Extract needs. It replaces the bare
// contentSelector parameter as a value-type seam so new knobs (the A8 hydration
// switches) thread through CrawlFn/CrawlOne without growing the positional
// signature each time. ContentSelector overrides readability's main-text pick
// (per-site SiteConfig.content_selector); the zero Options{} means "no selector,
// hydration off" — exactly pre-A8 behavior.
type Options struct {
	ContentSelector string
	Hydration       HydrationOptions
}

type Extractor interface {
	// Extract is the pre-A8 entry point: it extracts with the given content
	// selector and hydration DISABLED (byte-identical to the original behavior).
	// It is retained so existing callers and tests compile unchanged; new callers
	// thread per-crawl knobs via ExtractWith.
	Extract(res fetcher.Result, contentSelector string) (model.Snapshot, []string, error)
	// ExtractWith extracts with the full Options seam (content selector + A8
	// hydration recovery). It is the path the crawl pipeline uses.
	ExtractWith(res fetcher.Result, opts Options) (model.Snapshot, []string, error)
}

type extractor struct{}

// NewExtractor returns the default goquery+readability extractor.
func NewExtractor() Extractor { return extractor{} }

// Extract preserves the pre-A8 signature: a content selector with hydration off.
// It delegates to ExtractWith so there is one implementation of the merge.
func (e extractor) Extract(res fetcher.Result, contentSelector string) (model.Snapshot, []string, error) {
	return e.ExtractWith(res, Options{ContentSelector: contentSelector})
}

func (extractor) ExtractWith(res fetcher.Result, opts Options) (model.Snapshot, []string, error) {
	contentSelector := opts.ContentSelector
	snap := model.Snapshot{
		FetchedAt:      res.FetchedAt,
		HTTPStatus:     res.HTTPStatus,
		ResponseTimeMS: res.ResponseTime.Milliseconds(),
		// An origin may emit X-Robots-Tag as SEPARATE header lines (one directive
		// per line), which is semantically a single comma-joined directive list.
		// Header.Get returns ONLY the first line, silently dropping a noindex on
		// line 2+; join ALL lines so every directive reaches the verdict (#1).
		XRobotsTag: strings.Join(res.Header.Values("X-Robots-Tag"), ", "),
		RawHTML:    res.Body,
	}
	if rc, err := json.Marshal(res.RedirectChain); err == nil {
		snap.RedirectChain = string(rc)
	}

	// Content-Type gate: x/net/html.Parse (via goquery) never rejects arbitrary
	// bytes, so a non-HTML 200 body (PDF/JSON/binary/WAF challenge) would parse
	// into a garbage tree and be stored as an empty-but-valid SEO snapshot — a
	// false "page is fine" signal. Reject non-HTML up front so the caller skips
	// persistence rather than recording binary noise as an indexable HTML page.
	if !isHTMLContentType(res.Header.Get("Content-Type")) {
		return snap, nil, ErrNonHTML
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(res.Body))
	if err != nil {
		// x/net/html rejects DOMs nested deeper than 512 open elements even when
		// they are well-formed and fully closed. It exposes no typed error, so we
		// match its message and wrap a sentinel the scheduler can detect.
		if strings.Contains(err.Error(), domTooDeepMarker) {
			return snap, nil, fmt.Errorf("extract: parse failed: %w", ErrDOMTooDeep)
		}
		return snap, nil, err
	}

	// Strip INERT <template>/<noscript> subtrees ONCE, before ANY field
	// extraction below. goquery (x/net/html) parses the children of <template>
	// and <noscript> into the regular element tree — unlike a browser, where a
	// <template>'s content is an inert document fragment and <noscript>'s content
	// is mere text while scripting is enabled. Left in the tree, those inert
	// nodes are descended into by collectRobotsMeta / the head-canonical read /
	// hreflang / links / images / JSON-LD / headings and fabricate false signals
	// — most dangerously a false CRITICAL deindex from a meta-robots noindex or
	// off-page canonical that never actually applies to the page. Removing them up
	// front fixes every downstream extractor from a single source of truth, mirroring
	// the precedent in maintext.go and precheck.DetectDoc. Crucially we do NOT strip
	// <script> here: the A8 hydration recovery below reads __NEXT_DATA__/__NUXT_DATA__
	// and the RSC flight <script> bodies off this same doc — and a payload that lives
	// inside a <template> is itself inert (a real browser never executes it), so
	// dropping template/noscript before hydration is strictly more correct.
	doc.Find("template, noscript").Remove()

	// A8 hydration recovery (DOM-FIRST). When enabled, decode the embedded
	// framework state payloads (__NEXT_DATA__ / __NUXT_DATA__ / __next_f flight)
	// from the document we ALREADY parsed — no second/third DOM parse, no DB query
	// (the in-memory body is the only input). The recovered fields only ever
	// BACK-FILL what the DOM left empty; the DOM head is the verifiable ground
	// truth and wins on every conflict. With hydration disabled the harvest is
	// skipped entirely and `rec` stays the zero value, so the merge below is a
	// no-op and extraction is byte-identical to pre-A8.
	var rec recovered
	if opts.Hydration.Enabled {
		rec = harvestHydration(doc, opts.Hydration.MaxPayloadBytes)
	}

	snap.Title = strings.TrimSpace(doc.Find("head title").First().Text())
	snap.MetaDescription, _ = attr(doc, `meta[name="description"]`, "content")
	// Robots directives: collect EVERY <meta name=...> whose name is "robots" or
	// the documented "googlebot" (case-insensitively — the HTML name attribute is
	// not case-sensitive, and a page may carry more than one such tag). The old
	// code read only meta[name="robots"] via .First(), so a googlebot-specific
	// directive, an uppercased ROBOTS name, or a 2nd robots tag carrying noindex
	// was silently invisible to the verdict. Joining all matches into MetaRobots
	// lets the shared robotsmeta parser (and the rules engine, which reads the same
	// field) see every directive (#3).
	snap.MetaRobots = collectRobotsMeta(doc)
	// DOM head canonical only: scope the read to <head> so a stray in-body
	// link[rel="canonical"] (not honored by search engines) cannot override the
	// head canonical or fabricate one (#4).
	snap.Canonical, _ = attr(doc, `head link[rel="canonical"]`, "href")

	// DOM-FIRST back-fill of head fields: a recovered payload value fills a head
	// field ONLY when the DOM left it empty (DOM wins on every conflict). Each
	// successful back-fill marks the contributing source so extraction_source
	// records the merge provenance. The canonical is back-filled BEFORE the
	// relative-URL resolution below so a payload-recovered relative canonical is
	// resolved against the final URL exactly like a DOM one.
	rec.fill(&snap.Title, rec.fields.Title, rec.titleSrc)
	rec.fill(&snap.MetaDescription, rec.fields.MetaDescription, rec.descSrc)
	rec.fill(&snap.Canonical, rec.fields.Canonical, rec.canonSrc)

	// Canonical source provenance: a DOM/payload canonical is "link"; a canonical
	// recovered from the HTTP Link header (below) is "header". Default to "link";
	// the header path overrides it only when it actually supplies the value.
	snap.CanonicalType = "link"
	// HTTP Link-header canonical fallback (#4): when neither the DOM head nor a
	// hydration payload supplied a canonical, honor a rel="canonical" in the HTTP
	// Link header (RFC 8288). The DOM/payload canonical is the verifiable ground
	// truth and wins on every conflict, so this only fires when snap.Canonical is
	// still empty. The value is resolved against the final URL by the shared
	// resolver below exactly like a DOM canonical.
	if snap.Canonical == "" {
		if hc := headerCanonical(res.Header); hc != "" {
			snap.Canonical = hc
			snap.CanonicalType = "header"
		}
	}

	// Resolve a relative/scheme-relative canonical against the final URL so the
	// indexability self-check (and the stored value) compares absolute against
	// absolute. A bare href="/page" on https://example.com/page is otherwise
	// mis-judged "canonicalized_away" against the absolute FinalURL.
	if snap.Canonical != "" {
		if base, berr := url.Parse(res.FinalURL); berr == nil {
			if ref, rerr := url.Parse(snap.Canonical); rerr == nil {
				snap.Canonical = base.ResolveReference(ref).String()
			}
		}
	}

	// hreflang set. Sort + dedup before marshalling so the stored value is a
	// canonical SET, not a DOM-order list: the hreflang cluster is order- and
	// multiplicity-independent (the same alternates declared in a different
	// order, or a duplicate alternate, describe the same page), so a reordered-
	// but-equal set must NOT raw-string-differ and fire a false hreflang change
	// (mirrors the set-normalization in indexability.go canonicalQuery). A genuine
	// add/remove still changes the sorted set and fires.
	var hreflangs []string
	doc.Find(`link[rel="alternate"][hreflang]`).Each(func(_ int, s *goquery.Selection) {
		if lang, ok := s.Attr("hreflang"); ok {
			hreflangs = append(hreflangs, lang)
		}
	})
	if b, err := json.Marshal(sortedSet(hreflangs)); err == nil {
		snap.Hreflang = string(b)
	}

	// Heading outline H1-H6.
	headings := map[string][]string{}
	for _, tag := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		doc.Find(tag).Each(func(_ int, s *goquery.Selection) {
			headings[tag] = append(headings[tag], strings.TrimSpace(s.Text()))
		})
	}
	if b, err := json.Marshal(headings); err == nil {
		snap.Headings = string(b)
	}

	// Link counts. urlx.Host lowercases (DNS hostnames are case-insensitive) and
	// strips userinfo/default-port so the live host compares cleanly against
	// in-page link hosts.
	host := urlx.Host(res.FinalURL)
	var links []string
	seen := map[string]struct{}{}
	base, _ := url.Parse(res.FinalURL)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") {
			return
		}
		switch classifyLink(href, host) {
		case linkExternal:
			snap.ExternalLinkCount++
		case linkInternal:
			snap.InternalLinkCount++
			if base == nil {
				return
			}
			ref, perr := url.Parse(href)
			if perr != nil {
				return
			}
			abs := base.ResolveReference(ref)
			abs.Fragment = ""
			u := abs.String()
			if _, ok := seen[u]; !ok {
				seen[u] = struct{}{}
				links = append(links, u)
			}
		case linkSkip:
			// mailto:/tel:/javascript: etc. — not counted.
		}
	})

	// Images + missing alt. An <img> counts as missing alt only when the alt
	// attribute is ABSENT. An explicit alt="" is the correct decorative-image
	// convention (it declares "this image conveys nothing, skip it" to assistive
	// tech and SEO), and a present-but-whitespace-only alt likewise declares the
	// attribute — neither is "missing". This is the A5 alt-semantics fix; it can
	// only lower a page's stored MissingAltCount, re-baselining each page on its
	// next crawl (release-notes note).
	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		snap.ImageCount++
		if _, ok := s.Attr("alt"); !ok {
			snap.MissingAltCount++
		}
	})

	// OG + Twitter.
	og := map[string]string{}
	doc.Find(`meta[property^="og:"]`).Each(func(_ int, s *goquery.Selection) {
		p, _ := s.Attr("property")
		c, _ := s.Attr("content")
		og[p] = c
	})
	if b, err := json.Marshal(og); err == nil {
		snap.OG = string(b)
	}
	tw := map[string]string{}
	doc.Find(`meta[name^="twitter:"]`).Each(func(_ int, s *goquery.Selection) {
		n, _ := s.Attr("name")
		c, _ := s.Attr("content")
		tw[n] = c
	})
	if b, err := json.Marshal(tw); err == nil {
		snap.Twitter = string(b)
	}

	// JSON-LD blocks + schema types.
	//
	// Only blocks that parse as JSON are appended to ldBlocks. Appending an
	// unparseable raw block would make json.Marshal of the []json.RawMessage
	// fail (json.RawMessage.MarshalJSON re-validates), and the `if err == nil`
	// guard below would then leave snap.JSONLD as "" — silently voiding EVERY
	// valid sibling block. Rejected blocks are counted into JSONLDInvalidCount
	// (the structured_data_invalid_json rule surfaces them; A4).
	//
	// Type collection accepts both shapes a single <script> block may carry: a
	// top-level object (incl. the @graph wrapper) and a legal top-level array
	// [{...},{...}] of entities. The array shape contributed zero schema_types
	// before this fix, so an array of typed entities looked type-less.
	var ldBlocks []json.RawMessage
	var schemaTypes []string
	doc.Find(`script[type="application/ld+json"]`).Each(func(_ int, s *goquery.Selection) {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			return
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			snap.JSONLDInvalidCount++
			return
		}
		ldBlocks = append(ldBlocks, json.RawMessage(raw))
		schemaTypes = append(schemaTypes, jsonLDBlockTypes(v)...)
	})
	// Append JSON-LD recovered from the hydration payload (RSC flight script
	// elements / Next/Nuxt payloads) into the SAME ldBlocks/schemaTypes slices
	// BEFORE they are marshalled, so recovered structured data flows into both
	// snap.JSONLD and snap.SchemaTypes via the existing path (A4 reads
	// snap.JSONLD regardless of source). hydration already validated each block as
	// JSON (json.Valid), but we re-Unmarshal to derive types and to fail-closed if
	// a block is somehow unparseable — never voiding the DOM-harvested siblings.
	for _, blk := range rec.fields.JSONLD {
		var v any
		if err := json.Unmarshal(blk, &v); err != nil {
			continue
		}
		ldBlocks = append(ldBlocks, blk)
		schemaTypes = append(schemaTypes, jsonLDBlockTypes(v)...)
		rec.mark(rec.jsonldSrc)
	}
	if b, err := json.Marshal(ldBlocks); err == nil {
		snap.JSONLD = string(b)
	}
	// Dedup + sort schema types before marshalling. They are appended in
	// traversal order (DOM blocks first, then hydration-recovered) and a type can
	// repeat across blocks, so the raw slice's order and multiplicity are
	// incidental — the SEO signal is the SET of types present. Without this a mere
	// reorder ([Product,Offer] vs [Offer,Product]) or a multiplicity flip would
	// raw-string-differ and fire a false schema_types change; a genuinely added or
	// removed type still changes the sorted set (mirrors the hreflang set above).
	if b, err := json.Marshal(sortedSet(schemaTypes)); err == nil {
		snap.SchemaTypes = string(b)
	}

	// Main text + content hash + SimHash + word count.
	//
	// CHURN-SAFE thin-DOM composition (A8 criterion 5): when the DOM main text is
	// below the thinness floor (precheck.WordFloor — the EXACT same floor the
	// classifier uses, so they can never drift) AND the payload recovered prose,
	// compose the content hash over DOM-text + payload-prose so a hydrated page
	// with a near-empty DOM body is still monitored for content changes. The
	// payload prose is volatile-key filtered inside hydration (buildId/*Hash/*Id
	// stripped) and returned in a deterministic (sorted) order, so a deploy-only
	// identifier flip on an otherwise-identical page can NOT churn the hash and
	// spam content-change alerts. When the DOM is NOT thin, the hash is DOM-only —
	// the verifiable ground truth wins, exactly as pre-A8.
	text, _ := MainText(res.FinalURL, res.Body, contentSelector)
	domWords := len(strings.Fields(text))
	if domWords < precheck.WordFloor && len(rec.fields.BodyTextCandidates) > 0 {
		text = composeText(text, rec.fields.BodyTextCandidates)
		rec.mark(rec.proseSource)
	}
	snap.WordCount = len(strings.Fields(text))
	snap.ContentSHA256 = ContentSHA256(text)
	snap.ContentSimhash = SimHash(text)

	// Classification (A8): record HOW the page delivers its SEO content by running
	// the shared precheck classifier on the document we ALREADY parsed — no second
	// DOM parse, no I/O, no DB. render_mode is classified ALWAYS (even with payload
	// recovery disabled): the classification is the honesty half of A8 and feeds the
	// needs_rendering finding, so an operator who turns recovery off still gets told
	// "this page isn't visible without JS". Only the back-fill/compose above is
	// gated on Hydration.Enabled. render_mode is a NEW snapshot column, so writing
	// it changes no pre-A8 content field — the disabled path's extracted content
	// stays byte-identical to pre-A8 (criterion 12). extraction_source records the
	// merge provenance ("dom" when nothing was recovered — always the case with
	// recovery off — else "dom+next_data" / "dom+nuxt_data" / "dom+flight").
	js := precheck.DetectDoc(doc, res.Header.Get("Content-Type"))
	snap.RenderMode = model.RenderMode(js.Kind)
	snap.ExtractionSource = rec.source()

	// Indexability verdict.
	idx, reason := Indexability(IndexabilityInput{
		HTTPStatus: res.HTTPStatus,
		MetaRobots: snap.MetaRobots,
		XRobotsTag: snap.XRobotsTag,
		Canonical:  snap.Canonical,
		FinalURL:   res.FinalURL,
	})
	snap.Indexable = idx
	snap.IndexabilityReason = reason
	return snap, links, nil
}

// Hydration source labels recorded in snapshot.extraction_source.
const (
	sourceDOM      = "dom"
	sourceNextData = "next_data"
	sourceNuxtData = "nuxt_data"
	sourceFlight   = "flight"
)

// recovered bundles the hydration fields merged across all three payload sources
// plus the provenance bookkeeping the merge needs. `fields` holds the merged head
// fields / JSON-LD / prose; `titleSrc`/`descSrc`/`canonSrc`/`proseSource` name the
// source that produced each value (so a back-fill can attribute the contribution);
// `used` is the set of source labels that actually contributed to the snapshot.
//
// The zero recovered (hydration disabled or no payload) has empty fields and an
// empty `used` set, so `source()` returns "dom" and every `fill`/`mark` call is a
// no-op — the merge collapses to byte-identical pre-A8 behavior.
type recovered struct {
	fields      hydration.Fields
	titleSrc    string
	descSrc     string
	canonSrc    string
	jsonldSrc   string
	proseSource string
	used        map[string]struct{}
}

// fill back-fills *dst from val ONLY when *dst is empty (DOM wins always). On a
// successful fill it marks src as the contributing source so extraction_source
// records the merge provenance. The caller passes the source that produced val
// directly (rec.titleSrc/descSrc/canonSrc, set per-field by harvestHydration), so
// provenance is correct-by-construction — it never depends on whether two payload
// fields happen to share an identical string value.
func (r *recovered) fill(dst *string, val, src string) {
	if *dst != "" || strings.TrimSpace(val) == "" {
		return
	}
	*dst = val
	r.mark(src)
}

// mark records that source `src` contributed to the snapshot. An empty src (no
// recovery) is ignored.
func (r *recovered) mark(src string) {
	if src == "" {
		return
	}
	if r.used == nil {
		r.used = map[string]struct{}{}
	}
	r.used[src] = struct{}{}
}

// source returns the extraction_source provenance string: "dom" when nothing was
// recovered, else "dom+<src>[+<src>...]" with contributing sources in a stable
// (next_data, nuxt_data, flight) order so the value is deterministic.
func (r *recovered) source() string {
	if len(r.used) == 0 {
		return sourceDOM
	}
	out := sourceDOM
	for _, s := range []string{sourceNextData, sourceNuxtData, sourceFlight} {
		if _, ok := r.used[s]; ok {
			out += "+" + s
		}
	}
	return out
}

// harvestHydration decodes the framework state payloads embedded in doc and
// returns the merged recovered fields with per-field provenance. It decodes from
// each source independently (so a value's source is known) and merges in a fixed
// priority order: __NEXT_DATA__, then __NUXT_DATA__, then the RSC __next_f flight.
// The first non-empty value for each head field wins; the flight contributes
// JSON-LD (the only source that recovers it today). Body-text candidates take the
// first source that produced any prose, recording its label as proseSource.
//
// maxBytes is the per-payload decode cap (crawler.hydration.max_payload_bytes): an
// over-cap payload is skipped (Truncated) by the decoders and recovers nothing.
// All decoding/bounding/volatile-key filtering lives in internal/hydration — this
// only locates the script bodies and attributes provenance.
func harvestHydration(doc *goquery.Document, maxBytes int) recovered {
	r := recovered{}

	// __NEXT_DATA__ (Next.js Pages/App Router JSON payload).
	if raw := firstScriptText(doc, "script#__NEXT_DATA__"); raw != "" {
		if f, err := hydration.FromNextData([]byte(raw), maxBytes); err == nil && f.Decoded {
			r.merge(f, sourceNextData)
		}
	}
	// __NUXT_DATA__ (Nuxt 3 devalue payload).
	if raw := firstScriptText(doc, "script#__NUXT_DATA__"); raw != "" {
		if f, err := hydration.FromNuxtData([]byte(raw), maxBytes); err == nil && f.Decoded {
			r.merge(f, sourceNuxtData)
		}
	}
	// RSC __next_f flight rows, harvested from every self.__next_f.push(...) script.
	if rows := flightRows(doc); len(rows) > 0 {
		if f, err := hydration.FromFlight(rows, maxBytes); err == nil && f.Decoded {
			r.merge(f, sourceFlight)
		}
	}
	return r
}

// merge folds a single source's recovered Fields into r, recording provenance.
// Head fields are first-source-wins (DOM-first happens later in ExtractWith; here
// we only resolve which PAYLOAD source provides a value when the DOM is empty).
// JSON-LD blocks accumulate from any source (only flight produces them today).
// Body-text candidates take the first source that yields any prose.
func (r *recovered) merge(f hydration.Fields, src string) {
	if r.fields.Title == "" && f.Title != "" {
		r.fields.Title = f.Title
		r.titleSrc = src
	}
	if r.fields.MetaDescription == "" && f.MetaDescription != "" {
		r.fields.MetaDescription = f.MetaDescription
		r.descSrc = src
	}
	if r.fields.Canonical == "" && f.Canonical != "" {
		r.fields.Canonical = f.Canonical
		r.canonSrc = src
	}
	if len(f.JSONLD) > 0 {
		r.fields.JSONLD = append(r.fields.JSONLD, f.JSONLD...)
		if r.jsonldSrc == "" {
			r.jsonldSrc = src
		}
	}
	if len(r.fields.BodyTextCandidates) == 0 && len(f.BodyTextCandidates) > 0 {
		r.fields.BodyTextCandidates = f.BodyTextCandidates
		r.proseSource = src
	}
}

// firstScriptText returns the trimmed text of the first script matching selector,
// or "" if none. Used to locate the __NEXT_DATA__ / __NUXT_DATA__ payload bodies.
func firstScriptText(doc *goquery.Document, selector string) string {
	return strings.TrimSpace(doc.Find(selector).First().Text())
}

// flightRows extracts the RSC flight row strings from every
// self.__next_f.push([id, "row"]) call in the document. It delegates the actual
// row-string parsing to precheck.FlightRows — the single source of truth shared
// with the classifier — so detection and extraction harvest the exact same rows.
func flightRows(doc *goquery.Document) []string {
	return precheck.FlightRows(doc)
}

// composeText joins the DOM main text with the recovered payload prose for the
// thin-DOM content-hash composition. The candidates arrive already volatile-key
// filtered and sorted (deterministic) from hydration, so the composed text is a
// stable function of the page's recoverable content. A leading space is harmless
// (ContentSHA256/SimHash run over the joined string).
func composeText(domText string, candidates []string) string {
	parts := make([]string, 0, len(candidates)+1)
	if strings.TrimSpace(domText) != "" {
		parts = append(parts, domText)
	}
	parts = append(parts, candidates...)
	return strings.Join(parts, " ")
}

func attr(doc *goquery.Document, selector, name string) (string, bool) {
	v, ok := doc.Find(selector).First().Attr(name)
	return strings.TrimSpace(v), ok
}

// collectRobotsMeta gathers the content of EVERY robots directive <meta> in the
// document — those whose name attribute equals "robots" or the documented
// "googlebot" (matched case-insensitively, since the HTML name attribute is not
// case-sensitive) — and joins the non-empty values into a single comma-separated
// directive list. A page may legally carry several such tags (e.g. a generic
// robots tag plus a googlebot-specific one, or two robots tags); a verdict that
// read only the first via .First() would miss a noindex on any later tag. The
// joined value flows through the shared robotsmeta parser for the indexability
// verdict and is the same field the rules engine reads, so every directive is
// honored. Returns "" when no robots/googlebot meta is present.
func collectRobotsMeta(doc *goquery.Document) string {
	var vals []string
	doc.Find("meta[name]").Each(func(_ int, s *goquery.Selection) {
		name, _ := s.Attr("name")
		name = strings.TrimSpace(name)
		if !strings.EqualFold(name, "robots") && !strings.EqualFold(name, "googlebot") {
			return
		}
		if content, ok := s.Attr("content"); ok {
			if content = strings.TrimSpace(content); content != "" {
				vals = append(vals, content)
			}
		}
	})
	return strings.Join(vals, ", ")
}

// headerCanonical returns the canonical URL declared in the HTTP Link header
// (RFC 8288 rel="canonical"), or "" if none. It scans EVERY Link header line and
// every comma-separated link-value within a line, returning the first entry
// whose rel parameter contains the "canonical" token (rel may be space-separated
// multi-relation, e.g. rel="canonical next", and is matched case-insensitively).
// The returned target is the raw <URI-Reference> from the angle brackets; the
// caller resolves it against the final URL.
func headerCanonical(h http.Header) string {
	for _, line := range h.Values("Link") {
		for _, lv := range splitLinkValues(line) {
			target, params := parseLinkValue(lv)
			if target == "" {
				continue
			}
			for _, p := range params {
				k, v, ok := strings.Cut(p, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(k), "rel") {
					continue
				}
				if relHasCanonical(v) {
					return target
				}
			}
		}
	}
	return ""
}

// splitLinkValues splits one Link header line into its individual link-values on
// the commas that separate entries, while NOT splitting on a comma inside the
// angle-bracketed <URI-Reference> (a URL may legally contain a comma). RFC 8288
// link-values are comma-separated; the URI is always wrapped in <…>.
func splitLinkValues(line string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range line {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, line[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, line[start:])
	return out
}

// parseLinkValue parses a single RFC 8288 link-value "<uri>; p1=v1; p2=v2" into
// its URI-Reference (without the angle brackets) and its raw parameter segments.
// Returns ("", nil) when the value has no angle-bracketed URI.
func parseLinkValue(lv string) (string, []string) {
	lv = strings.TrimSpace(lv)
	open := strings.IndexByte(lv, '<')
	if open < 0 {
		return "", nil
	}
	closeIdx := strings.IndexByte(lv[open:], '>')
	if closeIdx < 0 {
		return "", nil
	}
	closeIdx += open
	target := strings.TrimSpace(lv[open+1 : closeIdx])
	var params []string
	for _, p := range strings.Split(lv[closeIdx+1:], ";") {
		if p = strings.TrimSpace(p); p != "" {
			params = append(params, p)
		}
	}
	return target, params
}

// relHasCanonical reports whether a Link rel parameter value designates the
// canonical relation. The value may be quoted ("canonical") and may carry
// several space-separated relation types ("canonical next"); the match is
// token-exact and case-insensitive.
func relHasCanonical(v string) bool {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	for _, tok := range strings.Fields(v) {
		if strings.EqualFold(tok, "canonical") {
			return true
		}
	}
	return false
}

// jsonLDBlockTypes collects schema.org @type values from a decoded JSON-LD
// block, which may be either a single object (incl. the @graph wrapper) or a
// legal top-level array of entities ([{...},{...}]). Each array member that is
// an object contributes its types the same way a top-level object does.
func jsonLDBlockTypes(v any) []string {
	switch b := v.(type) {
	case map[string]any:
		return jsonLDTypes(b, jsonLDMaxDepth)
	case []any:
		var types []string
		for _, member := range b {
			if m, ok := member.(map[string]any); ok {
				types = append(types, jsonLDTypes(m, jsonLDMaxDepth)...)
			}
		}
		return types
	default:
		return nil
	}
}

// jsonLDMaxDepth bounds the @graph recursion so a maliciously or accidentally
// self-nesting payload cannot blow the stack. Real-world JSON-LD nests one or
// two @graph levels (a container whose members are entities); 32 is far beyond
// any legitimate document while still terminating a pathological one.
const jsonLDMaxDepth = 32

// jsonLDTypes collects schema.org @type values from a decoded JSON-LD object.
// schema.org/JSON-LD permits @type to be a scalar string OR an array of strings
// (multi-type, e.g. ["Article","NewsArticle"]). The common Yoast/WordPress shape
// wraps the real entities in an "@graph" with no top-level @type; @graph may be
// an ARRAY of entities OR a single entity object, and a member may itself carry a
// nested @graph — so @graph is recursed depth-bounded (depth counts down to 0).
// Each collected @type is normalized to its bare schema.org term (a full IRI like
// https://schema.org/Product or a CURIE like schema:Product becomes "Product").
// Returns nil if no types are present.
func jsonLDTypes(obj map[string]any, depth int) []string {
	var types []string
	types = appendTypeValue(types, obj["@type"])
	if depth <= 0 {
		return types
	}
	switch graph := obj["@graph"].(type) {
	case []any:
		for _, member := range graph {
			if m, ok := member.(map[string]any); ok {
				types = append(types, jsonLDTypes(m, depth-1)...)
			}
		}
	case map[string]any:
		// @graph may legally be a single object rather than an array.
		types = append(types, jsonLDTypes(graph, depth-1)...)
	}
	return types
}

// appendTypeValue appends a JSON-LD @type value to dst, normalizing each value to
// its bare schema.org term. The value may be a single string or an array of
// strings (each non-empty member is collected).
func appendTypeValue(dst []string, v any) []string {
	switch t := v.(type) {
	case string:
		if s := bareSchemaType(t); s != "" {
			dst = append(dst, s)
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				if s = bareSchemaType(s); s != "" {
					dst = append(dst, s)
				}
			}
		}
	}
	return dst
}

// bareSchemaType normalizes a JSON-LD @type token to its bare term so the same
// type written three ways collapses to one stored value (otherwise a markup
// rewrite from "Product" to "https://schema.org/Product" would fire a false
// schema_types change, and a rich-result match keyed on the bare term would
// miss the IRI/CURIE forms). It strips a full schema.org IRI
// (http(s)://schema.org/Foo, with or without a trailing "/" or "#" fragment
// separator) and a CURIE prefix (schema:Foo / schemaorg:Foo). A token that is
// neither is returned trimmed and otherwise unchanged. Kept consistent with the
// rich-result type matching landing in parallel.
func bareSchemaType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Full IRI: http://schema.org/Foo or https://schema.org/Foo (also tolerate a
	// "schema.org/Foo" with no scheme). The term is the last path/fragment segment.
	lower := strings.ToLower(t)
	for _, p := range []string{"http://schema.org/", "https://schema.org/", "schema.org/"} {
		if strings.HasPrefix(lower, p) {
			term := t[len(p):]
			// Tolerate a trailing slash (".../Product/") so it is not itself taken as
			// the final segment below (which would silently drop the type).
			term = strings.TrimRight(term, "/")
			// A fragment form (schema.org#Foo) is rare but possible after the host.
			if i := strings.LastIndexAny(term, "/#"); i >= 0 {
				term = term[i+1:]
			}
			return strings.TrimSpace(term)
		}
	}
	// CURIE: a "prefix:Term" where the prefix names the schema.org vocabulary.
	// Only strip a known schema-vocabulary prefix — an arbitrary "foo:Bar" is
	// left intact (it is a different vocabulary, not a schema.org type alias).
	if i := strings.IndexByte(t, ':'); i > 0 {
		switch strings.ToLower(t[:i]) {
		case "schema", "schemaorg":
			return strings.TrimSpace(t[i+1:])
		}
	}
	return t
}

// sortedSet returns the unique non-empty members of in, sorted. It turns a
// DOM-/traversal-order slice into a canonical SET so a stored JSON array is a
// stable function of the page's content: a reordered-but-equal set (or a
// duplicate member) no longer raw-string-differs and fires a false diff change,
// while a genuine add/remove still changes the result. A nil/empty input yields
// nil (which json.Marshal renders as "null", exactly as before — empty pages are
// unchanged). Mirrors the set-normalization in indexability.go canonicalQuery.
func sortedSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// isHTMLContentType reports whether a Content-Type header value designates HTML
// (or XHTML). An empty/absent header is treated as HTML (permissive): many
// servers omit Content-Type on HTML, and the upstream FetchOK classifier already
// vetted the body — the gate only exists to reject explicitly non-HTML types
// (application/json, application/pdf, image/*, …). A malformed value is treated
// as HTML rather than dropping a possibly-valid page.
func isHTMLContentType(ct string) bool {
	if strings.TrimSpace(ct) == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

// linkKind classifies an href relative to the page host for link counting.
type linkKind int

const (
	linkInternal linkKind = iota
	linkExternal
	linkSkip // mailto:/tel:/javascript: and other non-navigational schemes
)

func classifyLink(href, host string) linkKind {
	// Protocol-relative URL (//cdn.example.com/x): scheme inherited, host follows.
	if strings.HasPrefix(href, "//") {
		if urlx.Host("http:"+href) == host {
			return linkInternal
		}
		return linkExternal
	}
	// Root- or path-relative => same host => internal.
	if strings.HasPrefix(href, "/") {
		return linkInternal
	}
	lower := strings.ToLower(href)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if urlx.Host(href) != host {
			return linkExternal
		}
		return linkInternal
	}
	// Non-http schemes (mailto:, tel:, javascript:, data:, etc.) are not links
	// to a page and must not inflate internal/external counts.
	if i := strings.Index(lower, ":"); i > 0 {
		scheme := lower[:i]
		if isURLScheme(scheme) {
			return linkSkip
		}
	}
	// Bare relative path (e.g. "about", "page.html") => same host => internal.
	return linkInternal
}

// isURLScheme reports whether s looks like a URL scheme (letters/digits/+-.),
// distinguishing "mailto" in "mailto:a@b.com" from a stray ':' in a relative path.
func isURLScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9', r == '+', r == '-', r == '.':
			if i == 0 {
				return false // scheme must start with a letter
			}
		default:
			return false
		}
	}
	return true
}
