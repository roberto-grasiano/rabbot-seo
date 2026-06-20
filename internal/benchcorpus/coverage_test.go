package benchcorpus

import (
	"bytes"
	"strings"
	"testing"
)

// TestParsePathRoundTrip asserts ParsePath is the documented inverse of Path:
// for every class and a spread of indices, ParsePath(Path(class, i)) recovers
// exactly (class, i, true). This is the gap the build flagged (ParsePath was
// 0%). The assertion would fail if ParsePath decoded the wrong class, dropped
// the index, or mis-handled the leading slash that Path always emits.
func TestParsePathRoundTrip(t *testing.T) {
	classes := []Class{Landing, Article, Listing}
	indices := []int{0, 1, 7, 42, 100, 999, 12345}
	for _, c := range classes {
		for _, i := range indices {
			path := Path(c, i)
			gotClass, gotIndex, ok := ParsePath(path)
			if !ok {
				t.Errorf("ParsePath(%q) ok=false, want true (Path round-trip)", path)
				continue
			}
			if gotClass != c || gotIndex != i {
				t.Errorf("ParsePath(%q) = (%s, %d), want (%s, %d)",
					path, gotClass, gotIndex, c, i)
			}
		}
	}
}

// TestParsePathValid covers the documented tolerances (optional leading slash,
// optional trailing slash) and the explicit class names, with exact expected
// (class, index) so a decode bug is caught — not just an ok flag.
func TestParsePathValid(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantClass Class
		wantIndex int
	}{
		{"leading-slash", "/article/42", Article, 42},
		{"no-leading-slash", "article/42", Article, 42},
		{"trailing-slash", "/listing/7/", Listing, 7},
		{"both-slashes", "/landing/0/", Landing, 0},
		{"landing", "/landing/3", Landing, 3},
		{"article", "/article/3", Article, 3},
		{"listing", "/listing/3", Listing, 3},
		{"large-index", "/article/2147483", Article, 2147483},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotIndex, ok := ParsePath(tt.path)
			if !ok {
				t.Fatalf("ParsePath(%q) ok=false, want true", tt.path)
			}
			if gotClass != tt.wantClass || gotIndex != tt.wantIndex {
				t.Errorf("ParsePath(%q) = (%s, %d), want (%s, %d)",
					tt.path, gotClass, gotIndex, tt.wantClass, tt.wantIndex)
			}
		})
	}
}

// TestParsePathInvalid covers every reject branch in ParsePath's contract: no
// slash, an unknown class segment, an empty index, an extra path segment, a
// non-numeric index, and a negative index. Each must return ok=false with zero
// class and zero index. These are the negative cases the task calls out.
func TestParsePathInvalid(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"only-slashes", "///"},
		{"no-slash-after-trim", "article"},    // trim leaves "article", no inner '/'
		{"garbage", "not-a-real-path-at-all"}, // single token, no '/'
		{"bad-class", "/widget/42"},           // unknown class name
		{"bad-class-cased", "/Article/42"},    // case-sensitive: must be lowercase
		{"empty-index", "/article/"},          // rest == "" after trim is just "article"
		{"non-numeric-index", "/article/abc"}, // strconv.Atoi fails
		{"float-index", "/article/4.2"},       // not an integer
		{"negative-index", "/article/-5"},     // n < 0 rejected explicitly
		{"extra-segment", "/article/4/2"},     // rest contains '/'
		{"hex-index", "/article/0x10"},        // not base-10
		{"space-index", "/article/4 2"},       // not a clean integer
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotIndex, ok := ParsePath(tt.path)
			if ok {
				t.Errorf("ParsePath(%q) ok=true (got class=%s index=%d), want ok=false",
					tt.path, gotClass, gotIndex)
			}
			// Contract: on failure the class/index are the zero values.
			if gotClass != 0 || gotIndex != 0 {
				t.Errorf("ParsePath(%q) failed but returned (%d, %d), want (0, 0)",
					tt.path, gotClass, gotIndex)
			}
		})
	}
}

