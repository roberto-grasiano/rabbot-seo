package hydration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// defaultCap is the 2 MiB byte cap the production caller (extract) passes by
// default; the tests use it as a generous ceiling that is never exceeded by the
// small fixtures, so cap behavior is isolated to the dedicated over-cap tests.
const defaultCap = 2 << 20

// ── FromNextData ────────────────────────────────────────────────────────────

// realisticNextData mirrors a Next.js Pages Router __NEXT_DATA__ payload that
// carries SEO fields under props.pageProps (the app-specific seo.* / metaTitle
// shape the harvest allowlist targets) plus a buildId (a volatile key that must
// never leak into recovered prose).
const realisticNextData = `{
  "props": {
    "pageProps": {
      "seo": {
        "title": "Recovered SEO Title",
        "description": "A meta description recovered from the hydration payload.",
        "canonicalUrl": "https://example.com/canonical"
      },
      "article": {
        "body": "The quick brown fox jumps over the lazy dog. This is a sufficiently long prose paragraph of real human readable sentences carrying genuine page content worth monitoring for changes over time."
      }
    }
  },
  "buildId": "abc123DEPLOYHASH",
  "page": "/post/[slug]"
}`

func TestFromNextDataRecoversHeadFields(t *testing.T) {
	got, err := FromNextData([]byte(realisticNextData), defaultCap)
	if err != nil {
		t.Fatalf("FromNextData errored on a valid payload: %v", err)
	}
	if !got.Decoded {
		t.Fatalf("Decoded=false on a non-degenerate payload; want true")
	}
	if got.Truncated {
		t.Fatalf("Truncated=true on an under-cap payload; want false")
	}
	if got.Title != "Recovered SEO Title" {
		t.Errorf("Title = %q; want %q", got.Title, "Recovered SEO Title")
	}
	if got.MetaDescription != "A meta description recovered from the hydration payload." {
		t.Errorf("MetaDescription = %q; want recovered description", got.MetaDescription)
	}
	if got.Canonical != "https://example.com/canonical" {
		t.Errorf("Canonical = %q; want recovered canonicalUrl", got.Canonical)
	}
}

func TestFromNextDataRecoversProseExcludingVolatileKeys(t *testing.T) {
	got, err := FromNextData([]byte(realisticNextData), defaultCap)
	if err != nil {
		t.Fatalf("FromNextData errored: %v", err)
	}
	joined := strings.Join(got.BodyTextCandidates, "\n")
	if !strings.Contains(joined, "quick brown fox") {
		t.Errorf("prose harvest missed the article body; got candidates: %q", got.BodyTextCandidates)
	}
	// The volatile buildId hash must never be harvested as prose — it is the
	// deploy-only churn-defence invariant (acceptance #5).
	if strings.Contains(joined, "abc123DEPLOYHASH") {
		t.Errorf("volatile buildId leaked into recovered prose: %q", joined)
	}
}

// TestFromNextDataVolatileFlipIsStable proves the churn defence directly: two
// payloads identical except the buildId yield identical recovered Fields. A
// deploy-only buildId flip must never change the recovered content (acceptance
// #5 — the volatile-key filter is the reason).
func TestFromNextDataVolatileFlipIsStable(t *testing.T) {
	a, err := FromNextData([]byte(realisticNextData), defaultCap)
	if err != nil {
		t.Fatalf("FromNextData(a) errored: %v", err)
	}
	flipped := strings.Replace(realisticNextData, "abc123DEPLOYHASH", "zzz999NEWDEPLOY", 1)
	if flipped == realisticNextData {
		t.Fatalf("test bug: buildId replacement did not change the input")
	}
	b, err := FromNextData([]byte(flipped), defaultCap)
	if err != nil {
		t.Fatalf("FromNextData(b) errored: %v", err)
	}
	if strings.Join(a.BodyTextCandidates, "|") != strings.Join(b.BodyTextCandidates, "|") {
		t.Errorf("buildId flip changed recovered prose:\n a=%q\n b=%q", a.BodyTextCandidates, b.BodyTextCandidates)
	}
	if a.Title != b.Title || a.MetaDescription != b.MetaDescription || a.Canonical != b.Canonical {
		t.Errorf("buildId flip changed recovered head fields: a=%+v b=%+v", a, b)
	}
}

