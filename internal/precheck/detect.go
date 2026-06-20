// Package precheck performs an honest, pure-Go pre-flight for a URL: it reuses the
// existing fetcher.Doctor preflight (robots/egress/blocked/UA) and adds a calibrated
// HINT about whether a page's SEO content is visible to Rabbot without executing
// JavaScript. The JS hint is never a definitive verdict — every signal that drove a
// call is surfaced so the result is auditable, and the user is always told how to
// confirm (compare the browser's View Source with the rendered DOM). The package is
// designed to be callable programmatically (Run takes a context.Context + Options and
// returns a Report) so a later TUI wizard can reuse it without any cobra dependency.
package precheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/roberto-grasiano/rabbot-seo/internal/hydration"
)

// RenderKind is the calibrated HINT for how a page delivers its SEO content. It is
// never a definitive verdict — see JSDependency.Confidence and JSDependency.Signals.
type RenderKind string

const (
	// ServerRendered means the core SEO content/head is present in the initial HTML.
	ServerRendered RenderKind = "server_rendered"
	// Hydrated means a framework hydration payload (__NEXT_DATA__/__NUXT_DATA__/…) is
	// present, so content is recoverable WITHOUT JS even if a root div looks thin.
	Hydrated RenderKind = "hydrated"
	// HeadOnlyShell means the SEO head (title/meta/headings) is in the server HTML but the
	// body is an empty framework root with little prose and no hydration payload: the head
	// is monitorable, but the body content/links are likely client-rendered and not
	// recoverable without JavaScript. It is the honest "partial" between ServerRendered and
	// ClientShell, and grades the overall verdict YELLOW.
	HeadOnlyShell RenderKind = "head_only_shell"
	// ClientShell means an empty framework root plus very low visible words plus large
	// script bytes plus no hydration payload plus missing head fields — content likely
	// needs JavaScript. This is the only "needs JS" call, and it is always low confidence.
	ClientShell RenderKind = "client_shell"
	// Unknown means the signals are mixed or insufficient (non-HTML, blocked, empty body,
	// or a thin page that matches no confident pattern).
	Unknown RenderKind = "unknown"
)

// Confidence grades how strongly the signals support the RenderKind. It is always
// low for any "needs JS" (ClientShell) call — detection from raw HTML is a hint,
// not proof (adversarial verification refuted "reliably detectable").
type Confidence string

