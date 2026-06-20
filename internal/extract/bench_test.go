package extract

// B3 microbenchmarks over the per-page extraction hot path.
//
// These benches feed the deterministic benchcorpus pages (Landing/Article/
// Listing) through the SHIPPED extraction path so docs/PERFORMANCE.md can quote
// honest per-page CPU/alloc costs for the static binary. They are smoke-run with
//
//	CGO_ENABLED=0 go test -run '^$' -bench . -benchtime=1x ./internal/extract/...
//
// (CGO_ENABLED=0 so the numbers reflect the pure-Go static build the project
// ships — never the CGO_ENABLED=1 -race binary). Every bench calls
// b.ReportAllocs() so the alloc/op column is populated.
//
// # F53: the double-parse this bench measures (and does NOT fix)
//
// BenchmarkExtract deliberately exercises the known F53 double-parse. Extract
// (→ extractor.ExtractWith) parses the body ONCE via goquery
// (extract.go: goquery.NewDocumentFromReader) for the head/link/heading/JSON-LD
// selectors, then calls MainText which parses the SAME body a SECOND time
// (maintext.go: htmlparse.Parse on the readability path, or goquery on the
// CSS-selector path) for the main-text/SimHash/SHA. So Extract performs two full
// HTML parses per page on the readability path. B3 QUANTIFIES that overhead and
// publishes the measured share + a fast-follow note (decision #15); it does not
// optimize it here — no behavior change. BenchmarkMainText isolates parse #2 in
// both its branches (readability vs selector) so the share is attributable, and
// BenchmarkSimHash / BenchmarkContentSHA256 isolate the post-parse hashing cost
// (run over already-extracted text, never over HTML).
//
// The extract path benched here is the SHIPPED M1 default: Extractor.Extract,
// which threads Options{Hydration:{Enabled:false}} — i.e. A8 hydration recovery
// OFF (byte-identical to pre-A8). The daemon's actual hydration default comes
// from crawler.hydration.* config, not a code constant; the OFF arm is the
// honest baseline, so that is what these benches measure.

import (
	"net/http"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
)

// benchResult wraps a deterministic benchcorpus page in a fetcher.Result shaped
// exactly as the crawl pipeline hands one to Extract: a 200 text/html response
// with the canonical FinalURL the corpus assigns the page. Content-Type is set
// explicitly to text/html so the extractor's Content-Type gate is unambiguous
// (an absent type is treated as HTML, but we pin it for clarity). The body is a
// benchcorpus class page, FAR under the fetcher's 5 MiB cap, so Extract parses
// the full document and the bench measures the real parse, not a truncated
// prefix.
func benchResult(class benchcorpus.Class, index int) fetcher.Result {
	return fetcher.Result{
		FinalURL:   benchcorpus.URL(class, index),
		HTTPStatus: 200,
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       benchcorpus.Page(class, index),
		FetchedAt:  time.Unix(1_700_000_000, 0).UTC(),
	}
}

// benchExtractIndex is the fixed page index every bench uses so a single page's
// bytes are exercised per size class (the corpus is deterministic; any fixed
// index gives a repeatable page). It matches the golden index pinned in
// benchcorpus_test.go for consistency, though any non-negative value works.
const benchExtractIndex = 42

// BenchmarkExtract measures the full per-page extraction cost across the three
// corpus size classes via the SHIPPED Extract entry point (hydration OFF). This
// is the bench that exercises the F53 double-parse (see the file-level comment):
// each iteration parses the page twice (goquery for the selectors, then
// htmlparse inside MainText for the readability main-text). The result/links are
// blackholed so the compiler cannot elide the work.
func BenchmarkExtract(b *testing.B) {
	ex := NewExtractor()
	classes := []struct {
		name  string
		class benchcorpus.Class
	}{
		{"small", benchcorpus.Landing},   // ~8 KiB light marketing page
		{"typical", benchcorpus.Article}, // ~60 KiB long-form article
		{"heavy", benchcorpus.Listing},   // ~400 KiB link-heavy index page
	}
	for _, tc := range classes {
		res := benchResult(tc.class, benchExtractIndex)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				snap, links, err := ex.Extract(res, "")
				if err != nil {
					b.Fatalf("Extract(%s) error = %v", tc.name, err)
				}
				benchSnapSink = snap
				benchLinksSink = links
			}
		})
	}
}

