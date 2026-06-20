package precheck

import (
	"strings"
	"testing"
)

// signalByName returns the Signal with the given name and whether it was found,
// so tests can assert on a specific evaluated signal without index coupling.
func signalByName(sigs []Signal, name string) (Signal, bool) {
	for _, s := range sigs {
		if s.Name == name {
			return s, true
		}
	}
	return Signal{}, false
}

func TestDetect(t *testing.T) {
	const ssrHTML = `<html><head><title>Real Page Title</title>` +
		`<meta name="description" content="a real description"></head>` +
		`<body><h1>Welcome</h1>` +
		`<p>This is a genuine server-rendered article with plenty of real ` +
		`words so the visible word count is comfortably above the floor and ` +
		`the page reads as server rendered to the detector heuristics.</p>` +
		`</body></html>`

	// Next.js Pages Router: empty #__next root but a valid __NEXT_DATA__ payload.
	const nextHTML = `<html><head></head><body>` +
		`<div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"title":"Hydrated Title"}},"page":"/"}` +
		`</script>` +
		`<script src="/_next/static/chunks/main.js"></script>` +
		`</body></html>`

	// Nuxt: __NUXT_DATA__ present (devalue-encoded) carrying a recoverable head field —
	// the devalue root {"title":1} references index 1, the title string, so FromNuxtData
	// recovers Title. A payload that recovers a field is the only one that grades Hydrated
	// under the field-recovery gate (a structural-only blob recovers nothing → not Hydrated).
	const nuxtHTML = `<html><head></head><body>` +
		`<div id="__nuxt"></div>` +
		`<script type="application/json" id="__NUXT_DATA__">[{"title":1},"Hydrated Nuxt Title"]</script>` +
		`<script src="/_nuxt/entry.js"></script>` +
		`</body></html>`

	// Pure CRA shell: empty #root, big inline script, no head fields, no words.
	craShell := `<html><head></head><body><div id="root"></div>` +
		`<script>` + strings.Repeat("var x=1;/*padding to exceed scriptCeil*/", 2000) + `</script>` +
		`</body></html>`

	// Malformed __NEXT_DATA__ JSON: payload must NOT count; falls through to head/word heuristics.
	const malformedNext = `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">{not valid json,,,}</script>` +
		`<script>` + `</script></body></html>`

	// SSR page that ALSO carries data-reactroot + ng-version fingerprints but has real
	// content — fingerprints must NOT force a needs-JS verdict (false-positive guard).
	const ssrWithFingerprints = `<html><head><title>Fingerprinted SSR</title></head>` +
		`<body><div id="root" data-reactroot="" ng-version="17.0.0">` +
		`<h1>Heading</h1><p>Lots of real visible words right here in the server HTML ` +
		`so this clearly reads as server rendered despite the framework fingerprints ` +
		`present on the root element of the document body content.</p></div>` +
		`</body></html>`

	// Head-only shell (excalidraw-like): the SEO head (title/meta) is in the server
	// HTML, but the body is an empty framework root with no prose and no hydration
	// payload — the body content is NOT recoverable without JavaScript.
	const headOnlyShell = `<html><head><title>Client App</title>` +
		`<meta name="description" content="static head meta on a client-rendered app"></head>` +
		`<body><div id="root"></div><script src="/assets/index.js"></script></body></html>`

	tests := []struct {
		name            string
		html            string
		contentType     string
		wantKind        RenderKind
		wantVisible     bool
		wantPayload     bool
		wantSignalFires string // a signal name that must be Present=true ("" to skip)
	}{
		{
			name:        "ssr_with_content",
			html:        ssrHTML,
			contentType: "text/html; charset=utf-8",
			wantKind:    ServerRendered,
			wantVisible: true,
		},
		{
			name:            "next_data_hydrated",
			html:            nextHTML,
			contentType:     "text/html",
			wantKind:        Hydrated,
			wantVisible:     true,
			wantPayload:     true,
			wantSignalFires: "next_data_payload",
		},
		{
			name:            "nuxt_data_hydrated",
			html:            nuxtHTML,
			contentType:     "text/html",
			wantKind:        Hydrated,
			wantVisible:     true,
			wantPayload:     true,
			wantSignalFires: "nuxt_data_payload",
		},
		{
			name:            "cra_client_shell",
			html:            craShell,
			contentType:     "text/html",
			wantKind:        ClientShell,
			wantVisible:     false,
			wantSignalFires: "empty_framework_root",
		},
		{
			name:        "malformed_next_data_falls_through",
			html:        malformedNext,
			contentType: "text/html",
			// No valid payload, no head fields, no words, empty root, but scripts are
			// tiny so it is not a confident ClientShell either => Unknown.
			wantKind:    Unknown,
			wantVisible: false,
			wantPayload: false,
		},
		{
			name:        "non_html_unknown",
			html:        `{"json":true}`,
			contentType: "application/json",
			wantKind:    Unknown,
			wantVisible: false,
		},
		{
			name:        "empty_body_unknown",
			html:        ``,
			contentType: "text/html",
			wantKind:    Unknown,
			wantVisible: false,
		},
		{
			name:            "ssr_with_fingerprints_not_needs_js",
			html:            ssrWithFingerprints,
			contentType:     "text/html",
			wantKind:        ServerRendered,
			wantVisible:     true,
			wantSignalFires: "react_fingerprint",
		},
		{
			name:            "head_only_shell",
			html:            headOnlyShell,
			contentType:     "text/html",
			wantKind:        HeadOnlyShell,
			wantVisible:     false, // head fields visible, but body content is not recoverable
			wantSignalFires: "empty_framework_root",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect([]byte(tc.html), tc.contentType)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.ContentVisibleToCrawler != tc.wantVisible {
				t.Errorf("ContentVisibleToCrawler = %t, want %t", got.ContentVisibleToCrawler, tc.wantVisible)
			}
			if got.HydrationPayload != tc.wantPayload {
				t.Errorf("HydrationPayload = %t, want %t", got.HydrationPayload, tc.wantPayload)
			}
			if tc.wantSignalFires != "" {
				s, ok := signalByName(got.Signals, tc.wantSignalFires)
				if !ok {
					t.Fatalf("signal %q not evaluated; signals=%v", tc.wantSignalFires, got.Signals)
				}
				if !s.Present {
					t.Errorf("signal %q Present = false, want true", tc.wantSignalFires)
				}
			}
			// Signals must always be populated (auditable, never a black box).
			if len(got.Signals) == 0 {
				t.Errorf("Signals is empty; the detector must always list evaluated signals")
			}
			// Summary and Advice must always be set (honest reporting).
			if strings.TrimSpace(got.Summary) == "" {
				t.Errorf("Summary is empty")
			}
			if strings.TrimSpace(got.Advice) == "" {
				t.Errorf("Advice is empty")
			}
		})
	}
}