const (
	// ConfidenceLow marks a weakly-supported or inherently-uncertain read (always
	// used for ClientShell).
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium marks a moderately-supported read.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh marks a strongly-supported read (a parsed hydration payload or
	// substantial server-rendered head/content).
	ConfidenceHigh Confidence = "high"
)

// Tunable detector thresholds. These are EMPIRICALLY UN-TUNED starting points
// (research note §168): JS-need detection from raw HTML is a calibrated hint, not a
// measured constant, so these values are deliberately conservative and may be
// adjusted as real-world calibration data accumulates.
const (
	// WordFloor is the visible-word count below which a page is considered "thin".
	// At or above it the page reads as server-rendered; below it (combined with an
	// empty framework root, large scripts, and no head/payload) it hints ClientShell.
	// It is exported so the crawl-time extractor's payload-prose composition shares
	// the EXACT same floor (extract triggers payload-prose recovery only when the
	// DOM main text falls below WordFloor) — the threshold can never drift between
	// the classifier and the extractor.
	WordFloor = 25
	// wordFloor is the unexported alias kept for the dense in-package references
	// below; it mirrors WordFloor so the existing detector arithmetic reads cleanly.
	wordFloor = WordFloor
	// scriptCeil is the inline-script byte total above which the script-to-prose ratio
	// is considered heavy enough to support (never alone) a ClientShell hint.
	scriptCeil = 4096
)

// Signal is one named observation that drove the JSDependency call. Every evaluated
// signal — fired or not — is surfaced so the verdict is auditable, never a black box.
type Signal struct {
	// Name is a stable identifier, e.g. "next_data_payload" or "empty_framework_root".
	Name string
	// Present reports whether the signal fired.
	Present bool
	// Detail is human-readable specifics, e.g. "root div #__next had 0 words".
	Detail string
}

// JSDependency is the pure detector result for one HTML document.
type JSDependency struct {
	// Kind is the calibrated render-mode hint.
	Kind RenderKind
	// ContentVisibleToCrawler reports whether the main SEO content (body text/links, not
	// just the static head) is recoverable without JavaScript. True for ServerRendered and
	// Hydrated; false for HeadOnlyShell (head present but body is a client shell) and
	// ClientShell.
	ContentVisibleToCrawler bool
	// Confidence grades how strongly the signals support Kind.
	Confidence Confidence
	// HydrationPayload reports whether a parseable/known hydration payload was found.
	HydrationPayload bool
	// Framework is a best-guess fingerprint: "next", "nuxt", "react", "angular", "vue", or "".
	Framework string
	// VisibleWordCount is the number of whitespace-separated tokens in the body text.
	VisibleWordCount int
	// ScriptBytes is the summed length of all inline <script> text.
	ScriptBytes int
	// Signals lists every signal evaluated (present and absent), for honest display.
	Signals []Signal
	// Summary is a one-line honest phrasing ("appears/likely", never certainty).
	Summary string
	// Advice tells the user how to confirm (compare View Source with the rendered DOM).
	Advice string
}

// Detect inspects raw HTML in memory and returns a calibrated HINT about whether the
// page's SEO content is visible without executing JavaScript. It performs no I/O.
// contentType gates HTML parsing: a non-HTML or empty body yields Kind=Unknown. The
// result is never definitive — the KEY RULE applies: a hydration payload that RECOVERS at
// least one SEO field OR substantial server-rendered head/content yields
// ContentVisibleToCrawler=true and a Hydrated/ServerRendered Kind, never ClientShell, even
// when a framework root looks empty. A payload that decodes but recovers nothing does not
// qualify — the verdict is graded on fields actually recovered, not on payload structure.
//
// Detect owns only the content-type/body gate and the single DOM parse; the
// classification itself lives in DetectDoc, so a caller that already parsed the
// document (the crawl-time extractor) can reuse the exact same classifier WITHOUT a
// second/third DOM parse by calling DetectDoc directly.
func Detect(rawHTML []byte, contentType string) JSDependency {
	// Gate on content type + body presence first: non-HTML or an empty body (the
	// fetcher only returns a body for ok-class fetches) cannot be read as a page.
	if len(bytes.TrimSpace(rawHTML)) == 0 || !isHTMLContentType(contentType) {
		js := JSDependency{
			Kind:       Unknown,
			Confidence: ConfidenceLow,
			Signals: []Signal{{
				Name:    "html_parseable",
				Present: false,
				Detail:  "empty body or non-HTML content type — nothing to inspect",
			}},
		}
		applyMessages(&js)
		return js
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawHTML))
	if err != nil {
		js := JSDependency{
			Kind:       Unknown,
			Confidence: ConfidenceLow,
			Signals: []Signal{{
				Name:    "html_parseable",
				Present: false,
				Detail:  "HTML failed to parse",
			}},
		}
		applyMessages(&js)
		return js
	}

	return DetectDoc(doc, contentType)
}