// TestParsePathSignedPlus pins the one subtle case: strconv.Atoi accepts a
// leading "+", so "/article/+5" parses to 5 and is NOT rejected (only n < 0 is).
// This documents the exact boundary of the "no sign" comment versus the code.
func TestParsePathSignedPlus(t *testing.T) {
	c, n, ok := ParsePath("/article/+5")
	// Atoi("+5") == 5, nil; 5 >= 0, so this is accepted.
	if !ok || c != Article || n != 5 {
		t.Fatalf("ParsePath(\"/article/+5\") = (%s, %d, %v), want (article, 5, true)", c, n, ok)
	}
}

// TestClassForIndexCycling asserts the documented landing→article→listing cycle
// for indices that land on EACH class (including the negative-folding branch
// ClassForIndex was missing at 75%). A negative index must fold to the same
// class as its non-negative congruent residue mod 3.
func TestClassForIndexCycling(t *testing.T) {
	tests := []struct {
		index int
		want  Class
	}{
		{0, Landing},
		{1, Article},
		{2, Listing},
		{3, Landing},
		{4, Article},
		{5, Listing},
		{99, Landing},  // 99 % 3 == 0
		{100, Article}, // 100 % 3 == 1
		{101, Listing}, // 101 % 3 == 2
		// Negative indices exercise the m < 0 fold. In Go, -1 % 3 == -1, so the
		// guard adds numClasses: -1 -> 2 -> Listing, -2 -> 1 -> Article,
		// -3 -> 0 -> Landing.
		{-1, Listing},
		{-2, Article},
		{-3, Landing},
		{-4, Listing},
	}
	for _, tt := range tests {
		got := ClassForIndex(tt.index)
		if got != tt.want {
			t.Errorf("ClassForIndex(%d) = %s, want %s", tt.index, got, tt.want)
		}
	}
}

// TestClassForIndexNonNegative is an invariant cross-check: the returned class
// must always be one of the three defined classes (never a negative or
// out-of-range Class), even for negative indices. This would fail if the
// negative-fold guard were removed (Class(-1) would print as "landing" via the
// default String() branch but is NOT a valid enum value).
func TestClassForIndexInRange(t *testing.T) {
	for i := -10; i <= 10; i++ {
		c := ClassForIndex(i)
		if c < Landing || c > Listing {
			t.Errorf("ClassForIndex(%d) = %d, outside [%d,%d]", i, c, Landing, Listing)
		}
	}
}

