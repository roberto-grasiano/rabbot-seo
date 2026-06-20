package precheck

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestDetectRSCFlightDecodedHydrated is the A8 RSC upgrade (acceptance #10): an App
// Router page whose self.__next_f stream actually DECODES via hydration.FromFlight
// (real element tuples — a title row here) finally applies the KEY RULE to RSC — a
// decoded payload means content is recoverable without JS. It must grade Hydrated at
// MEDIUM confidence (the wire format is proprietary, so never the High that a parsed
// __NEXT_DATA__ earns), set HydrationPayload=true, and ContentVisibleToCrawler=true —
// even with an empty #__next, thin visible text, and large executable scripts.
func TestDetectRSCFlightDecodedHydrated(t *testing.T) {
	// A real flight push carrying a title element tuple: hydration.FromFlight decodes
	// the "1:[\"$\",\"title\",null,{\"children\":\"Decoded RSC Title\"}]" row.
	row := `1:["$","title",null,{"children":"Decoded RSC Title"}]`
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script>self.__next_f=self.__next_f||[];self.__next_f.push([1,` + jsonString(row) + `])</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`

	got := Detect([]byte(html), "text/html")
	if got.Kind != Hydrated {
		t.Fatalf("decoded RSC flight Kind = %q, want Hydrated (KEY RULE applies to a decoded payload)", got.Kind)
	}
	if !got.HydrationPayload {
		t.Errorf("decoded RSC flight must set HydrationPayload=true")
	}
	if !got.ContentVisibleToCrawler {
		t.Errorf("decoded RSC flight ContentVisibleToCrawler = false, want true")
	}
	if got.Confidence != ConfidenceMedium {
		t.Errorf("decoded RSC flight Confidence = %q, want medium (proprietary wire format, never high)", got.Confidence)
	}
	// The flight signal must still be surfaced as present (auditable).
	s, ok := signalByName(got.Signals, "next_rsc_flight")
	if !ok || !s.Present {
		t.Fatalf("next_rsc_flight signal must be evaluated and Present; got ok=%t signal=%+v", ok, s)
	}
}

// TestDetectRSCFlightUndecodableUnknown is the other arm (BOTH ARMS lesson): a
// self.__next_f blob that does NOT decode into any recoverable element (the bare
// "row" string carries no JSON tuple) must STILL block the needs-JS call but route to
// Unknown — never ClientShell and never an over-claimed Hydrated. This preserves the
// honest "present but unreadable" behavior for proprietary streams we cannot parse.
func TestDetectRSCFlightUndecodableUnknown(t *testing.T) {
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script>self.__next_f=self.__next_f||[];self.__next_f.push([1,"row"])</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`

	got := Detect([]byte(html), "text/html")
	if got.Kind == ClientShell {
		t.Errorf("undecodable RSC flight must not grade ClientShell; got ClientShell")
	}
	if got.Kind != Unknown {
		t.Errorf("undecodable RSC flight Kind = %q, want Unknown (present but unreadable)", got.Kind)
	}
	if got.HydrationPayload {
		t.Errorf("undecodable RSC flight must not set HydrationPayload=true (nothing decoded)")
	}
	if got.ContentVisibleToCrawler {
		t.Errorf("undecodable RSC flight ContentVisibleToCrawler = true, want false")
	}
	s, ok := signalByName(got.Signals, "next_rsc_flight")
	if !ok || !s.Present {
		t.Fatalf("next_rsc_flight signal must be evaluated and Present; got ok=%t signal=%+v", ok, s)
	}
}

// TestDetectRSCFlightEscapedRowDecodes hardens the flight-row harvester against the
// escape-handling bug class: a real RSC push embeds a JSON-LD script element whose row
// string contains ESCAPED double quotes (\" around the inner ld+json). The harvester
// must read the whole quoted literal — not stop at the first inner \" — so hydration can
// decode it. With multiple element tuples in one row (title + script), this also proves
// a single push carrying recoverable content folds into Hydrated.
func TestDetectRSCFlightEscapedRowDecodes(t *testing.T) {
	// A flight row carrying a title tuple followed by a JSON-LD script tuple. The inner
	// __html is itself a JSON document, so the row string is densely escaped.
	row := `2:[["$","title",null,{"children":"Escaped RSC Title"}],` +
		`["$","script",null,{"type":"application/ld+json",` +
		`"dangerouslySetInnerHTML":{"__html":"{\"@type\":\"Article\",\"headline\":\"RSC Article\"}"}}]]`
	html := `<html><head></head><body><div id="__next"></div>` +
		`<script>self.__next_f=self.__next_f||[];self.__next_f.push([1,` + jsonString(row) + `])</script>` +
		`<script>` + strings.Repeat("x", 200000) + `</script></body></html>`

	got := Detect([]byte(html), "text/html")
	if got.Kind != Hydrated {
		t.Fatalf("escaped RSC row Kind = %q, want Hydrated (row with escaped quotes must decode)", got.Kind)
	}
	if !got.HydrationPayload {
		t.Errorf("escaped RSC row must set HydrationPayload=true")
	}
	if got.Confidence != ConfidenceMedium {
		t.Errorf("escaped RSC row Confidence = %q, want medium", got.Confidence)
	}
}

// TestDetectDocMatchesDetect proves the factored DetectDoc core produces the identical
// result to Detect when handed the SAME already-parsed document — extract reuses this
// path to classify WITHOUT a third DOM parse, so the two must never diverge.
func TestDetectDocMatchesDetect(t *testing.T) {
	htmls := []string{
		`<html><head><title>SSR</title></head><body><h1>h</h1>` +
			`<p>plenty of genuine server rendered words here so this clearly reads as a ` +
			`server rendered page comfortably above the visible word floor for the heuristics.</p></body></html>`,
		`<html><head></head><body><div id="__next"></div>` +
			`<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"x"}}}</script>` +
			`<script src="/_next/static/main.js"></script></body></html>`,
		`<html><head><title>App</title><meta name="description" content="d"></head>` +
			`<body><div id="root"></div><script src="/assets/index.js"></script></body></html>`,
	}
	for i, h := range htmls {
		viaDetect := Detect([]byte(h), "text/html")
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(h)))
		if err != nil {
			t.Fatalf("case %d: parse: %v", i, err)
		}
		viaDoc := DetectDoc(doc, "text/html")
		if viaDoc.Kind != viaDetect.Kind {
			t.Errorf("case %d: DetectDoc.Kind = %q, Detect.Kind = %q", i, viaDoc.Kind, viaDetect.Kind)
		}
		if viaDoc.ContentVisibleToCrawler != viaDetect.ContentVisibleToCrawler {
			t.Errorf("case %d: visible mismatch DetectDoc=%t Detect=%t", i, viaDoc.ContentVisibleToCrawler, viaDetect.ContentVisibleToCrawler)
		}
		if viaDoc.HydrationPayload != viaDetect.HydrationPayload {
			t.Errorf("case %d: payload mismatch DetectDoc=%t Detect=%t", i, viaDoc.HydrationPayload, viaDetect.HydrationPayload)
		}
		if viaDoc.Confidence != viaDetect.Confidence {
			t.Errorf("case %d: confidence mismatch DetectDoc=%q Detect=%q", i, viaDoc.Confidence, viaDetect.Confidence)
		}
	}
}

