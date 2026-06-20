// Package hydration locates and decodes the embedded framework state payloads
// that meta-framework pages ship in their initial HTML — Next.js __NEXT_DATA__
// (JSON), Nuxt 3 __NUXT_DATA__ (the "devalue" array-with-index-references wire
// format), and React Server Components __next_f flight rows — and recovers the
// SEO signals (title, meta description, canonical, JSON-LD, prose) carried in
// them. It is the single source of truth for payload decoding, shared by the
// crawl-time extractor (which back-fills DOM-empty fields and a content hash)
// and the precheck classifier (which delegates its payload probes here).
//
// Every decoder is bounded and hostile-input-safe by contract: a byte cap that
// returns a Truncated marker rather than allocating an unbounded payload, a
// depth cap and visited-set so cyclic index references terminate instead of
// looping, a degenerate-value honesty rule (scalars and empty containers carry
// no recoverable content, so Decoded stays false and no field is claimed), and
// a volatile-key filter (buildId, *Hash, *Id, timestamps) so a deploy-only
// identifier flip can never churn the recovered content hash. Malformed input
// returns an error and never panics. The package imports only goquery + stdlib;
// it must not import model, extract, or precheck (precheck and extract import
// it).
package hydration

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds on recovered output. These keep the decoders' output bounded
// regardless of input size (an unbounded-output decoder is a DoS vector and a
// fuzz failure): a malicious payload with ten thousand prose-shaped leaves must
// not produce ten thousand candidates.
const (
	// maxProseCandidates caps the number of body-text leaves harvested.
	maxProseCandidates = 64
	// maxJSONLDBlocks caps the number of recovered ld+json blocks.
	maxJSONLDBlocks = 32
	// minProseWords is the word floor a string leaf must clear to be harvested
	// as prose by the long-run branch. It matches precheck's thinness floor
	// (wordFloor=25) so the extract-side content-hash composition shares the
	// exact threshold.
	minProseWords = 25
	// minSentenceWords is the shorter floor for the sentence-like branch: a leaf
	// of at least this many words that ends in terminal punctuation is real body
	// prose even when it falls under minProseWords. It is high enough to reject
	// short UI labels and slugs ("Read more.", "Sign in.").
	minSentenceWords = 8
	// maxWalkDepth bounds recursive structure walks (object/array nesting and
	// devalue index dereferencing) so cyclic or pathologically deep payloads
	// terminate. Real framework payloads are nowhere near this deep.
	maxWalkDepth = 64
	// maxWalkNodes bounds the total number of nodes visited in a single harvest
	// walk, a second guard against pathological fan-out independent of depth.
	maxWalkNodes = 100000
)

// Fields are the SEO signals recovered from a hydration payload. Head fields are
// empty when not present; the caller (extract) merges them DOM-first, so an
// empty field here simply means "nothing to back-fill". The markers report what
// happened: Decoded is true only when a real (non-degenerate) payload was
// parsed; Truncated is true when the payload exceeded the byte cap and was
// skipped (Decoded then stays false and all fields are zero).
type Fields struct {
	// Title is a recovered document title.
	Title string
	// MetaDescription is a recovered meta description.
	MetaDescription string
	// Canonical is a recovered canonical URL (verbatim — the caller resolves it
	// against the page URL, matching how the DOM canonical is resolved).
	Canonical string
	// JSONLD holds raw ld+json blocks recovered from the payload (e.g. RSC
	// script elements), each a standalone JSON document. The caller appends
	// these to the snapshot's JSONLD/SchemaTypes via the existing path.
	JSONLD []json.RawMessage
	// SchemaTypes is an optional convenience list of @type values; the caller
	// may instead derive types from JSONLD with its own helper. Left empty by
	// the decoders today (extract derives types) but reserved for symmetry.
	SchemaTypes []string
	// BodyTextCandidates are prose-like string leaves (each >= minProseWords),
	// volatile-key filtered, that back the content hash only when the DOM main
	// text is below the thinness floor.
	BodyTextCandidates []string
	// Decoded reports whether a real, non-degenerate payload was parsed. A
	// scalar, empty object/array, or over-cap input leaves it false: no
	// recovery is claimed.
	Decoded bool
	// Truncated reports that the payload exceeded the byte cap and was skipped.
	Truncated bool
}

// overCap reports whether raw exceeds the byte cap. A non-positive cap disables
// the check (treated as "no cap") so a caller that wants the cap off can pass 0.
func overCap(n, maxBytes int) bool {
	return maxBytes > 0 && n > maxBytes
}

