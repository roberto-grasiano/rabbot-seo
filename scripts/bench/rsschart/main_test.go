package main

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// writeTempCSV writes content to a temp file and returns its path. readCSV takes
// a path (not an io.Reader), so we round-trip through the filesystem.
func writeTempCSV(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "capacity.csv")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp csv: %v", err)
	}
	return p
}

func TestReadCSV_ParsesHeaderAndRows(t *testing.T) {
	// Header (non-numeric first field) is skipped; the two data rows are parsed.
	// vmrss_kb -> MiB is /1024; db_bytes -> MiB is /(1024*1024).
	csv := "elapsed_s,vmrss_kb,db_bytes\n" +
		"0,1024,1048576\n" + // 0s, 1 MiB rss, 1 MiB db
		"30,2048,2097152\n" // 30s, 2 MiB rss, 2 MiB db
	path := writeTempCSV(t, csv)

	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV: unexpected error: %v", err)
	}
	want := []sample{
		{elapsedS: 0, rssMiB: 1, dbMiB: 1},
		{elapsedS: 30, rssMiB: 2, dbMiB: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (header must be skipped)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadCSV_UnitConversionExact(t *testing.T) {
	// 1536 KiB = 1.5 MiB; 1572864 bytes = 1.5 MiB. A wrong divisor would not
	// produce exactly 1.5 for both, so this is a falsifiable conversion check.
	path := writeTempCSV(t, "elapsed_s,vmrss_kb,db_bytes\n12.5,1536,1572864\n")
	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("row count = %d, want 1", len(got))
	}
	s := got[0]
	if s.elapsedS != 12.5 {
		t.Errorf("elapsedS = %v, want 12.5", s.elapsedS)
	}
	if s.rssMiB != 1.5 {
		t.Errorf("rssMiB = %v, want 1.5 (1536 KiB / 1024)", s.rssMiB)
	}
	if s.dbMiB != 1.5 {
		t.Errorf("dbMiB = %v, want 1.5 (1572864 B / 1048576)", s.dbMiB)
	}
}

func TestReadCSV_SkipsMalformedAndShortRows(t *testing.T) {
	// The code's contract: a non-numeric field (in any of the 3 used columns) or a
	// row with <3 fields is *skipped* (not an error). Only good rows survive.
	csv := "elapsed_s,vmrss_kb,db_bytes\n" + // header: bad first field -> skip
		"\n" + // blank line -> <3 fields -> skip
		"0,1024,1048576\n" + // good
		"5,onlytwo\n" + // <3 fields -> skip
		"x,1024,1048576\n" + // bad elapsed -> skip
		"10,notanumber,1048576\n" + // bad rss -> skip
		"15,1024,notanumber\n" + // bad db -> skip
		"20,2048,2097152\n" // good
	path := writeTempCSV(t, csv)

	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV: unexpected error (malformed rows are skipped, not fatal): %v", err)
	}
	want := []sample{
		{elapsedS: 0, rssMiB: 1, dbMiB: 1},
		{elapsedS: 20, rssMiB: 2, dbMiB: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("kept %d rows, want %d (only the two fully-numeric rows): %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadCSV_EmptyInputNoRowsNoError(t *testing.T) {
	// Empty file: EOF immediately, zero rows, no error (main() handles the
	// "no data rows" case separately).
	path := writeTempCSV(t, "")
	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV(empty): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("readCSV(empty): got %d rows, want 0", len(got))
	}
}

func TestReadCSV_HeaderOnlyNoRows(t *testing.T) {
	path := writeTempCSV(t, "elapsed_s,vmrss_kb,db_bytes\n")
	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV(header only): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("readCSV(header only): got %d rows, want 0", len(got))
	}
}

func TestReadCSV_RaggedCSVStructureErrors(t *testing.T) {
	// FieldsPerRecord = -1 makes a varying field count *allowed*, so a CSV with
	// rows of differing widths must NOT error (it's not a structural error). This
	// guards against accidentally re-enabling field-count enforcement.
	csv := "elapsed_s,vmrss_kb,db_bytes,extra\n0,1024,1048576,ignored\n"
	path := writeTempCSV(t, csv)
	got, err := readCSV(path)
	if err != nil {
		t.Fatalf("readCSV(ragged): unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != (sample{elapsedS: 0, rssMiB: 1, dbMiB: 1}) {
		t.Fatalf("readCSV(ragged) = %+v, want one row {0,1,1} (extra column ignored)", got)
	}
}

func TestReadCSV_BareQuoteIsStructuralError(t *testing.T) {
	// An unterminated/bare quote is a CSV *structural* error which readCSV wraps
	// and returns (it does not silently skip). Distinguishes "skip a bad number"
	// from "fail on malformed CSV".
	csv := "elapsed_s,vmrss_kb,db_bytes\n0,10\"24,1048576\n"
	path := writeTempCSV(t, csv)
	_, err := readCSV(path)
	if err == nil {
		t.Fatal("readCSV(bare quote): expected a parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse CSV") {
		t.Errorf("error = %q, want it wrapped with %q", err.Error(), "parse CSV")
	}
}

func TestReadCSV_MissingFileErrors(t *testing.T) {
	_, err := readCSV(filepath.Join(t.TempDir(), "does-not-exist.csv"))
	if err == nil {
		t.Fatal("readCSV(missing file): expected an error, got nil")
	}
}

func TestNiceCeil(t *testing.T) {
	// Exact expected axis ceilings, including boundaries (value == m*pow returns
	// that step) and just-over boundaries (snaps to the next step).
	cases := []struct {
		in, want float64
	}{
		{0, 1},        // non-positive -> 1
		{-5, 1},       // negative -> 1
		{1, 1},        // exact boundary
		{1.0001, 2},   // just over 1 -> 2
		{2, 2},        // exact boundary
		{2.5, 5},      // over 2 -> 5
		{5, 5},        // exact boundary
		{5.0001, 10},  // over 5 -> 10
		{10, 10},      // exact boundary
		{10.0001, 20}, // over 10 -> 2*10
		{17, 20},      // -> 2*10
		{100, 100},    // exact power of ten
		{250, 500},    // over 200 -> 5*100
		{0.3, 0.5},    // sub-unit: over 0.2 -> 0.5
	}
	for _, c := range cases {
		got := niceCeil(c.in)
		// Use a tiny tolerance for the float-decade arithmetic.
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("niceCeil(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNiceCeil_IsACeilingAndNotZero(t *testing.T) {
	// Property: the result is always >= the input (a real ceiling) and strictly
	// positive (the axis must never collapse). These catch a flipped comparison.
	for _, v := range []float64{0, 0.05, 0.49, 1, 1.5, 7, 42, 99, 333, 1234} {
		got := niceCeil(v)
		if got <= 0 {
			t.Errorf("niceCeil(%v) = %v, must be > 0", v, got)
		}
		if got < v {
			t.Errorf("niceCeil(%v) = %v, must be >= input", v, got)
		}
	}
}

func TestTrimFloat(t *testing.T) {
	// Axis label: integer when whole, else exactly one decimal (truncating extra
	// precision via FormatFloat's rounding).
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{2, "2"},
		{10, "10"},
		{250, "250"},
		{1.5, "1.5"},
		{0.5, "0.5"},
		{1.25, "1.2"}, // one-decimal rounding (banker's: .25 -> .2)
		{3.14, "3.1"},
	}
	for _, c := range cases {
		if got := trimFloat(c.in); got != c.want {
			t.Errorf("trimFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTrimFloat2AndF(t *testing.T) {
	// Coordinate format: up to two decimals, trailing zeros and a trailing dot
	// trimmed, and -0 normalized to "0". f is a thin alias for trimFloat2.
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"}, // negative zero -> "0"
		{1, "1"},
		{64, "64"},
		{100, "100"},
		{1.5, "1.5"},
		{12.5, "12.5"},
		{1.25, "1.25"},
		{1.2, "1.2"},
		{1.234, "1.23"}, // two-decimal rounding
		{0.1, "0.1"},
		{-1.5, "-1.5"}, // negatives preserved
	}
	for _, c := range cases {
		if got := trimFloat2(c.in); got != c.want {
			t.Errorf("trimFloat2(%v) = %q, want %q", c.in, got, c.want)
		}
		if got := f(c.in); got != c.want {
			t.Errorf("f(%v) = %q, want %q (f must alias trimFloat2)", c.in, got, c.want)
		}
	}
}

func TestEscapeXML(t *testing.T) {
	cases := []struct {
		name     string
		in, want string
	}{
		{"ampersand", "a & b", "a &amp; b"},
		{"lt", "a < b", "a &lt; b"},
		{"gt", "a > b", "a &gt; b"},
		{"quote", `say "hi"`, "say &quot;hi&quot;"},
		{"apos", "it's", "it&apos;s"},
		{"plain passes through", "plain title 123", "plain title 123"},
		{"ampersand escaped once", "&lt;", "&amp;lt;"}, // & is escaped, not the literal "lt"
		{"all five", `&<>"'`, "&amp;&lt;&gt;&quot;&apos;"},
	}
	for _, c := range cases {
		if got := escapeXML(c.in); got != c.want {
			t.Errorf("%s: escapeXML(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestWritePolyline_EmitsExpectedPoints(t *testing.T) {
	// Identity maps so the emitted points equal the input coordinates exactly.
	// Two samples -> "x0,y0 x1,y1"; each sample also yields one <circle>.
	var b strings.Builder
	samples := []sample{
		{elapsedS: 0, rssMiB: 10},
		{elapsedS: 5, rssMiB: 20.5},
	}
	identity := func(v float64) float64 { return v }
	rss := func(s sample) float64 { return s.rssMiB }
	writePolyline(&b, samples, identity, identity, "#abc123", rss)
	out := b.String()

	wantPoints := `points="0,10 5,20.5"`
	if !strings.Contains(out, wantPoints) {
		t.Errorf("polyline missing %q\n got: %s", wantPoints, out)
	}
	if !strings.Contains(out, `stroke="#abc123"`) {
		t.Errorf("polyline missing stroke color\n got: %s", out)
	}
	// One <circle> dot per sample.
	if n := strings.Count(out, "<circle "); n != len(samples) {
		t.Errorf("circle count = %d, want %d\n got: %s", n, len(samples), out)
	}
	// Dot coordinates match the mapped sample positions.
	if !strings.Contains(out, `cx="0" cy="10"`) || !strings.Contains(out, `cx="5" cy="20.5"`) {
		t.Errorf("dot coordinates wrong\n got: %s", out)
	}
}

func TestWritePolyline_PointCountMatchesSamples(t *testing.T) {
	var b strings.Builder
	samples := []sample{
		{elapsedS: 0, rssMiB: 1},
		{elapsedS: 1, rssMiB: 2},
		{elapsedS: 2, rssMiB: 3},
		{elapsedS: 3, rssMiB: 4},
	}
	identity := func(v float64) float64 { return v }
	rss := func(s sample) float64 { return s.rssMiB }
	writePolyline(&b, samples, identity, identity, dbColor, rss)

	pts := extractPolylinePoints(t, b.String())
	if got := len(strings.Fields(pts)); got != len(samples) {
		t.Errorf("polyline point count = %d, want %d (one per sample); points=%q", got, len(samples), pts)
	}
}

var polylinePointsRe = regexp.MustCompile(`<polyline[^>]*\bpoints="([^"]*)"`)

// extractPolylinePoints returns the points attribute of the first <polyline> in s.
func extractPolylinePoints(t *testing.T, s string) string {
	t.Helper()
	m := polylinePointsRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no <polyline points=...> found in:\n%s", s)
	}
	return m[1]
}

func TestRender_StructureAndPointCount(t *testing.T) {
	// Three samples; rss climbs 10 -> 30 -> 50, db climbs 1 -> 2 -> 4.
	samples := []sample{
		{elapsedS: 0, rssMiB: 10, dbMiB: 1},
		{elapsedS: 30, rssMiB: 30, dbMiB: 2},
		{elapsedS: 60, rssMiB: 50, dbMiB: 4},
	}
	svg := render(samples, "cap & test")

	// Root SVG element with the fixed dimensions.
	if !strings.HasPrefix(strings.TrimSpace(svg), "<svg ") {
		t.Errorf("output does not start with <svg: %.60q", svg)
	}
	if !strings.Contains(svg, `width="900"`) || !strings.Contains(svg, `height="420"`) {
		t.Error("svg missing the fixed 900x420 dimensions")
	}
	if !strings.HasSuffix(strings.TrimSpace(svg), "</svg>") {
		t.Error("svg not closed with </svg>")
	}

	// Title is escaped into the document (the & must become &amp;).
	if !strings.Contains(svg, "cap &amp; test") {
		t.Error("title not present/escaped in svg")
	}

	// There must be two polylines (RSS + DB), each with exactly len(samples) points.
	matches := polylinePointsRe.FindAllStringSubmatch(svg, -1)
	if len(matches) != 2 {
		t.Fatalf("found %d polylines, want 2 (RSS + DB)", len(matches))
	}
	for i, m := range matches {
		n := len(strings.Fields(m[1]))
		if n != len(samples) {
			t.Errorf("polyline %d has %d points, want %d", i, n, len(samples))
		}
	}

	// Both series colors are present.
	if !strings.Contains(svg, rssColor) {
		t.Errorf("svg missing RSS color %s", rssColor)
	}
	if !strings.Contains(svg, dbColor) {
		t.Errorf("svg missing DB color %s", dbColor)
	}
}

func TestRender_PeakReflectedInYAxis(t *testing.T) {
	// maxRSS = 50 -> niceCeil = 50, so the top RSS axis label (frac=1.0) is "50".
	// maxDB = 4 -> niceCeil = 5, so the top DB axis label is "5". A wrong axis-top
	// computation would not place these exact labels.
	samples := []sample{
		{elapsedS: 0, rssMiB: 10, dbMiB: 1},
		{elapsedS: 60, rssMiB: 50, dbMiB: 4},
	}
	svg := render(samples, "t")

	// Top RSS label is drawn in rssColor; assert the label text "50" exists.
	if !labelExists(svg, rssColor, "50") {
		t.Errorf("expected RSS axis top label %q in color %s\n%s", "50", rssColor, svg)
	}
	// Top DB label is niceCeil(4)=5, drawn in dbColor.
	if !labelExists(svg, dbColor, "5") {
		t.Errorf("expected DB axis top label %q in color %s\n%s", "5", dbColor, svg)
	}
}

// labelExists reports whether svg contains a <text ... fill="color"...>label</text>.
func labelExists(svg, color, label string) bool {
	re := regexp.MustCompile(`<text[^>]*fill="` + regexp.QuoteMeta(color) + `"[^>]*>` + regexp.QuoteMeta(label) + `</text>`)
	return re.MatchString(svg)
}

func TestRender_MonotonicYMapping(t *testing.T) {
	// In SVG, y grows downward and yRSS maps a larger value to a SMALLER y (higher
	// on screen). With rss ascending across samples, the RSS polyline's y
	// coordinates must be strictly decreasing. This is the load-bearing "a larger
	// RSS value maps to a higher point" property the published graph relies on.
	samples := []sample{
		{elapsedS: 0, rssMiB: 5, dbMiB: 1},
		{elapsedS: 10, rssMiB: 15, dbMiB: 1},
		{elapsedS: 20, rssMiB: 40, dbMiB: 1},
	}
	svg := render(samples, "t")

	matches := polylinePointsRe.FindAllStringSubmatch(svg, -1)
	if len(matches) != 2 {
		t.Fatalf("found %d polylines, want 2", len(matches))
	}
	// The first polyline written is RSS (see render: writePolyline RSS, then DB).
	ys := parseYs(t, matches[0][1])
	if len(ys) != len(samples) {
		t.Fatalf("RSS polyline has %d points, want %d", len(ys), len(samples))
	}
	for i := 1; i < len(ys); i++ {
		if !(ys[i] < ys[i-1]) {
			t.Errorf("RSS y not strictly decreasing for ascending values: y[%d]=%v !< y[%d]=%v (ys=%v)",
				i, ys[i], i-1, ys[i-1], ys)
		}
	}
	// And the largest value's y should sit at/above the smallest value's y by a
	// real margin (not all collapsed to one row).
	if ys[len(ys)-1] >= ys[0] {
		t.Errorf("peak point (y=%v) not above first point (y=%v)", ys[len(ys)-1], ys[0])
	}
}

// parseYs extracts the y component of each "x,y" pair in a points string.
func parseYs(t *testing.T, points string) []float64 {
	t.Helper()
	var ys []float64
	for _, pair := range strings.Fields(points) {
		parts := strings.Split(pair, ",")
		if len(parts) != 2 {
			t.Fatalf("bad point %q in %q", pair, points)
		}
		y, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			t.Fatalf("bad y %q: %v", parts[1], err)
		}
		ys = append(ys, y)
	}
	return ys
}

func TestRender_SingleSampleDoesNotPanic(t *testing.T) {
	// The doc comment promises a single sample still renders (flat line, no
	// divide-by-zero). maxElapsed<=0 is clamped to 1.
	samples := []sample{{elapsedS: 0, rssMiB: 7, dbMiB: 3}}
	svg := render(samples, "single")

	matches := polylinePointsRe.FindAllStringSubmatch(svg, -1)
	if len(matches) != 2 {
		t.Fatalf("found %d polylines, want 2", len(matches))
	}
	for i, m := range matches {
		if n := len(strings.Fields(m[1])); n != 1 {
			t.Errorf("polyline %d has %d points, want 1", i, n)
		}
		// The single point must have finite coordinates (no NaN/Inf from /0).
		for _, pair := range strings.Fields(m[1]) {
			parts := strings.Split(pair, ",")
			for _, c := range parts {
				v, err := strconv.ParseFloat(c, 64)
				if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
					t.Errorf("polyline %d coordinate %q is not finite (divide-by-zero?)", i, c)
				}
			}
		}
	}
}
