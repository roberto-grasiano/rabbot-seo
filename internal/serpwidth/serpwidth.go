// Package serpwidth measures the *rendered pixel width* of a title or meta
// description against the desktop SERP reference font, so callers can decide
// whether text will fit a result's container instead of counting characters.
//
// Why not len(). Search results truncate by rendered width, not character count:
// the desktop title container is ~580px of Arial 20px and the snippet ~Arial
// 14px, so 70 thin characters ("il.,:") can fit where 48 wide ones ("WMD&")
// clip. This package is the single owner of "how wide is this text?"; the budgets
// it is compared against are policy and live in the rules package next to the
// rules that apply them (mechanism vs policy).
//
// How it works. A static per-rune advance-width table (table.go) holds Arial's
// `hmtx` advances in design units at unitsPerEm = 2048. Width sums the advances
// as integers and scales to pixels exactly once — px = units*fontPx/2048 — so the
// result is deterministic with no per-rune floating-point drift, and the same
// (text, fontPx) always yields a bit-identical float64.
//
// Honest fallbacks (documented so callers know the approximation):
//
//   - A rune not in the table contributes FallbackAdvance design units (the
//     table's average Latin advance) — a defensible "average character" charge.
//   - A CJK / fullwidth ideograph contributes one full em (UnitsPerEm): we do not
//     carry per-ideograph metrics, and ideographs are ~em-wide in practice.
//   - A pictographic emoji (the emoji/dingbat/flag/emoji-arrow blocks) likewise
//     contributes one full em: emoji render ~em-wide, so the average-Latin charge
//     would under-measure emoji titles ~2× and miss ones Google truncates.
//   - A combining mark (unicode.Mn) contributes 0 — it composites onto the
//     preceding base glyph rather than advancing the pen (so "e" + U+0301 is as
//     wide as "e").
//   - Invalid UTF-8 decodes to U+FFFD and is charged FallbackAdvance; Width never
//     panics on arbitrary bytes.
//
// Deliberately ignored, as bounded approximations: kerning (a few px, absorbed by
// the "~" in the ~580px budget) and query-term bolding in snippets (unknowable
// from the page). No font binary is shipped, parsed, or loaded; the table is
// numeric facts only, so the static CGO_ENABLED=0 build and the no-dependency
// rule both hold. This package imports only the standard library.
package serpwidth

import "unicode"