// TestDetectKeyRule makes the KEY RULE load-bearing: a hydration payload that recovers
// a SEO field must yield ContentVisibleToCrawler=true and never ClientShell, even when
// the framework root div is empty and the script bytes are large.
func TestDetectKeyRule(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"Recovered Title"}}}</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == ClientShell {
		t.Fatalf("KEY RULE violated: hydration payload present but Kind = ClientShell")
	}
	if got.Kind != Hydrated {
		t.Errorf("Kind = %q, want Hydrated", got.Kind)
	}
	if !got.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = false, want true (payload present)")
	}
}

// TestDetectFingerprintAlone proves a bare framework fingerprint with no real
// content does NOT on its own produce a needs-JS (ClientShell) verdict unless the
// supporting low-words/high-scripts/empty-root signals also fire.
func TestDetectFingerprintAlone(t *testing.T) {
	// data-reactroot present, real content, small scripts: must be ServerRendered.
	html := `<html><head><title>t</title></head><body data-reactroot="">` +
		`<p>genuine words present here in the body element of this document so the ` +
		`visible word count exceeds the floor comfortably for a server rendered read</p>` +
		`</body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == ClientShell {
		t.Errorf("fingerprint + real content must not be ClientShell; got ClientShell")
	}
	if !got.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = false, want true")
	}
}

// TestDetectBoundaries proves the tunable constants gate ClientShell vs Unknown
// right at the wordFloor and scriptCeil thresholds.
func TestDetectBoundaries(t *testing.T) {
	// Below scriptCeil with an empty root and no head/words => not a confident
	// ClientShell (scripts too small) => Unknown.
	thinScript := `<html><head></head><body><div id="root"></div>` +
		`<script>var a=1;</script></body></html>`
	if got := Detect([]byte(thinScript), "text/html"); got.Kind == ClientShell {
		t.Errorf("small-script empty shell should be Unknown, not ClientShell")
	}

	// Words at/above the floor flip an otherwise-empty-root page to ServerRendered.
	manyWords := "<html><head></head><body><div id=\"root\">" +
		strings.Repeat("word ", wordFloor+5) +
		"</div></body></html>"
	if got := Detect([]byte(manyWords), "text/html"); got.Kind != ServerRendered {
		t.Errorf("Kind = %q, want ServerRendered (words >= wordFloor)", got.Kind)
	}
}

// TestDetectEmptyNuxtNotHydrated proves an EMPTY <script id="__NUXT_DATA__"></script>
// tag (zero recoverable content) does NOT count as a hydration payload — a presence-only
// marker carrying no content must not be graded Hydrated/high/recoverable (honesty: never
// claim "recoverable without JS" from a payload tag that holds nothing).
func TestDetectEmptyNuxtNotHydrated(t *testing.T) {
	html := `<html><head></head><body><div id="__nuxt"></div>` +
		`<script id="__NUXT_DATA__"></script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.HydrationPayload {
		t.Errorf("empty __NUXT_DATA__ must not set HydrationPayload=true")
	}
	if got.Kind == Hydrated {
		t.Errorf("empty __NUXT_DATA__ tag must not grade Hydrated; got %q", got.Kind)
	}
	s, ok := signalByName(got.Signals, "nuxt_data_payload")
	if !ok {
		t.Fatalf("nuxt_data_payload signal not evaluated")
	}
	if s.Present {
		t.Errorf("nuxt_data_payload Present = true for an empty tag, want false")
	}
}

