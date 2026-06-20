package serpwidth

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// titleFontPx mirrors the desktop SERP title font size (Arial 20px) that the
// rules package applies its 580px budget against. Kept local so this package
// stays pure mechanism with no policy import.
const titleFontPx = 20

// TestWidthGoldenASCII is the golden pin: Width of a fixed ASCII string at 20px
// must equal the value hand-summed from the advance table. It catches any silent
// regression in the metrics table (a single changed advance shifts the total).
func TestWidthGoldenASCII(t *testing.T) {
	const s = "Hello, World!"
	// Hand-summed design-unit advances for each rune (Arial hmtx, unitsPerEm 2048):
	//   H=1479 e=1139 l=455 l=455 o=1139 ,=569 (space)=569 W=1933 o=1139 r=682 l=455 d=1139 !=569
	units := 1479 + 1139 + 455 + 455 + 1139 + 569 + 569 + 1933 + 1139 + 682 + 455 + 1139 + 569
	want := float64(units) * float64(titleFontPx) / float64(UnitsPerEm)

	got := Width(s, titleFontPx)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Width(%q, %d) = %v, want %v (units=%d)", s, titleFontPx, got, want, units)
	}
}

// TestCharCountIsWrong is the thesis of A3: pixel width, not character count,
// decides SERP fit. 70 narrow runes ('i') fit the 580px title budget while only
// 48 wide runes ('W') blow past it — impossible under any "all chars equal"
// table. The 48-'W' figure is also the spec's worked example (≈906px).
func TestCharCountIsWrong(t *testing.T) {
	const titleBudgetPx = 580.0 // local copy of the policy budget; see rules pkg.

	narrow := Width(strings.Repeat("i", 70), titleFontPx)
	if narrow >= titleBudgetPx {
		t.Errorf("70×'i' = %vpx, want < %vpx (narrow chars must fit)", narrow, titleBudgetPx)
	}

	wide := Width(strings.Repeat("W", 48), titleFontPx)
	if wide <= titleBudgetPx {
		t.Errorf("48×'W' = %vpx, want > %vpx (wide chars must clip)", wide, titleBudgetPx)
	}

	// The narrow string has MORE characters yet LESS width — the whole point.
	if narrow >= wide {
		t.Errorf("70×'i' (%vpx) should be narrower than 48×'W' (%vpx)", narrow, wide)
	}

	// Pin the worked example from the spec: 48×'W' ≈ 906px.
	if r := math.Round(wide); r != 906 {
		t.Errorf("48×'W' rounded = %v, want 906 (spec worked example)", r)
	}
}

// TestWidthEmpty: the empty string measures exactly 0.
func TestWidthEmpty(t *testing.T) {
	if got := Width("", titleFontPx); got != 0 {
		t.Errorf("Width(\"\", %d) = %v, want 0", titleFontPx, got)
	}
}

// TestWhitespaceCollapse: runs of Unicode whitespace collapse to a single space
// before measuring, because HTML rendering collapses them but extract only trims
// the ends. "a \t  b" must therefore measure identically to "a b".
func TestWhitespaceCollapse(t *testing.T) {
	collapsed := Width("a b", titleFontPx)
	tests := []string{
		"a \t  b",
		"a\n\nb",
		"a  b", // NBSP runs collapse too
		"a    b",
		"a\r\n\tb",
	}
	for _, in := range tests {
		if got := Width(in, titleFontPx); math.Abs(got-collapsed) > 1e-9 {
			t.Errorf("Width(%q) = %v, want %v (== Width(%q))", in, got, collapsed, "a b")
		}
	}

	// Leading/trailing whitespace is also normalized away (collapsed to a single
	// space at the edge), matching how the browser lays out a trimmed text node.
	// We assert the interior-collapse invariant above is the load-bearing one;
	// edge whitespace is covered by the trim in extract upstream, but a single
	// leading/trailing space here must not change an interior-only measurement
	// pathologically — verify "  a b  " collapses its interior identically.
	if got := Width("a  b", titleFontPx); math.Abs(got-collapsed) > 1e-9 {
		t.Errorf("thin-space run: Width = %v, want %v", got, collapsed)
	}
}

// TestUnknownRuneFallback: a rune absent from the table contributes exactly
// FallbackAdvance design units. We compare a known glyph against the same glyph
// followed by an unknown PUA rune and require the delta to equal the fallback.
func TestUnknownRuneFallback(t *testing.T) {
	base := Width("A", titleFontPx)
	withUnknown := Width("A", titleFontPx) // U+E000 Private Use Area: never in the table
	delta := withUnknown - base
	wantDelta := float64(FallbackAdvance) * float64(titleFontPx) / float64(UnitsPerEm)
	if math.Abs(delta-wantDelta) > 1e-9 {
		t.Errorf("unknown-rune delta = %v, want %v (FallbackAdvance=%d)", delta, wantDelta, FallbackAdvance)
	}
	if FallbackAdvance <= 0 {
		t.Errorf("FallbackAdvance = %d, want a positive average Latin advance", FallbackAdvance)
	}
}