// DetectDoc is the doc-level classification core: it runs every render-mode signal
// over an ALREADY-PARSED document and returns the calibrated JSDependency hint. It is
// the single source of truth for the verdict logic; Detect is a thin wrapper that
// parses raw HTML and calls it. The crawl-time extractor calls DetectDoc on the
// document it already parsed, so classification never costs a second DOM parse.
//
// doc must be non-nil and already parsed (Detect's content-type/body gate handles the
// empty/non-HTML cases before this is reached). contentType is accepted for symmetry
// with Detect and to keep the signature stable for callers that classify from a raw
// fetch (the gate has already run by the time DetectDoc is called from Detect).
//
// Payload presence/decode is delegated entirely to internal/hydration — the single
// source of truth for __NEXT_DATA__ / __NUXT_DATA__ / __next_f decoding — so detection
// and crawl-time extraction share one decoder and can never disagree about whether a
// payload is recoverable.
func DetectDoc(doc *goquery.Document, contentType string) JSDependency {
	_ = contentType // gate already applied by Detect; accepted for signature symmetry.
	js := JSDependency{Kind: Unknown, Confidence: ConfidenceLow}

	// ── Counters ────────────────────────────────────────────────────────────
	// Visible word count must exclude script/style/noscript text: goquery's .Text()
	// concatenates descendant text including inline <script> bodies, which would
	// otherwise count a heavy JS bundle as "visible words" and mask a client shell.
	js.VisibleWordCount = len(strings.Fields(visibleBodyText(doc)))
	// ScriptBytes proxies executable BUNDLE weight, so it excludes recoverable-data
	// payloads (__NEXT_DATA__/__NUXT_DATA__/JSON-LD): those carry SEO content that is
	// recoverable WITHOUT JS, so counting them would conflate data with bundle bytes and
	// inflate the needs-JS proxy. Only true inline executable scripts are summed.
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if isRecoverablePayloadScript(s) {
			return
		}
		js.ScriptBytes += len(s.Text())
	})

	// ── Head fields (what the extractor already harvests) ───────────────────
	title := strings.TrimSpace(doc.Find("head title").First().Text())
	desc, _ := doc.Find(`meta[name="description"]`).First().Attr("content")
	desc = strings.TrimSpace(desc)
	hasH1 := doc.Find("h1").Length() > 0
	hasHeadFields := title != "" || desc != "" || hasH1

	// ── Hydration payloads (DECISIVE recoverability override) ────────────────
	// Payload presence/decode is delegated to internal/hydration — the single source of
	// truth for __NEXT_DATA__ / __NUXT_DATA__ / __next_f decoding — so detection and the
	// crawl-time extractor share one decoder and can never disagree. We pass no byte cap
	// (0): the doctor path runs once per URL, not on the crawl hot path, and the decoders
	// are still bounded by their internal depth/node budgets regardless of the byte cap.
	//
	// honesty: hydration's Decoded is true ONLY for a real, non-degenerate payload (a
	// scalar / empty object / empty array yields Decoded=false), which exactly preserves
	// the previous "only a non-empty object or array is a payload" rule for __NEXT_DATA__.
	// LOCKED DECISION — grade on ACTUAL field recovery, not payload structure: a
	// payload that decodes into a real, non-degenerate structure but recovers ZERO SEO
	// fields (empty title/meta/canonical, no JSON-LD, no prose) carries nothing the
	// extractor could back-fill, so it must NOT be claimed as a recoverable payload. An
	// app-specific __NEXT_DATA__ with no allow-listed head keys and no harvestable prose
	// is the canonical case: it decodes, but recovers nothing, so it does not grade
	// Hydrated/HIGH/recoverable. Each probe therefore reports whether hydration RECOVERED
	// a field, not merely that decoding succeeded.
	nextData := recoversNextData(doc)
	// Nuxt: decode the devalue payload through hydration too (a real decode AND a
	// recovered field). An empty/degenerate tag yields Decoded=false, and a non-degenerate
	// devalue blob that yields no head/prose recovers nothing — neither is claimed as a
	// payload, preserving both the empty-tag honesty rule and the field-recovery gate.
	nuxtData := recoversNuxtData(doc)
	// Legacy Nuxt global: window.__NUXT__ has no structured decoder, so it recovers no
	// SEO field — it is an INFORMATIONAL framework fingerprint only (it feeds
	// guessFramework and is surfaced as an auditable signal), never a recoverability
	// claim. Under the field-recovery gate a presence-only marker must not grade
	// Hydrated/recoverable, so it is deliberately kept OUT of the HydrationPayload set.
	nuxtGlobal := scriptContains(doc, "window.__NUXT__")
	// RSC App Router flight: detect presence for the signal, and DECODE the streamed
	// rows through hydration.FromFlight. A flight that decodes into recoverable elements
	// (a title/meta/link/script tuple or prose) folds into the KEY RULE below
	// (HydrationPayload ⇒ Hydrated); a present-but-undecodable blob — or a decoded blob
	// that recovered nothing — stays out of HydrationPayload and routes to Unknown
	// (honest "present but unreadable/empty").
	nextFlightPresent := scriptContains(doc, "self.__next_f")
	nextFlightDecoded := nextFlightPresent && recoversFlight(doc)

	// ── Framework fingerprints (INFORMATIONAL — must not force needs-JS) ─────
	reactFingerprint := doc.Find("[data-reactroot]").Length() > 0
	vueSSRMarker := doc.Find(`[data-server-rendered="true"]`).Length() > 0
	angularVersion := doc.Find("[ng-version]").Length() > 0

	// ── Empty framework root ─────────────────────────────────────────────────
	emptyRoot, emptyRootDetail := detectEmptyFrameworkRoot(doc)

	js.Framework = guessFramework(nextData, nextFlightPresent, nuxtData, nuxtGlobal, reactFingerprint, vueSSRMarker, angularVersion)
	// A DECODED RSC flight that recovered a field is a real, recoverable hydration payload
	// (the KEY RULE applies to RSC — acceptance #10); an undecodable or empty flight stays
	// out of the payload set. nextData/nuxtData are likewise true only when a field was
	// recovered (the field-recovery gate), so HydrationPayload reflects actual recovery.
	// nuxtGlobal is intentionally excluded: it is a presence-only fingerprint with no
	// decoder, so it recovers nothing and must not drive a recoverability claim.
	js.HydrationPayload = nextData || nuxtData || nextFlightDecoded

	// ── Record every evaluated signal (auditable, deterministic order) ───────
	js.Signals = []Signal{
		{Name: "next_data_payload", Present: nextData, Detail: payloadDetail(nextData, "script#__NEXT_DATA__ decoded and recovered at least one SEO field")},
		{Name: "nuxt_data_payload", Present: nuxtData, Detail: payloadDetail(nuxtData, "script#__NUXT_DATA__ decoded (devalue) and recovered at least one SEO field")},
		{Name: "nuxt_global", Present: nuxtGlobal, Detail: payloadDetail(nuxtGlobal, "window.__NUXT__ present (legacy global; presence only)")},
		{Name: "next_rsc_flight", Present: nextFlightPresent, Detail: rscFlightDetail(nextFlightPresent, nextFlightDecoded)},
		{Name: "empty_framework_root", Present: emptyRoot, Detail: emptyRootDetail},
		{Name: "react_fingerprint", Present: reactFingerprint, Detail: fingerprintDetail(reactFingerprint, "data-reactroot")},
		{Name: "vue_ssr_marker", Present: vueSSRMarker, Detail: fingerprintDetail(vueSSRMarker, `data-server-rendered="true"`)},
		{Name: "angular_version", Present: angularVersion, Detail: fingerprintDetail(angularVersion, "ng-version")},
		{Name: "low_visible_words", Present: js.VisibleWordCount < wordFloor, Detail: wordsDetail(js.VisibleWordCount)},
		{Name: "large_script_bytes", Present: js.ScriptBytes > scriptCeil, Detail: scriptsDetail(js.ScriptBytes)},
		{Name: "missing_head_fields", Present: !hasHeadFields, Detail: headDetail(title, desc, hasH1)},
	}

	// ── Verdict (KEY RULE first) ─────────────────────────────────────────────
	switch {
	case js.HydrationPayload:
		// KEY RULE: a hydration payload that RECOVERED at least one SEO field means content
		// is recoverable without JS (graded on actual recovery, not payload structure — a
		// decoded-but-empty payload never reaches here). A parsed __NEXT_DATA__ that
		// recovered a field earns high confidence; a Nuxt devalue payload or an RSC
		// __next_f flight that recovered a field is graded medium — the recovery is real,
		// but the proprietary wire formats keep us from over-claiming the high confidence
		// reserved for the standardized __NEXT_DATA__ JSON.
		js.Kind = Hydrated
		js.ContentVisibleToCrawler = true
		if nextData {
			js.Confidence = ConfidenceHigh
		} else {
			js.Confidence = ConfidenceMedium
		}
	case hasHeadFields && emptyRoot && js.VisibleWordCount < wordFloor && !nextFlightPresent:
		// Head-rendered, body client-shell: the SEO <head> (title/meta/headings) is in the
		// server HTML, but the framework root is empty, there is little body prose, and no
		// hydration payload/RSC stream was found to recover the body. The head is
		// monitorable; the body content/links are likely JS-rendered and not visible — an
		// honest "partial", neither a reassuring ServerRendered nor a full needs-JS
		// ClientShell. Still a hint, so confidence stays low.
		js.Kind = HeadOnlyShell
		js.ContentVisibleToCrawler = false
		js.Confidence = ConfidenceLow
	case hasHeadFields || js.VisibleWordCount >= wordFloor:
		// Server-rendered: the head fields / prose the extractor reads are present.
		js.Kind = ServerRendered
		js.ContentVisibleToCrawler = true
		if hasHeadFields && js.VisibleWordCount >= wordFloor {
			js.Confidence = ConfidenceHigh
		} else {
			js.Confidence = ConfidenceMedium
		}
	case nextFlightPresent:
		// RECOVERABILITY OVERRIDE (undecodable flight): an App Router page that streams
		// RSC rows (self.__next_f) carries server-streamed content and ships its SEO
		// <head> in the raw HTML (research note §89/§158), so a present RSC stream blocks
		// the needs-JS (ClientShell) call. A flight that DECODED into recoverable elements
		// already folded into the KEY RULE above (HydrationPayload ⇒ Hydrated). What
		// reaches here is a present-but-UNDECODABLE blob: it stays OUT of HydrationPayload
		// and routes to Unknown at low confidence (an honest "present but unreadable", not
		// a "needs JS" claim and never an over-claimed Hydrated for a stream we can't read).
		js.Kind = Unknown
		js.ContentVisibleToCrawler = false
		js.Confidence = ConfidenceLow
	case emptyRoot && js.VisibleWordCount < wordFloor && js.ScriptBytes > scriptCeil && !hasHeadFields:
		// Confident-but-low client shell: empty root + thin text + heavy scripts +
		// no head/payload/RSC stream. Still only a HINT, so confidence stays low.
		js.Kind = ClientShell
		js.ContentVisibleToCrawler = false
		js.Confidence = ConfidenceLow
	default:
		// Mixed/insufficient signals.
		js.Kind = Unknown
		js.ContentVisibleToCrawler = false
		js.Confidence = ConfidenceLow
	}

	applyMessages(&js)
	return js
}