// TestDetectNuxtConfidenceMedium proves a Nuxt payload that recovers a head field is
// graded Hydrated at MEDIUM confidence (not High): the devalue wire format earns the
// recovery, but unlike the standardized __NEXT_DATA__ JSON it is not graded high
// (calibrated-hint mandate). The payload recovers Title via a devalue index reference.
func TestDetectNuxtConfidenceMedium(t *testing.T) {
	html := `<html><head></head><body><div id="__nuxt"></div>` +
		`<script id="__NUXT_DATA__">[{"title":1},"Hydrated Nuxt Title"]</script>` +
		`<script src="/_nuxt/entry.js"></script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind != Hydrated {
		t.Fatalf("Kind = %q, want Hydrated", got.Kind)
	}
	if got.Confidence != ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium (presence-only payload)", got.Confidence)
	}
}

// TestDetectNextDataConfidenceHigh proves a PARSED __NEXT_DATA__ payload that recovers a
// SEO field stays at HIGH confidence — validation (json.Unmarshal) plus a recovered field
// earns the stronger grade.
func TestDetectNextDataConfidenceHigh(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"Recovered Title"}}}</script>` +
		`<script src="/_next/static/main.js"></script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind != Hydrated {
		t.Fatalf("Kind = %q, want Hydrated", got.Kind)
	}
	if got.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want high (parsed __NEXT_DATA__)", got.Confidence)
	}
}

// TestDetectRSCFlightNotClientShell proves an App Router page that ships ONLY streaming
// RSC rows (self.__next_f) with an empty #__next, thin text, large scripts, and no head
// is NOT flagged ClientShell. The research note records that RSC pages carry recoverable
// server-streamed content and their SEO <head> is already in the raw HTML, so a present
// RSC stream blocks the needs-JS call (routed to Unknown, since the stream is undecoded
// — never to Hydrated/high, which would over-claim a proprietary wire format).
func TestDetectRSCFlightNotClientShell(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script>self.__next_f=self.__next_f||[];self.__next_f.push([1,"row"])</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == ClientShell {
		t.Errorf("RSC flight present must not grade ClientShell; got ClientShell")
	}
	// It is presence-only/undecoded, so it must NOT be claimed as a decisive payload.
	if got.HydrationPayload {
		t.Errorf("RSC flight must not set HydrationPayload=true (undecoded wire format)")
	}
	s, ok := signalByName(got.Signals, "next_rsc_flight")
	if !ok || !s.Present {
		t.Fatalf("next_rsc_flight signal must be evaluated and Present; got ok=%t signal=%+v", ok, s)
	}
}

// TestDetectHeadOnlyShell makes the honest "partial" case load-bearing: a page whose
// SEO <head> is server-rendered but whose body is an empty client shell with no
// hydration payload must be graded HeadOnlyShell — NOT the falsely-reassuring
// ServerRendered — with the body content marked not visible to the crawler. This is the
// user's #1 requirement: when the frontend hides content, say so.
func TestDetectHeadOnlyShell(t *testing.T) {
	html := `<html><head><title>App</title><meta name="description" content="d"></head>` +
		`<body><div id="root"></div><script src="/app.js"></script></body></html>`
	got := Detect([]byte(html), "text/html")
	// Must be HeadOnlyShell, NOT the falsely-reassuring ServerRendered (which would claim
	// full monitorability for a page whose body is a client shell).
	if got.Kind != HeadOnlyShell {
		t.Fatalf("Kind = %q, want head_only_shell (head present, body is an empty client shell)", got.Kind)
	}
	if got.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = true, want false (body content not recoverable)")
	}
	// The empty-root and missing-body signals must be surfaced so the call is auditable.
	if s, ok := signalByName(got.Signals, "empty_framework_root"); !ok || !s.Present {
		t.Errorf("empty_framework_root signal must be present; got ok=%t %+v", ok, s)
	}
}

// TestDetectDegenerateNextDataNotHydrated proves a __NEXT_DATA__ tag whose JSON decodes
// to a degenerate value (null/false/0/"x"/[]/{}) carries zero recoverable hydration
// content and must NOT be claimed as a payload. json.Unmarshal succeeds on all of these,
// but only a NON-EMPTY object or array is a real hydration payload — anything else would
// falsely grade the page Hydrated/high/recoverable, violating the package honesty mandate.
func TestDetectDegenerateNextDataNotHydrated(t *testing.T) {
	degenerate := []struct {
		name string
		body string
	}{
		{"null", "null"},
		{"false", "false"},
		{"zero", "0"},
		{"string", `"x"`},
		{"empty_array", "[]"},
		{"empty_object", "{}"},
	}
	for _, d := range degenerate {
		t.Run(d.name, func(t *testing.T) {
			// Empty #__next, big inline script, no head/words: with a real payload this
			// would grade Hydrated/recoverable; a degenerate payload must NOT.
			html := `<html><head></head><body><div id="__next"></div>` +
				`<script id="__NEXT_DATA__" type="application/json">` + d.body + `</script>` +
				`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
			got := Detect([]byte(html), "text/html")
			if got.HydrationPayload {
				t.Errorf("degenerate __NEXT_DATA__ (%s) must not set HydrationPayload=true", d.body)
			}
			if got.Kind == Hydrated {
				t.Errorf("degenerate __NEXT_DATA__ (%s) must not grade Hydrated; got %q", d.body, got.Kind)
			}
			if got.ContentVisibleToCrawler {
				t.Errorf("degenerate __NEXT_DATA__ (%s) must not be ContentVisibleToCrawler via the payload path", d.body)
			}
			s, ok := signalByName(got.Signals, "next_data_payload")
			if !ok {
				t.Fatalf("next_data_payload signal not evaluated")
			}
			if s.Present {
				t.Errorf("next_data_payload Present = true for degenerate value %s, want false", d.body)
			}
		})
	}
}