// TestCJKRuneIsOneEm: a CJK/fullwidth rune occupies one full em (UnitsPerEm),
// the honest fallback for ideographic glyphs we don't carry per-rune metrics for.
func TestCJKRuneIsOneEm(t *testing.T) {
	base := Width("A", titleFontPx)
	withCJK := Width("A中", titleFontPx) // U+4E2D, CJK Unified Ideograph
	delta := withCJK - base
	wantDelta := float64(UnitsPerEm) * float64(titleFontPx) / float64(UnitsPerEm) // == fontPx
	if math.Abs(delta-wantDelta) > 1e-9 {
		t.Errorf("CJK delta = %v, want %v (one em)", delta, wantDelta)
	}
	if math.Abs(delta-float64(titleFontPx)) > 1e-9 {
		t.Errorf("one em at %dpx should equal %vpx, got %v", titleFontPx, float64(titleFontPx), delta)
	}
}

// TestCombiningMarkIsZero: a combining mark (unicode.Mn) adds zero advance — it
// stacks on the preceding base glyph rather than advancing the pen.
func TestCombiningMarkIsZero(t *testing.T) {
	base := Width("e", titleFontPx)
	withMark := Width("é", titleFontPx) // U+0301 COMBINING ACUTE ACCENT
	if math.Abs(withMark-base) > 1e-9 {
		t.Errorf("combining mark changed width: base=%v with-mark=%v (want equal)", base, withMark)
	}
}

// TestEmojiNoPanicNonNegative: an emoji (outside the table, not CJK, not a
// combining mark) is measured via the fallback and must produce a finite,
// non-negative, non-panicking result. Emoji width in real fonts varies; we only
// promise honesty + safety, never pixel-accuracy for pictographs.
func TestEmojiNoPanic(t *testing.T) {
	got := Width("hi 👍🏽 there", titleFontPx)
	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("emoji width not finite: %v", got)
	}
	if got < 0 {
		t.Fatalf("emoji width negative: %v", got)
	}
}

// TestEmojiIsOneEm: a wide pictographic emoji occupies one full em (UnitsPerEm),
// the same honest fallback as a CJK ideograph — NOT the ~average-Latin
// FallbackAdvance. Real SERP fonts render these glyphs ~em-wide; charging the
// Latin fallback (~9.79px @20px) instead of one em (20px @20px) underweights
// emoji titles ~2× and lets Google-truncated titles slip past the budget.
// We compare a known glyph against the same glyph followed by an emoji and
// require the delta to equal one em (== fontPx at this size).
func TestEmojiIsOneEm(t *testing.T) {
	// Representative runes from each emoji range the fix covers.
	cases := []struct {
		name string
		r    rune
	}{
		{"misc symbols / pictographs", '\U0001F525'}, // U+1F525 FIRE (U+1F300–1FAFF)
		{"emoticons", '\U0001F600'},                  // U+1F600 GRINNING FACE
		{"supplemental symbols", '\U0001FA90'},       // U+1FA90 RINGED PLANET (U+1FA70–1FAFF)
		{"misc symbols (BMP)", '☀'},                  // U+2600 BLACK SUN WITH RAYS (U+2600–26FF)
		{"dingbats", '✨'},                            // U+2728 SPARKLES (U+2700–27BF)
		{"mahjong/dominoes/cards", '\U0001F0CF'},     // U+1F0CF PLAYING CARD BLACK JOKER (U+1F000–1F0FF)
		{"regional indicator", '\U0001F1FA'},         // U+1F1FA REGIONAL INDICATOR SYMBOL LETTER U
		{"arrows dingbat", '⬅'},                      // U+2B05 LEFTWARDS BLACK ARROW (U+2B00–2BFF)
	}
	base := Width("A", titleFontPx)
	wantDelta := float64(UnitsPerEm) * float64(titleFontPx) / float64(UnitsPerEm) // == fontPx
	for _, tc := range cases {
		got := Width("A"+string(tc.r), titleFontPx)
		delta := got - base
		if math.Abs(delta-wantDelta) > 1e-9 {
			t.Errorf("%s (U+%04X): delta = %v, want %v (one em)", tc.name, tc.r, delta, wantDelta)
		}
	}
}