// visibleBodyText returns the page's body text with script/style/noscript content
// removed, so a heavy inline JS bundle is not miscounted as visible prose. It clones
// the body selection before mutating it, leaving the parsed document untouched.
func visibleBodyText(doc *goquery.Document) string {
	body := doc.Find("body").Clone()
	body.Find("script, style, noscript, template").Remove()
	return body.Text()
}

// detectEmptyFrameworkRoot reports whether a known framework root element exists but
// holds negligible inner content. It is necessary-but-not-sufficient for ClientShell.
// Script/style text inside the root is excluded so an inline bundle does not read as
// "filled" content.
func detectEmptyFrameworkRoot(doc *goquery.Document) (bool, string) {
	rootSelectors := []string{"#root", "#app", "#__next", "#__nuxt"}
	for _, sel := range rootSelectors {
		el := doc.Find(sel).First()
		if el.Length() == 0 {
			continue
		}
		inner := el.Clone()
		inner.Find("script, style, noscript, template").Remove()
		words := len(strings.Fields(inner.Text()))
		if words == 0 {
			return true, "framework root " + sel + " present with 0 visible words"
		}
		return false, "framework root " + sel + " present with content"
	}
	return false, "no known framework root element (#root/#app/#__next/#__nuxt)"
}

// hydrationCap is the byte cap precheck passes to the hydration decoders. The doctor
// path runs once per URL (never on the crawl hot path), so it disables the byte cap
// (0 = no cap) and relies on the decoders' internal depth/node budgets to bound work.
// Detection only needs the Decoded marker, not the recovered fields, so an over-cap
// truncation (Truncated=true, Decoded=false) would merely read as "not a payload" —
// uncapping here keeps a large-but-legitimate payload from being mis-classified as
// undecoded in the rare case it exceeds a cap.
const hydrationCap = 0