// TestDetectNonEmptyNextDataHydrated is the positive counterpart: a payload that
// RECOVERS a SEO field — whether a head-key object or an array whose leaves carry real
// prose — IS a recoverable hydration payload and must grade Hydrated/high, so the
// field-recovery gate does not over-reject genuine field-bearing payloads. (A non-empty
// container that recovers NOTHING, e.g. [["k","v"]], is covered by the structural-only
// guard tests above and must NOT grade Hydrated.)
func TestDetectNonEmptyNextDataHydrated(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"head_key_object", `{"props":{"pageProps":{"title":"x"}}}`},
		{"array_with_prose", `[["This is a genuine long article body with plenty of real words ` +
			`so the prose harvester comfortably clears its minimum word floor here today."]]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html := `<html><head></head><body><div id="__next"></div>` +
				`<script id="__NEXT_DATA__" type="application/json">` + c.body + `</script>` +
				`<script src="/_next/static/main.js"></script></body></html>`
			got := Detect([]byte(html), "text/html")
			if !got.HydrationPayload {
				t.Errorf("non-empty __NEXT_DATA__ (%s) must set HydrationPayload=true", c.body)
			}
			if got.Kind != Hydrated {
				t.Errorf("non-empty __NEXT_DATA__ (%s) Kind = %q, want Hydrated", c.body, got.Kind)
			}
			if got.Confidence != ConfidenceHigh {
				t.Errorf("non-empty parsed __NEXT_DATA__ (%s) Confidence = %q, want high", c.body, got.Confidence)
			}
		})
	}
}

// TestDetectStructuralPayloadNoFieldsNotHydrated is the LOCKED-DECISION guard
// ("grade on actual field recovery"): a __NEXT_DATA__ that decodes into a real,
// non-degenerate structure but whose payload recovers ZERO SEO fields (empty
// title/meta/canonical, no JSON-LD, no prose) must NOT grade Hydrated/HIGH/recoverable.
// Decoding succeeded structurally, but the extractor would back-fill nothing, so the
// honest verdict is graded on what was ACTUALLY recovered — head_only_shell when the
// server head is present, never a reassuring Hydrated that claims "SEO fields recoverable".
func TestDetectStructuralPayloadNoFieldsNotHydrated(t *testing.T) {
	// App-specific Next.js payload: non-degenerate object, but the head keys are not in
	// the allowlist and there is no harvestable prose — FromNextData recovers nothing.
	// Server head (title/meta) IS present, so the honest reclassification is head_only_shell.
	html := `<html><head><title>App</title>` +
		`<meta name="description" content="static head meta"></head>` +
		`<body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{}},"page":"/","query":{},"buildId":"abc123"}</script>` +
		`<script src="/_next/static/main.js"></script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == Hydrated {
		t.Fatalf("structural-only payload (no recovered fields) must NOT grade Hydrated; got Hydrated")
	}
	if got.Kind != HeadOnlyShell {
		t.Errorf("Kind = %q, want head_only_shell (head present, payload recovered nothing)", got.Kind)
	}
	if got.HydrationPayload {
		t.Errorf("HydrationPayload = true; a payload that recovers zero SEO fields must not be claimed")
	}
	if got.Confidence == ConfidenceHigh {
		t.Errorf("Confidence = high; a payload that recovered nothing must never grade high")
	}
	// Doctor output must not claim "SEO fields recoverable" when nothing was recovered.
	if strings.Contains(got.Summary, "recoverable") {
		t.Errorf("Summary claims recoverability for an empty payload: %q", got.Summary)
	}
}