// volatileKey reports whether an object key is a deploy-only / churning
// identifier whose value must never be harvested as prose or content — a
// buildId flip on an otherwise-identical deploy must not change recovered text.
// The match is case-insensitive and covers exact deploy keys plus the common
// *Id / *Hash / *At suffixes that name identifiers and timestamps.
func volatileKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	switch k {
	case "buildid", "dataroute", "datarouteid", "aspath", "querystring",
		"timestamp", "ts", "hash", "etag", "nonce", "csrf", "csrftoken",
		"requestid", "traceid", "spanid", "sessionid", "deploymentid",
		"revalidate", "rsc", "__n_ssg", "__n_ssp", "gssp", "gsp", "gip":
		return true
	}
	// Suffix families: anything ending in id/hash/at/uuid/token names an
	// identifier/timestamp, not stable prose.
	for _, suf := range []string{"id", "hash", "uuid", "token", "_at", "at", "secret", "key"} {
		if strings.HasSuffix(k, suf) && len(k) > len(suf) {
			// Guard against false positives on real words ("paid", "format")
			// by only treating camelCase/snake boundaries as suffixes: require
			// the char before the suffix to be an uppercase letter (camelCase)
			// or an underscore (snake_case) in the ORIGINAL key.
			if hasSuffixBoundary(key, suf) {
				return true
			}
		}
	}
	return false
}

// hasSuffixBoundary reports whether key ends in suf at a camelCase or snake_case
// boundary (e.g. "buildId"/"build_id" match "id", but "paid" does not). The
// comparison is case-insensitive on the suffix run; the boundary check reads the
// original key's casing.
func hasSuffixBoundary(key, suf string) bool {
	lk := strings.ToLower(key)
	lsuf := strings.ToLower(suf)
	if !strings.HasSuffix(lk, lsuf) {
		return false
	}
	idx := len(key) - len(suf)
	if idx <= 0 {
		return false
	}
	prev := rune(key[idx-1])
	if prev == '_' {
		return true
	}
	// camelCase boundary: the first char of the suffix in the original key is
	// uppercase (e.g. the "I" in "buildId").
	first := rune(key[idx])
	return unicode.IsUpper(first)
}

// looksLikeProse reports whether s reads as human prose worth harvesting rather
// than a URL, hex blob, identifier, or single token. It requires at least
// minProseWords whitespace-separated tokens OR a sentence-like run, rejects
// URL-/hex-/id-like strings, and requires a healthy ratio of letters+spaces.
func looksLikeProse(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	// Reject obvious non-prose: URLs, data URIs, file paths, hex/base64 blobs.
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "/") ||
		strings.HasPrefix(low, "www.") {
		return false
	}
	if !strings.ContainsRune(s, ' ') {
		// A single token with no spaces is an id/slug/hash, not prose.
		return false
	}
	// Letters-and-spaces ratio: prose is mostly letters and spaces, not symbols
	// or digits. A hex blob or minified JS string fails this. Checked before the
	// word-count branches so non-prose (URLs with spaces, base64 with spaces) is
	// rejected regardless of length.
	var letters, total int
	for _, r := range s {
		total++
		if unicode.IsLetter(r) || unicode.IsSpace(r) {
			letters++
		}
	}
	if total == 0 || float64(letters)/float64(total) < 0.75 {
		return false
	}
	words := strings.Fields(s)
	// Prose qualifies two ways (per the decoder contract): a long run of words
	// (>= minProseWords), OR a shorter but clearly sentence-like run — several
	// words ending in terminal punctuation. The sentence branch recovers real
	// one-/two-sentence body leaves that fall just under the word floor without
	// admitting short labels/slugs.
	if len(words) >= minProseWords {
		return true
	}
	return len(words) >= minSentenceWords && endsSentence(s)
}

// endsSentence reports whether s ends with sentence-terminal punctuation
// (optionally followed by a closing quote/bracket), a cheap "looks like a real
// sentence" signal used by the shorter-prose branch of looksLikeProse.
func endsSentence(s string) bool {
	s = strings.TrimRight(strings.TrimSpace(s), `"')]}`)
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '.', '!', '?':
		return true
	default:
		return false
	}
}

// proseSink accumulates prose candidates with de-duplication and the count
// bound, so a payload that repeats the same leaf or carries thousands of leaves
// cannot produce unbounded or duplicate output.
type proseSink struct {
	seen map[string]struct{}
	out  []string
}

func newProseSink() *proseSink { return &proseSink{seen: map[string]struct{}{}} }

// sorted returns the accumulated candidates in a deterministic (lexical) order.
// The harvest walks Go maps, whose iteration order is randomized, so the raw
// accumulation order is non-deterministic; the extract-side content hash is
// composed from these candidates, and a non-deterministic order would churn the
// hash and emit false `content` change alerts on an otherwise-identical page.
// Sorting makes the recovered prose a stable function of the payload content.
func (p *proseSink) sorted() []string {
	if len(p.out) == 0 {
		return nil
	}
	out := make([]string, len(p.out))
	copy(out, p.out)
	sort.Strings(out)
	return out
}

// add records a prose candidate if it is novel, prose-like, and under the count
// bound. It reports whether the sink is now full (so a walk can stop early).
func (p *proseSink) add(s string) (full bool) {
	if len(p.out) >= maxProseCandidates {
		return true
	}
	s = strings.TrimSpace(s)
	if !looksLikeProse(s) {
		return false
	}
	if _, dup := p.seen[s]; dup {
		return false
	}
	p.seen[s] = struct{}{}
	p.out = append(p.out, s)
	return len(p.out) >= maxProseCandidates
}
