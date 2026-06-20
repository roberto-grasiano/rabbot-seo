package serpwidth

// Code in this file is a static metrics table — facts derived offline, not a
// redistributed font. See the provenance note below.
//
// PROVENANCE. The per-rune values are horizontal advance widths ("advanceWidth"
// from the `hmtx` table) of Arial Regular, the desktop SERP reference face,
// expressed in the font's design units at unitsPerEm = 2048. They were read once,
// offline, from the font's published metrics and transcribed here as integers.
// No font binary is shipped, parsed, or loaded at runtime: this is a table of
// numbers (the same status as quoting a glyph's width in a spec), so the static
// CGO_ENABLED=0 build and the no-new-dependency rule are both preserved.
//
// COVERAGE (acceptance: printable ASCII + Latin-1 letters + common typographic
// punctuation). Anything outside this set is handled by the documented fallbacks
// in Width: CJK/fullwidth ranges → one em, combining marks → 0, everything else
// → FallbackAdvance. Adding a rune here is a metrics fact, not a behavior change.

// UnitsPerEm is Arial's design grid: every advance in advanceUnits is in these
// units, and Width scales to pixels with a single `units*fontPx/UnitsPerEm`.
// Exported as part of this package's numeric contract — the golden tests pin the
// scale explicitly rather than hiding a magic 2048, and it documents the unit any
// external audit of the table must assume. The rules package consumes only Width,
// not this constant.
const UnitsPerEm = 2048

// FallbackAdvance is the advance charged for a rune that is in neither the table
// nor a recognized CJK/combining range. It is the mean of the table's Latin
// lowercase a–z advances (≈ the "average character" width), fixed at generation
// time so the fallback is deterministic and never depends on table iteration.
// Value computed offline from advanceUnits['a'..'z']: sum 26067 / 26 = 1002.6,
// truncated to 1002 design units.
const FallbackAdvance int16 = 1002

// advanceUnits maps a rune to its Arial advance width in design units. Runes are
// grouped to keep the table auditable against the source metrics.
var advanceUnits = map[rune]int16{
	// --- ASCII control/space region ---
	' ': 569, // U+0020 SPACE

	// --- ASCII punctuation 0x21–0x2F ---
	'!':  569,
	'"':  727,
	'#':  1139,
	'$':  1139,
	'%':  1821,
	'&':  1366,
	'\'': 391,
	'(':  682,
	')':  682,
	'*':  797,
	'+':  1196,
	',':  569,
	'-':  682,
	'.':  569,
	'/':  569,

	// --- digits 0–9 (Arial digits are tabular: all 1139) ---
	'0': 1139, '1': 1139, '2': 1139, '3': 1139, '4': 1139,
	'5': 1139, '6': 1139, '7': 1139, '8': 1139, '9': 1139,

	// --- ASCII punctuation 0x3A–0x40 ---
	':': 569,
	';': 569,
	'<': 1196,
	'=': 1196,
	'>': 1196,
	'?': 1139,
	'@': 2079,

	// --- uppercase A–Z ---
	'A': 1366, 'B': 1366, 'C': 1479, 'D': 1479, 'E': 1366,
	'F': 1251, 'G': 1593, 'H': 1479, 'I': 569, 'J': 1024,
	'K': 1366, 'L': 1139, 'M': 1706, 'N': 1479, 'O': 1593,
	'P': 1366, 'Q': 1593, 'R': 1479, 'S': 1366, 'T': 1251,
	'U': 1479, 'V': 1366, 'W': 1933, 'X': 1366, 'Y': 1366,
	'Z': 1251,

	// --- ASCII punctuation 0x5B–0x60 ---
	'[':  569,
	'\\': 569,
	']':  569,
	'^':  961,
	'_':  1139,
	'`':  682,

	// --- lowercase a–z ---
	'a': 1139, 'b': 1139, 'c': 1024, 'd': 1139, 'e': 1139,
	'f': 569, 'g': 1139, 'h': 1139, 'i': 455, 'j': 455,
	'k': 1024, 'l': 455, 'm': 1706, 'n': 1139, 'o': 1139,
	'p': 1139, 'q': 1139, 'r': 682, 's': 1024, 't': 569,
	'u': 1139, 'v': 1024, 'w': 1479, 'x': 1024, 'y': 1024,
	'z': 1024,

	// --- ASCII punctuation 0x7B–0x7E ---
	'{': 684,
	'|': 532,
	'}': 684,
	'~': 1196,

	// --- Latin-1 letters (accented Latin commonly seen in titles/descriptions).
	// Accented forms share their base letter's advance in Arial (the accent is a
	// zero-advance mark composited above), which is why these mirror a–z / A–Z. ---
	'À': 1366, 'Á': 1366, 'Â': 1366, 'Ã': 1366, 'Ä': 1366, 'Å': 1366,
	'Æ': 1821, 'Ç': 1479, 'È': 1366, 'É': 1366, 'Ê': 1366, 'Ë': 1366,
	'Ì': 569, 'Í': 569, 'Î': 569, 'Ï': 569, 'Ð': 1479, 'Ñ': 1479,
	'Ò': 1593, 'Ó': 1593, 'Ô': 1593, 'Õ': 1593, 'Ö': 1593, 'Ø': 1593,
	'Ù': 1479, 'Ú': 1479, 'Û': 1479, 'Ü': 1479, 'Ý': 1366, 'Þ': 1366,
	'ß': 1196,
	'à': 1139, 'á': 1139, 'â': 1139, 'ã': 1139, 'ä': 1139, 'å': 1139,
	'æ': 1821, 'ç': 1024, 'è': 1139, 'é': 1139, 'ê': 1139, 'ë': 1139,
	'ì': 455, 'í': 455, 'î': 455, 'ï': 455, 'ð': 1139, 'ñ': 1139,
	'ò': 1139, 'ó': 1139, 'ô': 1139, 'õ': 1139, 'ö': 1139, 'ø': 1139,
	'ù': 1139, 'ú': 1139, 'û': 1139, 'ü': 1139, 'ý': 1024, 'þ': 1139,
	'ÿ': 1024,

	// --- common typographic punctuation (the spec's explicit set) ---
	'–': 1139, // U+2013 EN DASH
	'—': 2048, // U+2014 EM DASH (one em)
	'‘': 569,  // U+2018 LEFT SINGLE QUOTATION MARK
	'’': 569,  // U+2019 RIGHT SINGLE QUOTATION MARK
	'“': 909,  // U+201C LEFT DOUBLE QUOTATION MARK
	'”': 909,  // U+201D RIGHT DOUBLE QUOTATION MARK
	'…': 1933, // U+2026 HORIZONTAL ELLIPSIS
	'•': 803,  // U+2022 BULLET
	'·': 569,  // U+00B7 MIDDLE DOT
	'©': 1815, // U+00A9 COPYRIGHT SIGN
	'®': 1815, // U+00AE REGISTERED SIGN
	'™': 1717, // U+2122 TRADE MARK SIGN
	'°': 768,  // U+00B0 DEGREE SIGN
}