// TestDetectStructuralPayloadNoHeadNoFields is the no-head counterpart of the
// LOCKED-DECISION guard: a structural-only payload with no recovered fields AND no
// server head must not grade Hydrated either — it falls through to the honest
// client-shell/unknown reads, never a reassuring recoverable verdict.
func TestDetectStructuralPayloadNoHeadNoFields(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{}},"page":"/"}</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == Hydrated {
		t.Fatalf("structural-only payload (no head, no recovered fields) must NOT grade Hydrated")
	}
	if got.HydrationPayload {
		t.Errorf("HydrationPayload = true; a payload that recovers zero SEO fields must not be claimed")
	}
	if got.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = true via an empty payload; nothing was recovered")
	}
	if strings.Contains(got.Summary, "recoverable") {
		t.Errorf("Summary claims recoverability for an empty payload: %q", got.Summary)
	}
}

// TestDetectPayloadWithProseHydrated is the positive counterpart: a payload that
// recovers REAL content (body prose here) IS recoverable and must still grade
// Hydrated/visible — the field-recovery gate must not over-reject genuine payloads
// whose recovery rides on prose/JSON-LD rather than the narrow head-key allowlist.
func TestDetectPayloadWithProseHydrated(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"body":"This is a genuine long article body with plenty ` +
		`of real words so the prose harvester comfortably clears its minimum word floor ` +
		`here today and recovers real recoverable body content from the payload."}}}</script>` +
		`<script src="/_next/static/main.js"></script></body></html>`
	got := Detect([]byte(html), "text/html")
	if !got.HydrationPayload {
		t.Errorf("payload with recoverable prose must set HydrationPayload=true")
	}
	if got.Kind != Hydrated {
		t.Errorf("Kind = %q, want Hydrated (real prose recovered)", got.Kind)
	}
	if !got.ContentVisibleToCrawler {
		t.Errorf("ContentVisibleToCrawler = false, want true (prose recovered)")
	}
}

