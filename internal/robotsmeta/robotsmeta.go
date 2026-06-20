// Package robotsmeta is the single, dependency-free canonical parser for HTML
// meta-robots / X-Robots-Tag directive values. It is a leaf package so any
// consumer (the extractor's indexability verdict, the rules engine) can share
// ONE tokenization of these values instead of re-implementing a slightly
// different split each time and drifting.
//
// A robots directive value is a list of directive tokens. Google documents
// three real-world separators: comma ("noindex, nofollow"), and within an
// X-Robots-Tag header an optional leading "<user-agent>:" prefix
// ("googlebot: noindex") where the first colon-delimited token names the bot,
// not a directive. Some valued directives also use a colon internally
// ("max-snippet:-1"). To handle all of these uniformly the parser splits on
// commas, colons, and ASCII whitespace, lowercases every token, and (only when
// the leading token is NOT itself a known directive) drops that leading token
// as a user-agent name.
package robotsmeta

import "strings"

// knownDirectives is the set of bare (valueless) robots directives the parser
// recognizes as directives rather than user-agent names. It is used solely to
// decide whether a leading token is a "<user-agent>:" prefix to strip: if the
// first token is one of these, it is a real directive and is kept. The set is
// deliberately generous so a value like "noindex: nofollow" (colon used as a
// separator) keeps its leading "noindex".
var knownDirectives = map[string]struct{}{
	"all":               {},
	"none":              {},
	"index":             {},
	"noindex":           {},
	"follow":            {},
	"nofollow":          {},
	"noarchive":         {},
	"nosnippet":         {},
	"noimageindex":      {},
	"notranslate":       {},
	"nocache":           {},
	"indexifembedded":   {},
	"max-snippet":       {},
	"max-image-preview": {},
	"max-video-preview": {},
	"unavailable_after": {},
}

// Parse tokenizes a robots directive value into its lowercased directive
// tokens, in order, stripping an optional leading "<user-agent>:" prefix.
//
// The value is split on commas, colons, and ASCII whitespace; empty fields are
// dropped. If the FIRST resulting token is not a known directive, it is treated
// as a user-agent name (the documented "googlebot: noindex" form) and dropped.
// Returns nil for an empty/whitespace value.
func Parse(value string) []string {
	tokens := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', ':', ' ', '\t', '\n', '\r', '\f', '\v':
			return true
		default:
			return false
		}
	})
	if len(tokens) == 0 {
		return nil
	}
	for i := range tokens {
		tokens[i] = strings.ToLower(tokens[i])
	}
	// Strip a leading user-agent token: only when the first token is NOT itself
	// a known directive (so "noindex: nofollow" keeps noindex, but
	// "googlebot: noindex" drops googlebot). After stripping, if nothing is
	// left the value carried no directives.
	if _, ok := knownDirectives[tokens[0]]; !ok {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// IsNoindex reports whether a robots directive value removes the page from the
// index. The token "noindex" deindexes; per Google's robots-meta spec "none" is
// equivalent to "noindex, nofollow", so it deindexes too. Matching is
// token-exact (after Parse), so "noindexible" does not false-positive the way a
// substring match would.
func IsNoindex(value string) bool {
	for _, tok := range Parse(value) {
		switch tok {
		case "noindex", "none":
			return true
		}
	}
	return false
}

// HasToken reports whether a robots directive value contains the given bare
// directive token (case-insensitive). Because "none" expands to
// "noindex, nofollow", HasToken(value, "noindex") and HasToken(value,
// "nofollow") both report true when the value contains "none". The wanted token
// is matched token-exact against the parsed (user-agent-stripped, lowercased)
// tokens.
func HasToken(value, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	for _, tok := range Parse(value) {
		if tok == want {
			return true
		}
		// "none" implies both noindex and nofollow.
		if tok == "none" && (want == "noindex" || want == "nofollow") {
			return true
		}
	}
	return false
}
