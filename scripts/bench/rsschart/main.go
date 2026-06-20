// Command rsschart renders a capacity CSV (as written by scripts/bench/
// capacity.sh: header "elapsed_s,vmrss_kb,db_bytes") into a small, deterministic
// SVG line chart at assets/capacity-rss.svg. It is dev tooling — never shipped
// (goreleaser builds only ./cmd/rabbot) — and uses only the standard library
// (an SVG is plain text, so no plotting dependency is needed; this keeps the
// zero-new-go.mod-entries acceptance criterion intact).
//
// The output is deterministic: the same CSV yields byte-identical SVG (no maps
// in the render path, no time, no rand), so the committed assets/capacity-rss.svg
// is reproducible from the committed capacity-N.csv.
//
// Usage:
//
//	rsschart -in capacity-2000.csv -out assets/capacity-rss.svg [-title "..."]
//
// Two series are drawn against elapsed seconds: resident memory (VmRSS, MiB) and
// the SQLite DB file size (MiB), each auto-scaled to its own right/left axis so a
// reader sees memory steadiness and storage growth on one picture.
package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	in := flag.String("in", "", "input capacity CSV (header: elapsed_s,vmrss_kb,db_bytes)")
	out := flag.String("out", "assets/capacity-rss.svg", "output SVG path")
	title := flag.String("title", "rabbot capacity — resident memory and DB size over time", "chart title")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "rsschart: -in is required")
		os.Exit(2)
	}

	rows, err := readCSV(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rsschart: %v\n", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "rsschart: no data rows in input")
		os.Exit(1)
	}

	svg := render(rows, *title)

	// 0644: the output is a committed, world-readable image asset
	// (assets/capacity-rss.svg), not a secret — gosec G306's 0600 default does
	// not apply to a published chart.
	if err := os.WriteFile(*out, []byte(svg), 0o644); err != nil { //nolint:gosec // committed public SVG asset, not a secret
		fmt.Fprintf(os.Stderr, "rsschart: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d points)\n", *out, len(rows))
}

// sample is one CSV data row.
type sample struct {
	elapsedS float64
	rssMiB   float64
	dbMiB    float64
}

// readCSV parses the capacity CSV. It tolerates (and skips) the header row and
// any blank lines, and returns the data rows in file order.
func readCSV(path string) ([]sample, error) {
	f, err := os.Open(path) //nolint:gosec // dev tool reading a developer-supplied capacity CSV path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = -1 // tolerate ragged rows; we validate the fields we use
	var out []sample
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse CSV: %w", err)
		}
		if len(rec) < 3 {
			continue
		}
		// Skip the header (non-numeric first field).
		es, err := strconv.ParseFloat(strings.TrimSpace(rec[0]), 64)
		if err != nil {
			continue
		}
		rssKB, err := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
		if err != nil {
			continue
		}
		dbB, err := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		if err != nil {
			continue
		}
		out = append(out, sample{
			elapsedS: es,
			rssMiB:   rssKB / 1024.0,
			dbMiB:    dbB / (1024.0 * 1024.0),
		})
	}
	return out, nil
}

// Chart geometry. Fixed so the output is deterministic and the committed SVG is
// stable across runs.
const (
	svgW     = 900
	svgH     = 420
	padL     = 64 // left padding (RSS axis labels)
	padR     = 64 // right padding (DB axis labels)
	padT     = 48 // top padding (title)
	padB     = 56 // bottom padding (x-axis labels)
	plotW    = svgW - padL - padR
	plotH    = svgH - padT - padB
	rssColor = "#2563eb" // blue: resident memory
	dbColor  = "#16a34a" // green: DB size
	gridGray = "#e5e7eb"
	axisGray = "#9ca3af"
	textGray = "#374151"
)