// TestDetectNuxtGlobalAloneNotHydrated extends the LOCKED-DECISION gate to the legacy
// window.__NUXT__ global: it is a presence-only marker with no structured decoder, so it
// recovers ZERO SEO fields. Like a structural-only __NEXT_DATA__, a bare global must NOT
// by itself grade Hydrated/recoverable — recoverability is graded on fields actually
// recovered, and a presence marker recovers nothing. It stays an informational framework
// fingerprint (guessFramework still reads it), never a recoverability claim.
func TestDetectNuxtGlobalAloneNotHydrated(t *testing.T) {
	html := `<html><head></head><body><div id="__nuxt"></div>` +
		`<script>window.__NUXT__={state:{}}</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`
	got := Detect([]byte(html), "text/html")
	if got.Kind == Hydrated {
		t.Fatalf("bare window.__NUXT__ (no recoverable fields) must NOT grade Hydrated")
	}
	if got.HydrationPayload {
		t.Errorf("HydrationPayload = true for a presence-only global that recovered nothing")
	}
	if strings.Contains(got.Summary, "recoverable") {
		t.Errorf("Summary claims recoverability for a presence-only global: %q", got.Summary)
	}
	// The framework guess must still recognize Nuxt from the global (informational).
	if got.Framework != "nuxt" {
		t.Errorf("Framework = %q, want nuxt (global is still an informational fingerprint)", got.Framework)
	}
	// The signal must still be surfaced as present (auditable).
	if s, ok := signalByName(got.Signals, "nuxt_global"); !ok || !s.Present {
		t.Errorf("nuxt_global signal must be evaluated and Present; got ok=%t %+v", ok, s)
	}
}

// TestDetectScriptBytesExcludesPayload proves the large_script_bytes heuristic counts
// EXECUTABLE bundle weight, not recoverable data: __NEXT_DATA__/__NUXT_DATA__/JSON-LD
// payload bytes are excluded from ScriptBytes so the displayed figure is honest.
func TestDetectScriptBytesExcludesPayload(t *testing.T) {
	bigPayload := strings.Repeat("a", 50000)
	html := `<html><head><title>t</title></head><body>` +
		`<script id="__NEXT_DATA__" type="application/json">{"x":"` + bigPayload + `"}</script>` +
		`<script type="application/ld+json">{"@context":"` + bigPayload + `"}</script>` +
		`<script>var a=1;</script></body></html>`
	got := Detect([]byte(html), "text/html")
	// Only the tiny executable <script>var a=1;</script> should count.
	if got.ScriptBytes > 100 {
		t.Errorf("ScriptBytes = %d; payload/JSON-LD bytes must be excluded (want small executable-only count)", got.ScriptBytes)
	}
}
