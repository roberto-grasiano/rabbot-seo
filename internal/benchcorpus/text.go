package benchcorpus

import (
	"bytes"
	"strings"
)

// sentence builds one deterministic sentence of between minWords and maxWords
// words from the fixed word tables. The exact length and the words chosen depend
// only on the integer inputs (seed, salt), so the output is fully deterministic.
// The sentence is grammar-ish (adjective noun verb adjective noun connector …)
// so readability and SimHash see realistic token streams, not a word soup.
func sentence(seed, minWords, maxWords int, salt ...int) string {
	s := abs(seed)
	extra := 0
	for _, x := range salt {
		extra += abs(x)
	}
	span := maxWords - minWords
	if span < 1 {
		span = 1
	}
	n := minWords + (s+extra)%span
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch i % 5 {
		case 0:
			b.WriteString(pick(adjectives, s+extra, i))
		case 1:
			b.WriteString(pick(nouns, s+extra, i))
		case 2:
			b.WriteString(pick(verbs, s+extra, i))
		case 3:
			b.WriteString(pick(adjectives, s+extra, i+1))
		case 4:
			b.WriteString(pick(connectors, s+extra, i))
		}
	}
	// Capitalize the first letter and terminate with a period so the prose reads
	// like sentences (ASCII-only inputs, so byte-0 upper is safe).
	out := b.String()
	if out != "" && out[0] >= 'a' && out[0] <= 'z' {
		out = string(out[0]-32) + out[1:]
	}
	return out + "."
}

// paragraph builds a deterministic paragraph of a fixed number of sentences. The
// sentence-count is fixed (not random) so the body word budget in writeBody is
// predictable; the per-sentence word counts vary with the indices.
func paragraph(seed, para, salt int) string {
	const sentencesPerParagraph = 5
	var b strings.Builder
	for i := 0; i < sentencesPerParagraph; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(sentence(seed, 9, 18, para, salt, i))
	}
	return b.String()
}

// countWords counts whitespace-delimited tokens, matching how the readability
// extractor and SimHash tokenize (strings.Fields). writeBody uses it to spend
// the per-section word budget.
func countWords(s string) int {
	return len(strings.Fields(s))
}

// writeEscaped writes s to b with the four HTML text-context metacharacters
// escaped (&, <, >, and " for attribute safety). The word tables are plain ASCII
// vocabulary with no metacharacters, so in practice this rarely substitutes —
// but escaping unconditionally keeps every generated document well-formed and
// guarantees the bytes can't accidentally inject markup if a table ever gains a
// punctuated entry. It does NOT depend on html.EscapeString so the exact byte
// output is fixed and self-evident.
func writeEscaped(b *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteByte(s[i])
		}
	}
}

// writeJSONString writes s to b as a JSON string literal (including the
// surrounding quotes) with the JSON-mandatory escapes applied. The inputs are
// ASCII prose, so this handles the realistic cases (quote, backslash, control
// bytes) deterministically without pulling in encoding/json — keeping the
// JSON-LD block's bytes fixed and the golden SHA insensitive to stdlib changes.
func writeJSONString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				b.WriteString("\\u00")
				b.WriteByte(hexDigit(c >> 4))
				b.WriteByte(hexDigit(c & 0xf))
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}