// render builds the SVG document for the samples. Both series share the x axis
// (elapsed seconds); RSS scales to the left axis, DB size to the right axis. Each
// axis top is the series max rounded up to a "nice" number so the gridlines are
// readable. With a single sample, the chart still renders (a single point/flat
// line) rather than dividing by zero.
func render(samples []sample, title string) string {
	maxElapsed := samples[0].elapsedS
	maxRSS := samples[0].rssMiB
	maxDB := samples[0].dbMiB
	for _, s := range samples {
		if s.elapsedS > maxElapsed {
			maxElapsed = s.elapsedS
		}
		if s.rssMiB > maxRSS {
			maxRSS = s.rssMiB
		}
		if s.dbMiB > maxDB {
			maxDB = s.dbMiB
		}
	}
	if maxElapsed <= 0 {
		maxElapsed = 1
	}
	rssTop := niceCeil(maxRSS)
	dbTop := niceCeil(maxDB)

	// x maps elapsed seconds to a pixel column; yRSS/yDB map a value to a pixel row
	// (SVG y grows downward, so a larger value sits higher = smaller y).
	x := func(es float64) float64 { return padL + (es/maxElapsed)*plotW }
	yRSS := func(v float64) float64 { return padT + plotH - (v/rssTop)*plotH }
	yDB := func(v float64) float64 { return padT + plotH - (v/dbTop)*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="ui-sans-serif,system-ui,sans-serif">`+"\n", svgW, svgH, svgW, svgH)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`+"\n", svgW, svgH)

	// Title.
	fmt.Fprintf(&b, `<text x="%d" y="28" font-size="16" font-weight="600" fill="%s">%s</text>`+"\n",
		padL, textGray, escapeXML(title))

	// Horizontal gridlines + left (RSS) and right (DB) axis labels at 4 divisions.
	const divs = 4
	for i := 0; i <= divs; i++ {
		frac := float64(i) / float64(divs)
		py := padT + plotH - frac*plotH
		fmt.Fprintf(&b, `<line x1="%d" y1="%s" x2="%d" y2="%s" stroke="%s" stroke-width="1"/>`+"\n",
			padL, f(py), padL+plotW, f(py), gridGray)
		// Left label: RSS MiB.
		fmt.Fprintf(&b, `<text x="%d" y="%s" font-size="11" text-anchor="end" fill="%s">%s</text>`+"\n",
			padL-8, f(py+4), rssColor, trimFloat(frac*rssTop))
		// Right label: DB MiB.
		fmt.Fprintf(&b, `<text x="%d" y="%s" font-size="11" text-anchor="start" fill="%s">%s</text>`+"\n",
			padL+plotW+8, f(py+4), dbColor, trimFloat(frac*dbTop))
	}

	// X axis baseline + a few elapsed-time ticks.
	baseY := padT + plotH
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="1"/>`+"\n",
		padL, baseY, padL+plotW, baseY, axisGray)
	for i := 0; i <= divs; i++ {
		frac := float64(i) / float64(divs)
		es := frac * maxElapsed
		px := x(es)
		fmt.Fprintf(&b, `<text x="%s" y="%d" font-size="11" text-anchor="middle" fill="%s">%ss</text>`+"\n",
			f(px), baseY+20, textGray, trimFloat(es))
	}

	// Series polylines.
	writePolyline(&b, samples, x, yRSS, rssColor, func(s sample) float64 { return s.rssMiB })
	writePolyline(&b, samples, x, yDB, dbColor, func(s sample) float64 { return s.dbMiB })

	// Legend.
	legendY := padT + 4
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/>`, padL+plotW-220, legendY, rssColor)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" fill="%s">VmRSS (MiB)</text>`, padL+plotW-204, legendY+11, textGray)
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/>`, padL+plotW-110, legendY, dbColor)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" fill="%s">DB file (MiB)</text>`+"\n", padL+plotW-94, legendY+11, textGray)

	b.WriteString("</svg>\n")
	return b.String()
}

// writePolyline writes one SVG polyline for a series into b, plus a small dot at
// each sample so a sparse series is still visible. valOf extracts the series
// value from a sample; yMap maps that value to a pixel row.
func writePolyline(b *strings.Builder, samples []sample, xMap func(float64) float64, yMap func(float64) float64, color string, valOf func(sample) float64) {
	var pts strings.Builder
	var dots strings.Builder
	for i, s := range samples {
		px := xMap(s.elapsedS)
		py := yMap(valOf(s))
		if i > 0 {
			pts.WriteByte(' ')
		}
		pts.WriteString(f(px))
		pts.WriteByte(',')
		pts.WriteString(f(py))
		fmt.Fprintf(&dots, `<circle cx="%s" cy="%s" r="2" fill="%s"/>`, f(px), f(py), color)
	}
	fmt.Fprintf(b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`+"\n", color, pts.String())
	b.WriteString(dots.String())
	b.WriteByte('\n')
}

// niceCeil rounds v up to a readable axis top (1, 2, 5 x 10^n). A non-positive
// input returns 1 so the axis never collapses to zero height.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	// Find the power of ten at or below v.
	pow := 1.0
	for pow*10 <= v {
		pow *= 10
	}
	for pow > v {
		pow /= 10
	}
	// pow <= v < pow*10. Snap up to 1,2,5,10 x pow.
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*pow {
			return m * pow
		}
	}
	return 10 * pow
}

// f formats a float for an SVG coordinate with two decimals, trimming trailing
// zeros so the output is compact and stable.
func f(v float64) string {
	return trimFloat2(v)
}

// trimFloat formats an axis label: integer if whole, else one decimal.
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// trimFloat2 formats a coordinate with up to two decimals, trimming trailing
// zeros and any trailing dot, so the same value always renders identically.
func trimFloat2(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// escapeXML escapes the five XML metacharacters in text content / attributes so
// a title with an ampersand or angle bracket cannot break the SVG.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