// BenchmarkMainText isolates the SECOND parse (F53 parse #2) in both of its
// branches so docs/PERFORMANCE.md can attribute the main-text share of Extract:
//
//   - readability: contentSelector=="" → x/net/html parse + readability extraction
//     (maintext.go htmlparse.Parse path). This is the default the daemon uses
//     unless a per-site content_selector is configured.
//   - selector: a CSS selector that matches the corpus body wrapper ("main") →
//     goquery parse + Find(selector).Text() (the per-site override path).
//
// Both run over the typical (Article ~60 KiB) page so the two branch costs are
// directly comparable on identical input.
func BenchmarkMainText(b *testing.B) {
	page := benchcorpus.Page(benchcorpus.Article, benchExtractIndex)
	pageURL := benchcorpus.URL(benchcorpus.Article, benchExtractIndex)

	// Sanity-guard the selector arm: the corpus body is wrapped in <main>, so
	// "main" must select non-empty text. A bench that silently selected nothing
	// would publish a fake-cheap "selector" number. This is the falsifiable check
	// that the selector path actually does work (it runs once, outside b.N).
	if got, err := MainText(pageURL, page, "main"); err != nil || len(got) == 0 {
		b.Fatalf("selector arm precondition failed: MainText(main) err=%v len=%d (corpus body must be non-empty under <main>)", err, len(got))
	}
	// Likewise guard the readability arm produces text, so neither arm is a no-op.
	if got, err := MainText(pageURL, page, ""); err != nil || len(got) == 0 {
		b.Fatalf("readability arm precondition failed: MainText(\"\") err=%v len=%d", err, len(got))
	}

	b.Run("readability", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			text, err := MainText(pageURL, page, "")
			if err != nil {
				b.Fatalf("MainText(readability) error = %v", err)
			}
			benchTextSink = text
		}
	})

	b.Run("selector", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			text, err := MainText(pageURL, page, "main")
			if err != nil {
				b.Fatalf("MainText(selector) error = %v", err)
			}
			benchTextSink = text
		}
	})
}

// BenchmarkSimHash measures the SimHash cost over a TYPICAL page's extracted
// main text (not its HTML): the bench derives the text once via MainText (the
// same text the extractor would hash) and times SimHash over that string, so the
// number reflects the hashing work on real article prose, isolated from parsing.
func BenchmarkSimHash(b *testing.B) {
	text := benchTypicalText(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchUint64Sink = SimHash(text)
	}
}

// BenchmarkContentSHA256 measures the content-hash cost over the SAME typical
// extracted text (hashing the text, not the HTML), isolated from parsing.
func BenchmarkContentSHA256(b *testing.B) {
	text := benchTypicalText(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchStringSink = ContentSHA256(text)
	}
}

// benchTypicalText extracts the main text of a typical (Article) corpus page
// once, the way the extractor would, so the hashing benches run over realistic
// article prose. It fails the bench if the text is empty (which would make the
// hash benches measure hashing an empty string — a meaningless, fake-cheap
// number), keeping them falsifiable.
func benchTypicalText(b *testing.B) string {
	b.Helper()
	page := benchcorpus.Page(benchcorpus.Article, benchExtractIndex)
	pageURL := benchcorpus.URL(benchcorpus.Article, benchExtractIndex)
	text, err := MainText(pageURL, page, "")
	if err != nil {
		b.Fatalf("benchTypicalText: MainText error = %v", err)
	}
	if len(text) == 0 {
		b.Fatalf("benchTypicalText: extracted text is empty; the hash benches would measure nothing")
	}
	return text
}

// Package-level sinks defeat dead-code elimination: assigning each bench's result
// to an exported-scope variable keeps the compiler from optimizing the measured
// call away. They are written-only (never read), which is the standard Go
// benchmark idiom for blackholing results.
var (
	benchSnapSink   any
	benchLinksSink  []string
	benchTextSink   string
	benchStringSink string
	benchUint64Sink uint64
)