// TestRenderKindModelDriftGuard is the drift guard (acceptance #9): the set of
// precheck.RenderKind HINT values MUST equal the set of model.RenderMode persisted
// values, so a new render kind can never be added on one side and silently dropped on
// the other (a missing value would make a whole classification vanish from a surface).
// model cannot import precheck (it would cycle), so the equality is pinned HERE, in a
// precheck test, which may legally import model.
func TestRenderKindModelDriftGuard(t *testing.T) {
	// Every precheck.RenderKind the classifier can emit.
	precheckKinds := []RenderKind{
		ServerRendered,
		Hydrated,
		HeadOnlyShell,
		ClientShell,
		Unknown,
	}
	// Every model.RenderMode the snapshot can persist.
	modelModes := []model.RenderMode{
		model.RenderServerRendered,
		model.RenderHydrated,
		model.RenderHeadOnlyShell,
		model.RenderClientShell,
		model.RenderUnknown,
	}

	if len(precheckKinds) != len(modelModes) {
		t.Fatalf("set sizes diverged: precheck has %d kinds, model has %d modes", len(precheckKinds), len(modelModes))
	}

	precheckSet := make(map[string]struct{}, len(precheckKinds))
	for _, k := range precheckKinds {
		precheckSet[string(k)] = struct{}{}
	}
	modelSet := make(map[string]struct{}, len(modelModes))
	for _, m := range modelModes {
		modelSet[string(m)] = struct{}{}
	}

	for v := range precheckSet {
		if _, ok := modelSet[v]; !ok {
			t.Errorf("precheck.RenderKind %q has no matching model.RenderMode", v)
		}
	}
	for v := range modelSet {
		if _, ok := precheckSet[v]; !ok {
			t.Errorf("model.RenderMode %q has no matching precheck.RenderKind", v)
		}
	}

	// Pin the cross-cast directly too, so a value-level (not just set-level) rename on
	// either side is caught: each precheck kind cast to model.RenderMode must equal the
	// model const that names the same concept.
	pairs := []struct {
		kind RenderKind
		mode model.RenderMode
	}{
		{ServerRendered, model.RenderServerRendered},
		{Hydrated, model.RenderHydrated},
		{HeadOnlyShell, model.RenderHeadOnlyShell},
		{ClientShell, model.RenderClientShell},
		{Unknown, model.RenderUnknown},
	}
	for _, p := range pairs {
		if model.RenderMode(p.kind) != p.mode {
			t.Errorf("drift: model.RenderMode(%q) = %q, want %q", string(p.kind), model.RenderMode(p.kind), p.mode)
		}
	}
}

// jsonString returns s as a JSON string literal (quoted, escaped), used to embed a
// flight row inside a self.__next_f.push([...]) script body the way the framework does.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
