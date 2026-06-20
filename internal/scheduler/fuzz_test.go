package scheduler

import (
	"bytes"
	"compress/gzip"
	"math"
	"strings"
	"testing"
)

// gz gzip-compresses s for seeding the gzip decompression path.
func gz(s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

// FuzzSitemap fuzzes ParseSitemap (sitemap.go), including the transparent gzip
// decompression path (maybeGunzip). It asserts the parser's invariants on
// arbitrary bytes:
//
//   - err != nil  =>  len(entries) == 0   (no partial results on error)
//   - every Loc is non-empty and TrimSpace-stable
//   - every Priority is finite and > 0
//   - isIndex / entry-shape consistency (entries only ever come back when the
//     call succeeded; the closed return set is err-xor-entries)
//   - determinism: a second parse of identical bytes yields the same result
//
// The non-finite-priority invariant is the spec's known falsifier:
// <priority>NaN</priority> passes encoding/xml's ParseFloat and survives the
// `pri <= 0` guard (NaN compares false), so a NaN priority would otherwise
// reach discovery's priority sort. The NaN seed below fails until ParseSitemap
// sanitizes non-finite priorities to 0.5.
func FuzzSitemap(f *testing.F) {
	// Seeds drawn from the inline fixtures in sitemap_test.go.
	f.Add([]byte(sitemapXML))                         // 1: real urlset
	f.Add([]byte(sitemapIndexXML))                    // 2: real sitemapindex
	f.Add(gz(sitemapXML))                             // 3: gzip-wrapped urlset
	f.Add(gz(sitemapIndexXML))                        // 4: gzip-wrapped index
	f.Add([]byte{0x1f, 0x8b, 0x42, 0x41, 0x44, 0x00}) // 5: gzip magic + garbage
	f.Add(truncatedGzip(sitemapXML))                  // 6: truncated gzip stream
	// 7: urlset whose comment mentions <sitemapindex. The detection heuristic
	// is strings.Contains, so this is (documented) misclassified as an index —
	// the invariant suite must still hold; we do NOT redesign the heuristic.
	f.Add([]byte(`<?xml version="1.0"?>` +
		`<!-- this is not a <sitemapindex despite the text -->` +
		`<urlset><url><loc>https://example.com/x</loc></url></urlset>`))
	// 8: the known falsifier — non-finite priority. Initially fails; the fix
	// sanitizes NaN/Inf to 0.5.
	f.Add([]byte(`<urlset><url><loc>https://example.com/n</loc>` +
		`<priority>NaN</priority></url></urlset>`))
	// 9: +Inf priority — also survives the pri<=0 guard (Inf > 0).
	f.Add([]byte(`<urlset><url><loc>https://example.com/i</loc>` +
		`<priority>Inf</priority></url></urlset>`))
	// 10: empty, NUL, whitespace-only loc, negative priority.
	f.Add([]byte(""))
	f.Add([]byte{0x00, 0x00, 0x00})
	f.Add([]byte(`<urlset><url><loc>   </loc><priority>-1</priority></url></urlset>`))
	// 11 (fix #7): a non-numeric <priority> next to a VALID sibling. Pre-fix,
	// sitemapURL.Priority was a float64, so "high" aborted xml.Unmarshal and the
	// whole document (including the valid /good URL) was discarded — the invariant
	// suite's "err xor entries" still holds, but the regression was a silent
	// data-loss, not a non-finite priority. The tolerant string parse keeps both.
	f.Add([]byte(`<urlset><url><loc>https://example.com/bad</loc><priority>high</priority></url>` +
		`<url><loc>https://example.com/good</loc><priority>0.9</priority></url></urlset>`))
	// 12: comma-decimal and percent priorities (real-world malformed values).
	f.Add([]byte(`<urlset><url><loc>https://example.com/c</loc><priority>0,5</priority></url>` +
		`<url><loc>https://example.com/p</loc><priority>100%</priority></url></urlset>`))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, isIndex, err := ParseSitemap(data)

		if err != nil {
			if len(entries) != 0 {
				t.Fatalf("err != nil but len(entries) = %d (want 0): err=%v", len(entries), err)
			}
			// On error nothing else is decidable; isIndex may be either.
			return
		}

		for i, e := range entries {
			if e.Loc == "" {
				t.Fatalf("entry[%d] has empty Loc", i)
			}
			if strings.TrimSpace(e.Loc) != e.Loc {
				t.Fatalf("entry[%d].Loc = %q is not TrimSpace-stable", i, e.Loc)
			}
			if math.IsNaN(e.Priority) || math.IsInf(e.Priority, 0) {
				t.Fatalf("entry[%d].Priority = %v is non-finite", i, e.Priority)
			}
			if e.Priority <= 0 {
				t.Fatalf("entry[%d].Priority = %v is not > 0", i, e.Priority)
			}
		}

		// Determinism: identical bytes parse identically.
		entries2, isIndex2, err2 := ParseSitemap(data)
		if (err2 != nil) || isIndex2 != isIndex || len(entries2) != len(entries) {
			t.Fatalf("nondeterministic parse: (err=%v,isIndex=%v,n=%d) vs (err=%v,isIndex=%v,n=%d)",
				err, isIndex, len(entries), err2, isIndex2, len(entries2))
		}
		for i := range entries {
			if entries[i] != entries2[i] {
				t.Fatalf("nondeterministic entry[%d]: %+v vs %+v", i, entries[i], entries2[i])
			}
		}
	})
}

// truncatedGzip returns a valid gzip stream with its tail chopped off, so the
// gzip reader errors mid-stream and maybeGunzip falls back to the raw bytes.
func truncatedGzip(s string) []byte {
	full := gz(s)
	if len(full) <= 10 {
		return full
	}
	return full[:len(full)-5]
}