// TestEmojiTitleFiresBudget is the A3 false-PASS regression: a realistic 10-emoji
// promotional title that Google would truncate. Charging each emoji the
// ~average-Latin FallbackAdvance (the bug) measures ~525px — under the 580px
// title budget — so title_pixel_overflow wrongly PASSES. Charging each emoji one
// em (the fix) measures ~627px, exceeding the budget, so the rule fires. The
// per-emoji correction is fixed: 10 × (UnitsPerEm − FallbackAdvance) px-units,
// which is what flips this title across the budget. The rule lives in the rules
// package; here we pin the measurer that drives it.
func TestEmojiTitleFiresBudget(t *testing.T) {
	const titleBudgetPx = 580.0 // local copy of the policy budget; see rules pkg.
	// 10 emoji drawn from the covered ranges, interleaved with ASCII the way a
	// real promo title decorates words.
	const title = "🔥🎉🎊 Best Summer Sale Clearance 💰🎁🌟 Save Big Today 👍🚀📣💥"

	emoji := 0
	for _, r := range title {
		if r >= 0x1F300 || (r >= 0x2600 && r <= 0x27BF) || (r >= 0x2B00 && r <= 0x2BFF) {
			emoji++
		}
	}
	if emoji != 10 {
		t.Fatalf("test fixture must contain exactly 10 emoji, counted %d", emoji)
	}

	got := Width(title, titleFontPx)
	if got <= titleBudgetPx {
		t.Errorf("emoji title measured %.2fpx, want > %.0fpx (Google truncates it; rule must fire)", got, titleBudgetPx)
	}

	// Anchor the fix's magnitude: the same title with every emoji forced to the
	// Latin fallback (the pre-fix behavior) is below budget. The difference is
	// exactly 10 ems' worth of correction over the fallback, and that correction
	// is what carries the title across the 580px line.
	const buggyPerEmoji = float64(FallbackAdvance) * float64(titleFontPx) / float64(UnitsPerEm)
	const fixedPerEmoji = float64(UnitsPerEm) * float64(titleFontPx) / float64(UnitsPerEm)
	buggyWidth := got - float64(emoji)*(fixedPerEmoji-buggyPerEmoji)
	if buggyWidth >= titleBudgetPx {
		t.Fatalf("fixture not discriminating: buggy width %.2fpx is already >= budget %.0fpx (must be a false PASS pre-fix)", buggyWidth, titleBudgetPx)
	}
}

// TestDeterminism: Width is a pure function of (text, fontPx). The integer-sum,
// scale-once design means repeated and re-ordered calls return bit-identical
// results — no per-rune float accumulation drift.
func TestDeterminism(t *testing.T) {
	inputs := []string{
		"Hello, World!",
		strings.Repeat("Wmi.,", 113),
		"naïve café — “quotes” … •",
		"中文 mixed with ASCII 日本語",
		"emoji 🎉 and combining é",
	}
	for _, s := range inputs {
		first := Width(s, titleFontPx)
		for i := 0; i < 50; i++ {
			if got := Width(s, titleFontPx); got != first {
				t.Fatalf("Width(%q) non-deterministic: call %d = %v, first = %v", s, i, got, first)
			}
		}
	}
}

// TestWidthScalesLinearlyWithFont: because scaling is applied once at the end,
// the same text at 2×fontPx is exactly 2× the width (within float epsilon).
func TestWidthScalesLinearly(t *testing.T) {
	const s = "Scaling check, naïve café!"
	w20 := Width(s, 20)
	w40 := Width(s, 40)
	if math.Abs(w40-2*w20) > 1e-9 {
		t.Errorf("Width@40 = %v, want 2×Width@20 = %v", w40, 2*w20)
	}
}

// TestWidthZeroFont: a zero or negative font size measures 0 (no negative px),
// guarding the rules-package boundary against a misconfigured budget call.
func TestWidthNonPositiveFont(t *testing.T) {
	if got := Width("anything", 0); got != 0 {
		t.Errorf("Width(_, 0) = %v, want 0", got)
	}
	if got := Width("anything", -5); got < 0 {
		t.Errorf("Width(_, -5) = %v, want non-negative", got)
	}
}

// FuzzWidth: arbitrary UTF-8 (and invalid byte sequences) must never panic; the
// result is always finite and ≥ 0. This is acceptance criterion 4.
func FuzzWidth(f *testing.F) {
	seeds := []string{
		"", "a", "Hello, World!", "中文", "café", "👍🏽",
		"\xff\xfe", "á́", strings.Repeat("W", 1000),
	}
	for _, s := range seeds {
		f.Add(s, 20)
	}
	f.Fuzz(func(t *testing.T, text string, fontPx int) {
		got := Width(text, fontPx)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("non-finite width %v for %q @ %d", got, text, fontPx)
		}
		if got < 0 {
			t.Fatalf("negative width %v for %q @ %d", got, text, fontPx)
		}
		// Invalid UTF-8 must still be handled (RuneError path), never panic;
		// reaching here without panicking is the assertion.
		_ = utf8.ValidString(text)
	})
}