// Width returns the rendered width, in pixels, of text set in the desktop SERP
// reference font at fontPx pixels. Runs of Unicode whitespace are collapsed to a
// single space and leading/trailing whitespace is dropped before measuring,
// matching how a browser lays out a text node (HTML collapses interior runs;
// upstream extraction only trims the ends).
//
// The computation is integer-exact until a single final scale: it accumulates
// each rune's design-unit advance (table lookup, or the documented CJK / combining
// / fallback rule) into an int64, then returns advanceUnits*fontPx/UnitsPerEm as a
// float64. A non-positive fontPx (a misconfigured budget) yields 0 rather than a
// zero/negative width. The empty string yields 0.
func Width(text string, fontPx int) float64 {
	if fontPx <= 0 || text == "" {
		return 0
	}

	var units int64
	emitted := false      // whether any non-space rune has been charged yet
	pendingSpace := false // a collapsed whitespace run awaiting a following non-space rune

	for _, r := range text {
		if isSpace(r) {
			// Collapse any run of whitespace to a single space, but defer charging
			// it until a non-space rune follows. Leading whitespace (emitted ==
			// false) and trailing whitespace (no following rune) thus charge zero.
			if emitted {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			units += int64(advance(' '))
			pendingSpace = false
		}
		units += int64(advance(r))
		emitted = true
	}

	return float64(units) * float64(fontPx) / float64(UnitsPerEm)
}

// advance returns the design-unit advance for a single rune, applying the table
// and the three documented fallbacks. It is the one place the fallback policy
// lives, so Width and any future caller agree.
func advance(r rune) int16 {
	if w, ok := advanceUnits[r]; ok {
		return w
	}
	// Combining marks stack on the base glyph: zero advance.
	if unicode.Is(unicode.Mn, r) {
		return 0
	}
	// CJK / fullwidth ideographs: one em.
	if isWideIdeograph(r) {
		return UnitsPerEm
	}
	// Wide pictographic emoji render ~em-wide in the SERP font, like an
	// ideograph — not the ~average-Latin FallbackAdvance. Charging the fallback
	// underweights emoji titles ~2× and lets Google-truncated titles slip the
	// budget, so emoji take the same one-em path as ideographs.
	if isWideEmoji(r) {
		return UnitsPerEm
	}
	// Everything else (unmapped Latin, symbols, U+FFFD): average advance.
	return FallbackAdvance
}

// isSpace reports whether r is whitespace for collapsing purposes. unicode.IsSpace
// covers ASCII spaces/tabs/newlines, NBSP (U+00A0), the Unicode space separators,
// and the line/paragraph separators — exactly the runs HTML layout collapses.
func isSpace(r rune) bool {
	return unicode.IsSpace(r)
}

// isWideIdeograph reports whether r is an East-Asian ideograph / fullwidth form
// that we approximate at one em. The ranges are the CJK ideograph blocks, the
// Hiragana/Katakana syllabaries, Hangul syllables, and the fullwidth/halfwidth
// forms block — the glyphs a Latin advance table cannot honestly measure. This is
// an approximation by design (see package doc), not a Unicode property query, so
// the boundaries are explicit and auditable.
func isWideIdeograph(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo
		return true
	case r >= 0x2E80 && r <= 0x2EFF: // CJK Radicals Supplement
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK Symbols and Punctuation
		return true
	case r >= 0x3040 && r <= 0x309F: // Hiragana
		return true
	case r >= 0x30A0 && r <= 0x30FF: // Katakana
		return true
	case r >= 0x3130 && r <= 0x318F: // Hangul Compatibility Jamo
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK Unified Ideographs Extension A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0xA000 && r <= 0xA4CF: // Yi Syllables/Radicals
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul Syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK Compatibility Ideographs
		return true
	case r >= 0xFF00 && r <= 0xFF60: // Fullwidth Forms (ASCII-range fullwidth)
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // Fullwidth signs
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // CJK Extension B–F + Compatibility Supplement
		return true
	default:
		return false
	}
}

// isWideEmoji reports whether r is a pictographic emoji we approximate at one em.
// Emoji render roughly square (~em-wide) in the SERP fonts, so charging them the
// ~average-Latin FallbackAdvance under-measures emoji titles ~2× — enough to miss
// titles Google truncates. Like isWideIdeograph this is a deliberate, auditable
// approximation (the boundaries are explicit ranges, not a Unicode property), and
// it is checked AFTER the advance table in advance(), so the handful of in-table
// BMP symbols with real Arial metrics (© ® ™ • · ° and the em dash) keep their
// exact advances. The ranges are the emoji blocks: the supplemental pictographs /
// symbols, emoticons, transport, and supplemental symbols; the misc-symbols and
// dingbats blocks; playing cards; regional-indicator flag letters; and the arrows
// blocks that carry emoji-presentation arrows.
func isWideEmoji(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // Misc Symbols & Pictographs, Emoticons, Transport, Supplemental & Symbols-Extended-A
		return true
	case r >= 0x2600 && r <= 0x27BF: // Miscellaneous Symbols + Dingbats
		return true
	case r >= 0x1F000 && r <= 0x1F0FF: // Mahjong/Domino/Playing-Card tiles
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // Regional Indicator Symbols (flag letters)
		return true
	case r >= 0x2190 && r <= 0x21FF: // Arrows (emoji-presentation arrows live here)
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // Miscellaneous Symbols and Arrows (emoji arrows/stars)
		return true
	default:
		return false
	}
}