// recoversAnyField reports whether a decoded hydration payload actually carries a
// recoverable SEO field — a title, meta description, canonical, any JSON-LD block, or any
// harvested body-prose candidate. It is the LOCKED-DECISION gate: a payload that decoded
// into a real structure but recovered NONE of these (an app-specific __NEXT_DATA__ with no
// allow-listed head keys and no harvestable prose) carries nothing the extractor could
// back-fill, so it must not be claimed as a recoverable hydration payload. Grading is on
// what was ACTUALLY recovered, never on payload structure alone.
func recoversAnyField(f hydration.Fields) bool {
	return f.Title != "" ||
		f.MetaDescription != "" ||
		f.Canonical != "" ||
		len(f.JSONLD) > 0 ||
		len(f.BodyTextCandidates) > 0
}

// recoversNextData reports whether the page carries a __NEXT_DATA__ script that decodes
// into a real, non-degenerate payload AND recovers at least one SEO field. Decoding is
// delegated to hydration.FromNextData (the single source of truth); a missing tag, empty
// tag, malformed JSON, or a degenerate value (scalar / empty object / empty array) all
// yield false (preserving the "only a non-empty object/array is a payload" honesty), and
// a decoded-but-empty payload that recovers no field also yields false (the field-recovery
// gate — grade on what was actually recovered, not on structure).
func recoversNextData(doc *goquery.Document) bool {
	recovered := false
	doc.Find("script#__NEXT_DATA__").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			return true
		}
		f, err := hydration.FromNextData([]byte(raw), hydrationCap)
		if err == nil && f.Decoded && recoversAnyField(f) {
			recovered = true
			return false // a real, field-bearing payload; stop
		}
		return true
	})
	return recovered
}

