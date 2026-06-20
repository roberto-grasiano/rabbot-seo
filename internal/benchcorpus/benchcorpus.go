// Package benchcorpus is a deterministic, dependency-free synthetic-HTML
// generator. It is the shared substrate for the B3 benchmark suite (the
// in-package bench_test.go microbenches and the scheduler recheck bench) and
// the offline capacity harness (scripts/bench/corpussite). It ships only as a
// test/dev dependency — nothing here is reachable from the rabbot binary
// (goreleaser builds only ./cmd/rabbot).
//
// # Why deterministic by construction
//
// A benchmark corpus must be byte-identical across runs and machines, or the
// numbers are not comparable and the golden-SHA pin in TestCorpusIsFixed is
// meaningless. Every byte a page contains is derived from its size Class and
// integer index through fixed word tables and integer arithmetic — there is no
// math/rand, no time.Now, no map iteration, and no environment read anywhere in
// the generation path. Page(class, i) called twice returns identical bytes; the
// same call on any machine returns the same bytes. TestCorpusIsFixed pins the
// SHA-256 of one page per class to a literal hex constant, so ANY edit to the
// word tables or the page template (even one word) flips a SHA and fails the
// test. That falsifiability is the entire point of the "fixed corpus".
//
// # Size classes (documented byte-size targets)
//
// The three classes model the per-page cost distribution a real SEO monitor
// sees, all FAR under the fetcher's 5 MiB body cap (internal/fetcher) so no page
// is ever truncated and the parse benches measure the full document:
//
//   - Landing  (~8 KiB, band 6-12 KiB):  a light marketing page — full <head>
//     metadata, a short hero + a few content sections, a handful of links.
//   - Article  (~60 KiB, band 48-80 KiB): the typical page — a long-form article
//     body. This is the realistic RawHTML the store benches persist, since
//     raw_html is a stored column and the write cost must be honest.
//   - Listing  (~400 KiB, band 300-500 KiB): a heavy, link-heavy index page —
//     hundreds of <a> to OTHER in-corpus pages so extract's link-discovery and
//     internal/external classification do real work.
//
// Each page carries a realistic <head> (title, meta description, meta robots,
// canonical, hreflang alternates, Open Graph, Twitter card, and a JSON-LD block)
// and a body with H1-H6 headings, images (mixed alt/no-alt), and internal +
// external links, so the extract benches exercise REAL field extraction across
// every selector the extractor reads — not an empty shell.
package benchcorpus

import (
	"bytes"
	"strconv"
	"strings"
)

// Class is a corpus page size class. The zero value is Landing.
type Class int

// Corpus size classes. Keep Landing at iota 0 so the zero Class is the light
// page (a caller that forgets to set a class gets the cheapest page, never a
// 400 KiB listing by accident).
const (
	Landing Class = iota // ~8 KiB  light marketing page
	Article              // ~60 KiB typical long-form article
	Listing              // ~400 KiB heavy link-heavy index page
)

// numClasses is the count of defined classes; ClassForIndex cycles through them.
const numClasses = 3

// String returns the lowercase class name. It is used in generated paths and
// titles, so its output is part of the golden bytes — do not change the strings
// without re-pinning TestCorpusIsFixed.
func (c Class) String() string {
	switch c {
	case Article:
		return "article"
	case Listing:
		return "listing"
	default:
		return "landing"
	}
}

// site is the synthetic origin every generated page lives under. It is a
// loopback-style absolute base purely for building canonical/og:url/internal
// link URLs that look real to the extractor; corpussite serves these pages on
// 127.0.0.1 and the bench httptest server rewrites links to its own base as
// needed. The host is a documentation domain (RFC 2606) so nothing here implies
// a real third-party site is fetched.
const site = "https://corpus.example/"

// Path returns the stable site-relative path for a page, e.g. "/article/42".
// The path is deterministic from (class, index) and is how listing pages link
// to other in-corpus pages. corpussite maps an incoming request path back to a
// (class, index) via ParsePath.
func Path(class Class, index int) string {
	return "/" + class.String() + "/" + strconv.Itoa(index)
}

// URL returns the absolute canonical URL for a page under the synthetic site.
func URL(class Class, index int) string {
	return site + class.String() + "/" + strconv.Itoa(index)
}

// ParsePath maps a request path produced by Path back to its (class, index).
// It accepts "/class/index" (e.g. "/article/42"), tolerating an optional
// leading slash and a trailing slash. ok is false for any path that does not
// match the scheme. corpussite uses it to resolve an incoming request to the
// page it should serve. It is the inverse of Path for all valid inputs.
func ParsePath(path string) (class Class, index int, ok bool) {
	p := strings.Trim(path, "/")
	slash := strings.IndexByte(p, '/')
	if slash < 0 {
		return 0, 0, false
	}
	name, rest := p[:slash], p[slash+1:]
	var c Class
	switch name {
	case "landing":
		c = Landing
	case "article":
		c = Article
	case "listing":
		c = Listing
	default:
		return 0, 0, false
	}
	// rest must be a non-negative base-10 integer (no sign, no extra segments).
	if rest == "" || strings.ContainsRune(rest, '/') {
		return 0, 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, 0, false
	}
	return c, n, true
}

// ClassForIndex maps a flat page index (0..N-1, as a corpus-site of N pages
// enumerates) to a size class deterministically. It cycles landing → article →
// listing so a sweep of N pages contains a fixed, repeatable mix of all three
// classes (≈ one third each) regardless of N. This is the index→class helper a
// corpus-site of N pages needs.
func ClassForIndex(index int) Class {
	// Guard against a negative index folding to a negative modulus.
	m := index % numClasses
	if m < 0 {
		m += numClasses
	}
	return Class(m)
}

// wordCountFor is the body word budget per class. These drive the page size into
// its target band; they are part of the golden bytes, so changing one re-pins
// the SHA. Tuned so landing≈8 KiB, article≈60 KiB, listing≈400 KiB (the listing
// size is dominated by its link list, not its prose).
func wordCountFor(class Class) int {
	switch class {
	case Article:
		return 6400 // long-form body → ~60 KiB
	case Listing:
		return 600 // listing prose is short; <a> links carry the weight
	default:
		return 300 // landing → ~8 KiB
	}
}

// linkCountFor is the number of internal in-corpus links a page emits. The
// listing is link-heavy by design (extract's link discovery + internal/external
// classification must do real work); landing/article carry a realistic handful.
func linkCountFor(class Class) int {
	switch class {
	case Listing:
		return 6800 // heavy index → thousands of <a>, ~400 KiB total
	case Article:
		return 24
	default:
		return 12
	}
}

// Page returns one deterministic HTML page for the given class and index.
// Calling it twice with the same arguments returns byte-identical output. The
// returned bytes are a complete HTML5 document with a realistic <head> and a
// body sized to the class's target band (see the package doc and the
// wordCountFor/linkCountFor budgets). A negative index is treated as its
// absolute value so callers never panic on bad input; index 0 is valid.
func Page(class Class, index int) []byte {
	if index < 0 {
		index = -index
	}
	var b bytes.Buffer
	// Pre-size the buffer to the class's rough target so the bench-setup
	// allocation cost is one grow, not many. These are hints only; the exact
	// size is whatever the template produces (pinned by the golden test).
	switch class {
	case Article:
		b.Grow(64 << 10)
	case Listing:
		b.Grow(448 << 10)
	default:
		b.Grow(12 << 10)
	}
	writeHead(&b, class, index)
	writeBody(&b, class, index)
	return b.Bytes()
}