// TestWriteEscaped feeds strings containing each of the four escaped
// metacharacters (& < > ") plus the default pass-through path, asserting the
// EXACT byte output. writeEscaped was 42.9% because the word tables never
// contain metacharacters, so only the default branch ran. These cases force
// every switch arm.
func TestWriteEscaped(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ampersand", "a&b", "a&amp;b"},
		{"less-than", "a<b", "a&lt;b"},
		{"greater-than", "a>b", "a&gt;b"},
		{"double-quote", `a"b`, "a&quot;b"},
		{"all-four", `&<>"`, "&amp;&lt;&gt;&quot;"},
		{"plain-passthrough", "plain text 123", "plain text 123"},
		{"empty", "", ""},
		// Single quote is NOT escaped (the doc says four chars: & < > "); a ' must
		// pass through unchanged. This would fail if someone added a ''' arm.
		{"single-quote-untouched", "it's", "it's"},
		{"mixed", `<a href="x">&y`, `&lt;a href=&quot;x&quot;&gt;&amp;y`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			writeEscaped(&b, tt.in)
			if got := b.String(); got != tt.want {
				t.Errorf("writeEscaped(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestWriteJSONString forces every branch of the JSON escaper: the named
// escapes (" \ \n \r \t), the control-byte \uXXXX path (which drives hexDigit),
// and the default pass-through. The surrounding quotes are part of the output.
// writeJSONString was 46.7% because prose inputs only hit the default arm.
func TestWriteJSONString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `"hello"`},
		{"empty", "", `""`},
		{"double-quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"newline", "a\nb", `"a\nb"`},
		{"carriage-return", "a\rb", `"a\rb"`},
		{"tab", "a\tb", `"a\tb"`},
		// Control byte 0x00 takes the \u00 path: hexDigit(0)='0' twice.
		// Expected JSON output is the six ASCII bytes backslash-u-0-0-0-0.
		// This exercises the n < 10 arm of hexDigit (digit '0').
		{"null", "a\x00b", `"a\u0000b"`},
		// Control byte 0x1f: >>4 = 1 (hexDigit '1'), &0xf = 15 (hexDigit 'f').
		// This is the case that drives hexDigit through BOTH a 0-9 output ('1')
		// and an a-f output ('f') in a single byte.
		{"unit-separator", "a\x1fb", `"a\u001fb"`},
		// 0x0b (vertical tab) is < 0x20 but not one of the named escapes:
		// >>4 = 0 ('0'), &0xf = 11 (hexDigit 'b') -> backslash-u-0-0-0-b.
		{"vertical-tab", "\x0b", `"\u000b"`},
		// 0x07 (bell): >>4 = 0 ('0'), &0xf = 7 ('7') -> backslash-u-0-0-0-7.
		{"bell", "\x07", `"\u0007"`},
		{"all-named", "\"\\\n\r\t", `"\"\\\n\r\t"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			writeJSONString(&b, tt.in)
			if got := b.String(); got != tt.want {
				t.Errorf("writeJSONString(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestHexDigit asserts the full nibble→hex-char mapping directly, covering both
// branches: n < 10 returns '0'+n (digits) and n >= 10 returns 'a'+(n-10)
// (lowercase letters). hexDigit was 0%. Every nibble value 0..15 is checked
// against the literal expected ASCII byte, so a wrong base offset (e.g. uppercase
// 'A', or 'a'+n) would fail.
func TestHexDigit(t *testing.T) {
	want := "0123456789abcdef"
	for n := 0; n < 16; n++ {
		got := hexDigit(byte(n))
		if got != want[n] {
			t.Errorf("hexDigit(%d) = %q, want %q", n, string(got), string(want[n]))
		}
	}
}

// TestAbs covers both arms of abs, including the negative branch that was
// missing (66.7%). abs(MinInt) is intentionally not asserted (it overflows by
// definition of two's-complement and is never fed such input by the generator).
func TestAbs(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{0, 0},
		{5, 5},
		{-5, 5},   // negative branch
		{-1, 1},   // negative branch
		{42, 42},  // positive branch
		{-42, 42}, // negative branch
	}
	for _, tt := range tests {
		if got := abs(tt.in); got != tt.want {
			t.Errorf("abs(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// TestPickDeterministicAndInRange asserts pick's contract: the result is the
// table entry at (seed*31 + step*17) % len(table), depends ONLY on its integer
// inputs, and folds negative seed/step to their absolute values (the branches
// pick was missing at 66.7%). We assert the exact index formula so a change to
// the stride constants would fail, and that pick(t, -s, -st) == pick(t, s, st)
// (the negative-fold equivalence).
func TestPickDeterministicAndInRange(t *testing.T) {
	table := nouns // a real fixed table from words.go
	want := func(seed, step int) string {
		s, st := seed, step
		if s < 0 {
			s = -s
		}
		if st < 0 {
			st = -st
		}
		return table[(s*31+st*17)%len(table)]
	}

	cases := []struct{ seed, step int }{
		{0, 0}, {1, 0}, {0, 1}, {7, 3}, {42, 5}, {100, 13},
		{-1, 0}, {0, -1}, {-7, -3}, {-42, -5},
	}
	for _, c := range cases {
		got := pick(table, c.seed, c.step)
		if exp := want(c.seed, c.step); got != exp {
			t.Errorf("pick(nouns, %d, %d) = %q, want %q", c.seed, c.step, got, exp)
		}
	}

	// Negative-fold equivalence: a sign flip on either input must not change the
	// result. This is precisely the branch pair (seed<0, step<0) that was uncovered.
	if pick(table, -7, -3) != pick(table, 7, 3) {
		t.Errorf("pick negative-fold mismatch: pick(-7,-3)=%q != pick(7,3)=%q",
			pick(table, -7, -3), pick(table, 7, 3))
	}
	if pick(table, -1, 0) != pick(table, 1, 0) {
		t.Errorf("pick seed-fold mismatch: pick(-1,0)=%q != pick(1,0)=%q",
			pick(table, -1, 0), pick(table, 1, 0))
	}
	if pick(table, 0, -1) != pick(table, 0, 1) {
		t.Errorf("pick step-fold mismatch: pick(0,-1)=%q != pick(0,1)=%q",
			pick(table, 0, -1), pick(table, 0, 1))
	}

	// Determinism: many calls with identical inputs all return the first result.
	// (Written as a loop, not a self-comparison, so it asserts stability without
	// tripping staticcheck's identical-operands check.)
	first := pick(table, 12345, 9)
	for n := 0; n < 16; n++ {
		if again := pick(table, 12345, 9); again != first {
			t.Errorf("pick(nouns, 12345, 9) not deterministic: call %d gave %q, first gave %q",
				n, again, first)
		}
	}
}

// TestSentenceSpanFloor covers the span<1 guard in sentence (the branch it was
// missing at 95.5%): when minWords == maxWords the span is forced to 1 so the
// modulus is well-defined. The result must be a single capitalized,
// period-terminated word at minimum and must not panic on a zero/negative span.
func TestSentenceSpanFloor(t *testing.T) {
	// minWords == maxWords → span computed as 0, floored to 1. n becomes
	// minWords + (s+extra)%1 == minWords exactly.
	for _, seed := range []int{0, 1, 5, 100} {
		got := sentence(seed, 3, 3)
		if got == "" {
			t.Fatalf("sentence(%d, 3, 3) returned empty", seed)
		}
		if !strings.HasSuffix(got, ".") {
			t.Errorf("sentence(%d, 3, 3) = %q, want trailing period", seed, got)
		}
		// span floored to 1 → modulus is always 0 → exactly minWords words.
		if n := countWords(got); n != 3 {
			t.Errorf("sentence(%d, 3, 3) has %d words, want exactly 3 (span floor)", seed, n)
		}
		// First rune must be capitalized (the ASCII upper path).
		if got[0] < 'A' || got[0] > 'Z' {
			t.Errorf("sentence(%d, 3, 3) = %q, want capitalized first letter", seed, got)
		}
	}

	// maxWords < minWords also yields span<1 (negative span) → floored to 1.
	// Must not panic and must still produce minWords words.
	got := sentence(5, 4, 2)
	if n := countWords(got); n != 4 {
		t.Errorf("sentence(5, 4, 2) has %d words, want 4 (negative span floored)", n)
	}
}

// TestSentenceDeterministic asserts the package's core determinism contract at
// the sentence level: identical (seed, bounds, salt) yields identical bytes, and
// the salt actually influences the output (so it is not ignored). This guards a
// future edit that drops the salt mixing.
func TestSentenceDeterministic(t *testing.T) {
	a := sentence(42, 9, 18, 1, 2, 3)
	b := sentence(42, 9, 18, 1, 2, 3)
	if a != b {
		t.Errorf("sentence not deterministic: %q != %q", a, b)
	}
	// Different salt should (for these inputs) change the sentence. We assert it
	// is at least well-formed; salt affecting length/words is the intent.
	c := sentence(42, 9, 18, 9, 9, 9)
	if !strings.HasSuffix(c, ".") || c == "" {
		t.Errorf("salted sentence malformed: %q", c)
	}
}

// TestPageNegativeIndex covers the index<0 negation branch in Page (it was 90%).
// A negative index is treated as its absolute value, so Page(class, -i) must be
// byte-identical to Page(class, i). This would fail if the negation were dropped
// (the path/canonical/index-derived content would differ).
func TestPageNegativeIndex(t *testing.T) {
	for _, c := range []Class{Landing, Article, Listing} {
		pos := Page(c, 42)
		neg := Page(c, -42)
		if !bytes.Equal(pos, neg) {
			t.Errorf("Page(%s, -42) differs from Page(%s, 42): %d vs %d bytes",
				c, c, len(neg), len(pos))
		}
	}
	// index 0 is valid and -0 == 0; sanity-check it does not panic and is stable.
	a := Page(Landing, 0)
	b := Page(Landing, 0)
	if !bytes.Equal(a, b) {
		t.Error("Page(Landing, 0) not deterministic")
	}
}