// recoversNuxtData reports whether the page carries a __NUXT_DATA__ script that decodes
// (devalue) into a real, non-degenerate payload AND recovers at least one SEO field.
// Decoding is delegated to hydration.FromNuxtData. An empty tag, a non-array/malformed
// payload (which FromNuxtData reports as an error), a degenerate value, or a decoded blob
// that recovered no field all yield false — so a presence-only/empty tag (empty-tag
// honesty rule) and a structural-only devalue blob (field-recovery gate) are never
// claimed as recoverable.
func recoversNuxtData(doc *goquery.Document) bool {
	recovered := false
	doc.Find("script#__NUXT_DATA__").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			return true
		}
		f, err := hydration.FromNuxtData([]byte(raw), hydrationCap)
		if err == nil && f.Decoded && recoversAnyField(f) {
			recovered = true
			return false
		}
		return true
	})
	return recovered
}

// recoversFlight reports whether the page's RSC __next_f stream decodes into recoverable
// elements/prose AND yields at least one SEO field. It harvests the pushed flight rows and
// delegates decoding to hydration.FromFlight (the single source of truth). A present-but-
// undecodable stream, or a decoded stream that recovered no field, yields false — so it
// never folds into the KEY RULE and stays an honest "present but unreadable/empty" routed
// to Unknown by the verdict switch.
func recoversFlight(doc *goquery.Document) bool {
	rows := harvestFlightRows(doc)
	if len(rows) == 0 {
		return false
	}
	f, err := hydration.FromFlight(rows, hydrationCap)
	return err == nil && f.Decoded && recoversAnyField(f)
}

// FlightRows extracts the RSC __next_f flight row strings from every
// self.__next_f.push([id, "row"]) call in doc's scripts. It is the EXPORTED seam
// the crawl-time extractor uses so detection and extraction harvest the EXACT same
// rows (one source of truth for what a flight stream contains); it delegates to the
// internal harvester. The returned strings are the second (string) element of each
// pushed tuple, ready to hand to hydration.FromFlight.
func FlightRows(doc *goquery.Document) []string {
	return harvestFlightRows(doc)
}

// harvestFlightRows extracts the RSC flight row strings from every
// self.__next_f.push([id, "row"]) call in the document's scripts. hydration.FromFlight
// takes the already-extracted row strings (the second, string element of each pushed
// tuple); this pulls those strings out of the script bodies. Rows that are not a quoted
// string (e.g. self.__next_f=self.__next_f||[] bootstrap, or a push of a non-string)
// are skipped. Decoding/validation of each row is hydration's job, not this harvester's.
func harvestFlightRows(doc *goquery.Document) []string {
	var rows []string
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		text := s.Text()
		if !strings.Contains(text, "self.__next_f") {
			return
		}
		rows = append(rows, extractPushStrings(text)...)
	})
	return rows
}

// extractPushStrings scans script source for self.__next_f.push(...) calls and returns
// the JSON string argument of each push (the flight row). It walks the text looking for
// the "push(" marker, then reads the first JSON string literal after it (honoring \"
// and \\ escapes) and JSON-decodes it to the row's true contents. This is a deliberately
// small, allocation-bounded scan: it never executes script and treats anything it cannot
// read as "no row" rather than failing.
func extractPushStrings(src string) []string {
	var out []string
	const marker = "push("
	for i := 0; i < len(src); {
		idx := strings.Index(src[i:], marker)
		if idx < 0 {
			break
		}
		j := i + idx + len(marker)
		// Find the first JSON string literal opening quote after push(, scanning past the
		// numeric id and comma (e.g. push([1,"row"]) or push("row")).
		q := strings.IndexByte(src[j:], '"')
		if q < 0 {
			break
		}
		start := j + q
		row, end, ok := readJSONString(src, start)
		if ok && row != "" {
			out = append(out, row)
		}
		if end <= i {
			// No forward progress (defensive): advance past the marker to avoid a loop.
			i = j
			continue
		}
		i = end
	}
	return out
}