// TestFromNextDataDeterministicProseOrder pins the determinism invariant: the
// prose harvest walks Go maps (randomized iteration order), so the recovered
// candidate slice must be stably ordered across repeated decodes of the same
// payload. Non-determinism here would churn the extract-side content hash and
// emit false `content` change alerts. The fixture deliberately yields more than
// one candidate (the article body AND the meta-description sentence) so map
// order actually matters.
func TestFromNextDataDeterministicProseOrder(t *testing.T) {
	first, err := FromNextData([]byte(realisticNextData), defaultCap)
	if err != nil {
		t.Fatalf("FromNextData errored: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := FromNextData([]byte(realisticNextData), defaultCap)
		if err != nil {
			t.Fatalf("FromNextData errored on repeat %d: %v", i, err)
		}
		if strings.Join(got.BodyTextCandidates, "\x00") != strings.Join(first.BodyTextCandidates, "\x00") {
			t.Fatalf("non-deterministic prose order on repeat %d:\n first=%q\n got  =%q",
				i, first.BodyTextCandidates, got.BodyTextCandidates)
		}
	}
}

func TestFromNextDataDegeneratePayloadsYieldNoRecovery(t *testing.T) {
	cases := map[string]string{
		"scalar_number": `42`,
		"scalar_string": `"just a string"`,
		"scalar_null":   `null`,
		"scalar_bool":   `true`,
		"empty_object":  `{}`,
		"empty_array":   `[]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FromNextData([]byte(raw), defaultCap)
			if err != nil {
				t.Fatalf("FromNextData(%q) errored; degenerate should be a clean no-recovery, not an error: %v", raw, err)
			}
			if got.Decoded {
				t.Errorf("Decoded=true on degenerate payload %q; want false (no recovery claim)", raw)
			}
			if got.Title != "" || got.MetaDescription != "" || got.Canonical != "" || len(got.BodyTextCandidates) != 0 {
				t.Errorf("degenerate payload %q recovered fields: %+v; want zero", raw, got)
			}
		})
	}
}

func TestFromNextDataMalformedReturnsError(t *testing.T) {
	got, err := FromNextData([]byte(`{not valid json`), defaultCap)
	if err == nil {
		t.Fatalf("FromNextData on malformed JSON returned nil error; want an error")
	}
	if got.Decoded {
		t.Errorf("Decoded=true on malformed input; want false")
	}
}

func TestFromNextDataOverCapTruncates(t *testing.T) {
	got, err := FromNextData([]byte(realisticNextData), 8 /* bytes — far below the fixture size */)
	if err != nil {
		t.Fatalf("over-cap FromNextData errored; want a clean truncation marker: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated=false on an over-cap payload; want true")
	}
	if got.Decoded {
		t.Errorf("Decoded=true on an over-cap payload; want false (no recovery)")
	}
	if got.Title != "" || got.MetaDescription != "" || got.Canonical != "" || len(got.BodyTextCandidates) != 0 {
		t.Errorf("over-cap payload recovered fields: %+v; want zero", got)
	}
}

func TestFromNextDataDOMEmptyTitleIsRecovered(t *testing.T) {
	// title (not seo.title) under pageProps — the flatter allowlist key.
	raw := `{"props":{"pageProps":{"title":"Plain Title Key","description":"d","canonical":"https://e.com/c"}}}`
	got, err := FromNextData([]byte(raw), defaultCap)
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if got.Title != "Plain Title Key" {
		t.Errorf("Title = %q; want %q (metaTitle/title allowlist key)", got.Title, "Plain Title Key")
	}
	if got.Canonical != "https://e.com/c" {
		t.Errorf("Canonical = %q; want recovered canonical", got.Canonical)
	}
}

// ── FromNuxtData (devalue) ──────────────────────────────────────────────────

// goldenNuxt is a Nuxt 3 __NUXT_DATA__ devalue payload: a flat JSON array where
// integer entries are INDEX references into the same array. Index 0 is the
// "root" object; its values point at other indexes. This shape:
//
//	[ {"seo":1,"body":4}, {"title":2,"description":3}, "Nuxt Title",
//	  "Nuxt description text.", "The quick brown fox jumps over the lazy dog ..." ]
//
// resolves to {seo:{title:"Nuxt Title",description:"Nuxt description text."},
// body:"<prose>"}.
const goldenNuxt = `[{"seo":1,"body":4},{"title":2,"description":3},"Nuxt Title","Nuxt description text.","The quick brown fox jumps over the lazy dog and keeps running across a long readable sentence of real page content worth monitoring."]`

func TestFromNuxtDataDecodesGolden(t *testing.T) {
	got, err := FromNuxtData([]byte(goldenNuxt), defaultCap)
	if err != nil {
		t.Fatalf("FromNuxtData errored on golden devalue: %v", err)
	}
	if !got.Decoded {
		t.Fatalf("Decoded=false on a valid devalue payload; want true")
	}
	if got.Title != "Nuxt Title" {
		t.Errorf("Title = %q; want %q (resolved through index refs)", got.Title, "Nuxt Title")
	}
	if got.MetaDescription != "Nuxt description text." {
		t.Errorf("MetaDescription = %q; want resolved description", got.MetaDescription)
	}
	if !strings.Contains(strings.Join(got.BodyTextCandidates, "\n"), "quick brown fox") {
		t.Errorf("prose body not recovered through index refs; candidates: %q", got.BodyTextCandidates)
	}
}

// TestFromNuxtDataCyclicTerminates throws a self-referential index cycle at the
// resolver: index 0 references index 1, index 1 references index 0. A naive
// resolver would infinite-loop; the visited-set + depth cap must terminate
// without panic (acceptance #2).
func TestFromNuxtDataCyclicTerminates(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// [ {"a":1}, {"b":0} ] — 0->1->0->... cycle.
		_, _ = FromNuxtData([]byte(`[{"a":1},{"b":0}]`), defaultCap)
	}()
	select {
	case <-done:
		// terminated — good.
	case <-timeoutAfter():
		t.Fatalf("FromNuxtData did not terminate on a cyclic-reference payload (infinite loop)")
	}
}

// TestFromNuxtDataSelfCycleTerminates is the tightest cycle: an index that
// references itself.
func TestFromNuxtDataSelfCycleTerminates(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = FromNuxtData([]byte(`[{"self":0}]`), defaultCap)
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatalf("FromNuxtData did not terminate on a self-referential index")
	}
}

// TestFromNuxtDataForwardBranchingDAGTerminates is the regression test for the
// exponential-blowup DoS. A forward-branching DAG — index i is the array [i+1,
// i+1], so each index references the NEXT index twice — is NOT a cycle: every
// reference points strictly forward, so the on-stack visited-set never trips. A
// resolver that re-materializes a shared child once per incoming edge does
// 2^N resolutions while depth is only ~N (well under maxWalkDepth=64). At N=30
// that is ~10^9 resolutions: a several-second hang on a sub-300-byte payload, on
// the crawl hot path, from attacker-controlled HTML. A bounded resolver (node
// budget and/or memoization) must terminate near-instantly. The 5s timeoutAfter
// budget is the oracle: a correct decoder finishes in microseconds.
func TestFromNuxtDataForwardBranchingDAGTerminates(t *testing.T) {
	const n = 30
	// Build [ [1,1], [2,2], ..., [n,n], "leaf prose ..." ].
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "[%d,%d],", i, i)
	}
	b.WriteString(`"leaf"]`)
	payload := []byte(b.String())
	if len(payload) > 512 {
		t.Fatalf("DAG payload unexpectedly large (%d bytes); the point is a tiny hostile input", len(payload))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = FromNuxtData(payload, defaultCap)
	}()
	select {
	case <-done:
		// terminated — good.
	case <-timeoutAfter():
		t.Fatalf("FromNuxtData did NOT terminate within budget on an N=%d forward-branching DAG (%d bytes) — exponential blowup", n, len(payload))
	}
}

func TestFromNuxtDataMalformedReturnsError(t *testing.T) {
	cases := map[string]string{
		"not_json":       `[1,2,`,
		"object_not_arr": `{"a":1}`, // devalue root must be an array
		"scalar":         `5`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FromNuxtData([]byte(raw), defaultCap)
			if err == nil {
				t.Fatalf("FromNuxtData(%q) returned nil error; want an error", raw)
			}
			if got.Decoded {
				t.Errorf("Decoded=true on malformed input %q", raw)
			}
		})
	}
}

func TestFromNuxtDataEmptyArrayIsDegenerate(t *testing.T) {
	got, err := FromNuxtData([]byte(`[]`), defaultCap)
	if err != nil {
		t.Fatalf("empty array should be a clean no-recovery, not an error: %v", err)
	}
	if got.Decoded {
		t.Errorf("Decoded=true on empty devalue array; want false")
	}
}

func TestFromNuxtDataOutOfRangeIndexDoesNotPanic(t *testing.T) {
	// index 0 references index 99, which does not exist — must not panic and
	// must not fabricate a value.
	got, err := FromNuxtData([]byte(`[{"seo":99}]`), defaultCap)
	if err != nil {
		t.Fatalf("out-of-range index errored unexpectedly: %v", err)
	}
	if got.Title != "" || got.MetaDescription != "" {
		t.Errorf("out-of-range index fabricated fields: %+v", got)
	}
}

func TestFromNuxtDataOverCapTruncates(t *testing.T) {
	got, err := FromNuxtData([]byte(goldenNuxt), 8)
	if err != nil {
		t.Fatalf("over-cap FromNuxtData errored; want truncation marker: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated=false on over-cap devalue; want true")
	}
	if got.Decoded || got.Title != "" {
		t.Errorf("over-cap devalue recovered fields: %+v; want zero", got)
	}
}

// ── FromFlight (RSC __next_f) ───────────────────────────────────────────────

// flightRows stream the four element kinds the decoder recovers plus an unknown
// tag (which must be skipped without error) and a prose text row.
func flightRows() []string {
	return []string{
		`1:["$","title",null,{"children":"Flight Title"}]`,
		`2:["$","meta",null,{"name":"description","content":"Flight meta description."}]`,
		`3:["$","link",null,{"rel":"canonical","href":"https://example.com/flight-canonical"}]`,
		`4:["$","script",null,{"type":"application/ld+json","dangerouslySetInnerHTML":{"__html":"{\"@type\":\"Article\",\"headline\":\"Flight Article\"}"}}]`,
		`5:["$","div",null,{"children":"The quick brown fox jumps over the lazy dog across a long readable sentence of recoverable page prose."}]`,
		`6:["$","customtag-unknown",null,{"foo":"bar"}]`,
	}
}

func TestFromFlightRecoversFourElementKinds(t *testing.T) {
	got, err := FromFlight(flightRows(), defaultCap)
	if err != nil {
		t.Fatalf("FromFlight errored: %v", err)
	}
	if !got.Decoded {
		t.Fatalf("Decoded=false on a decodable flight stream; want true")
	}
	if got.Title != "Flight Title" {
		t.Errorf("Title = %q; want %q", got.Title, "Flight Title")
	}
	if got.MetaDescription != "Flight meta description." {
		t.Errorf("MetaDescription = %q; want recovered meta", got.MetaDescription)
	}
	if got.Canonical != "https://example.com/flight-canonical" {
		t.Errorf("Canonical = %q; want recovered link[rel=canonical]", got.Canonical)
	}
	if len(got.JSONLD) == 0 {
		t.Fatalf("no JSON-LD recovered from the ld+json script element")
	}
	if !json.Valid(got.JSONLD[0]) {
		t.Errorf("recovered JSON-LD block is not valid JSON: %q", got.JSONLD[0])
	}
	if !strings.Contains(string(got.JSONLD[0]), "Flight Article") {
		t.Errorf("recovered JSON-LD missing expected headline: %q", got.JSONLD[0])
	}
}

func TestFromFlightSkipsUnknownTagsWithoutError(t *testing.T) {
	rows := []string{
		`1:["$","customtag-unknown",null,{"foo":"bar"}]`,
		`2:["$","another-unknown",null,{}]`,
		`3:["$","title",null,{"children":"Still Recovered"}]`,
	}
	got, err := FromFlight(rows, defaultCap)
	if err != nil {
		t.Fatalf("unknown tags caused an error; want skip-don't-fail: %v", err)
	}
	if got.Title != "Still Recovered" {
		t.Errorf("Title = %q; want %q (unknown rows must not block known ones)", got.Title, "Still Recovered")
	}
}

func TestFromFlightRecoversProseTextRows(t *testing.T) {
	got, err := FromFlight(flightRows(), defaultCap)
	if err != nil {
		t.Fatalf("FromFlight errored: %v", err)
	}
	if !strings.Contains(strings.Join(got.BodyTextCandidates, "\n"), "quick brown fox") {
		t.Errorf("prose text row not recovered; candidates: %q", got.BodyTextCandidates)
	}
}

func TestFromFlightGarbageRowsAreSkipped(t *testing.T) {
	rows := []string{
		`not-a-frame-at-all`,
		`:`,
		`7:`,
		`8:not,json[`,
		`9:["$","title",null,{"children":"Survived"}]`,
	}
	got, err := FromFlight(rows, defaultCap)
	if err != nil {
		t.Fatalf("garbage rows caused an error; want skip-don't-fail: %v", err)
	}
	if got.Title != "Survived" {
		t.Errorf("Title = %q; want %q (garbage rows must be skipped)", got.Title, "Survived")
	}
}

func TestFromFlightEmptyIsDegenerate(t *testing.T) {
	got, err := FromFlight(nil, defaultCap)
	if err != nil {
		t.Fatalf("empty flight rows errored: %v", err)
	}
	if got.Decoded {
		t.Errorf("Decoded=true on empty flight; want false")
	}
}

func TestFromFlightOverCapTruncates(t *testing.T) {
	// The combined row length far exceeds an 8-byte cap.
	got, err := FromFlight(flightRows(), 8)
	if err != nil {
		t.Fatalf("over-cap FromFlight errored; want truncation marker: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated=false on over-cap flight; want true")
	}
	if got.Decoded || got.Title != "" {
		t.Errorf("over-cap flight recovered fields: %+v; want zero", got)
	}
}