// readJSONString reads a JSON string literal beginning at src[start]=='"' and returns
// the decoded contents, the index just past the closing quote, and whether a complete
// literal was read. It honors \" and \\ escapes so an embedded quote does not end the
// string early, and decodes the literal with encoding/json so escapes resolve exactly
// as the framework wrote them. An unterminated literal returns ok=false.
func readJSONString(src string, start int) (string, int, bool) {
	if start >= len(src) || src[start] != '"' {
		return "", start, false
	}
	i := start + 1
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2 // skip the escaped char (\" or \\ or \n etc.)
			continue
		case '"':
			lit := src[start : i+1]
			var decoded string
			if err := json.Unmarshal([]byte(lit), &decoded); err != nil {
				return "", i + 1, false
			}
			return decoded, i + 1, true
		default:
			i++
		}
	}
	return "", len(src), false
}

// rscFlightDetail describes the next_rsc_flight signal honestly given whether the
// stream was present and whether it decoded.
func rscFlightDetail(present, decoded bool) string {
	if !present {
		return "not present"
	}
	if decoded {
		return "self.__next_f streaming present, decoded, and recovered SEO fields (App Router RSC)"
	}
	return "self.__next_f streaming present but no SEO fields recovered here (App Router RSC; undecodable or empty stream)"
}

// isRecoverablePayloadScript reports whether a <script> is a recoverable-data payload
// (a hydration payload or JSON-LD) rather than executable bundle code. Such payloads
// carry SEO content recoverable WITHOUT JS, so they are excluded from the ScriptBytes
// bundle-weight proxy.
func isRecoverablePayloadScript(s *goquery.Selection) bool {
	if id, ok := s.Attr("id"); ok {
		switch id {
		case "__NEXT_DATA__", "__NUXT_DATA__":
			return true
		}
	}
	if t, ok := s.Attr("type"); ok {
		if strings.EqualFold(strings.TrimSpace(t), "application/ld+json") {
			return true
		}
	}
	return false
}

// scriptContains reports whether any <script> element's text contains substr.
func scriptContains(doc *goquery.Document, substr string) bool {
	found := false
	doc.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if strings.Contains(s.Text(), substr) {
			found = true
			return false
		}
		return true
	})
	return found
}

// guessFramework returns a best-guess framework fingerprint from the detected
// signals, preferring the most specific evidence. It is informational only.
func guessFramework(nextData, nextFlight, nuxtData, nuxtGlobal, react, vue, angular bool) string {
	switch {
	case nextData || nextFlight:
		return "next"
	case nuxtData || nuxtGlobal:
		return "nuxt"
	case angular:
		return "angular"
	case vue:
		return "vue"
	case react:
		return "react"
	default:
		return ""
	}
}

// isHTMLContentType reports whether ct designates HTML (or XHTML). An empty header is
// treated as HTML (permissive — many servers omit it). It mirrors extract.isHTMLContentType
// but is kept local to avoid a precheck->extract dependency (one-way dep cleanliness).
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

func payloadDetail(present bool, when string) string {
	if present {
		return when
	}
	return "not present"
}

func fingerprintDetail(present bool, attr string) string {
	if present {
		return attr + " attribute present (framework id only — not a needs-JS signal)"
	}
	return attr + " not present"
}

func wordsDetail(n int) string {
	if n < wordFloor {
		return fmt.Sprintf("body has %d visible words (below wordFloor=%d)", n, wordFloor)
	}
	return fmt.Sprintf("body has %d visible words (>= wordFloor=%d)", n, wordFloor)
}

func scriptsDetail(n int) string {
	if n > scriptCeil {
		return fmt.Sprintf("inline scripts total %d bytes (above scriptCeil=%d)", n, scriptCeil)
	}
	return fmt.Sprintf("inline scripts total %d bytes (<= scriptCeil=%d)", n, scriptCeil)
}

func headDetail(title, desc string, hasH1 bool) string {
	present := []string{}
	if title != "" {
		present = append(present, "title")
	}
	if desc != "" {
		present = append(present, "description")
	}
	if hasH1 {
		present = append(present, "h1")
	}
	if len(present) == 0 {
		return "no title/description/h1 in the server HTML"
	}
	return "server HTML carries head fields: " + strings.Join(present, ", ")
}
