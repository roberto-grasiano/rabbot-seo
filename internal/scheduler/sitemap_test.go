package scheduler

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

const sitemapXML = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/</loc><priority>1.0</priority></url>
  <url><loc>https://example.com/about</loc><priority>0.5</priority></url>
  <url><loc>https://example.com/blog</loc></url>
</urlset>`

const sitemapIndexXML = `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap-1.xml</loc></sitemap>
  <sitemap><loc>https://example.com/sitemap-2.xml</loc></sitemap>
</sitemapindex>`

func TestParseSitemapURLs(t *testing.T) {
	entries, isIndex, err := ParseSitemap([]byte(sitemapXML))
	if err != nil {
		t.Fatalf("ParseSitemap() error = %v", err)
	}
	if isIndex {
		t.Errorf("urlset misdetected as index")
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Loc != "https://example.com/" || entries[0].Priority != 1.0 {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Priority != 0.5 {
		t.Errorf("entry[1].Priority = %v, want 0.5", entries[1].Priority)
	}
	// Missing priority defaults to 0.5 per sitemap spec.
	if entries[2].Priority != 0.5 {
		t.Errorf("entry[2].Priority = %v, want default 0.5", entries[2].Priority)
	}
}

func TestParseSitemapIndex(t *testing.T) {
	entries, isIndex, err := ParseSitemap([]byte(sitemapIndexXML))
	if err != nil {
		t.Fatalf("ParseSitemap() error = %v", err)
	}
	if !isIndex {
		t.Errorf("sitemapindex not detected")
	}
	if len(entries) != 2 || entries[1].Loc != "https://example.com/sitemap-2.xml" {
		t.Errorf("index entries = %+v", entries)
	}
}

func TestParseSitemapBudget(t *testing.T) {
	entries, _, err := ParseSitemap([]byte(sitemapXML))
	if err != nil {
		t.Fatalf("ParseSitemap() error = %v", err)
	}
	limited := ApplyBudget(entries, 2)
	if len(limited) != 2 {
		t.Errorf("ApplyBudget kept %d, want 2", len(limited))
	}
}

// TestParseSitemapTolerantPriority guards fix #7: a single non-numeric
// <priority> must NOT make encoding/xml error and discard the WHOLE document.
// The bad value falls back to the protocol default (0.5); its sibling URLs (and
// their valid priorities) survive. Pre-fix, sitemapURL.Priority was a float64,
// so one "high" aborted xml.Unmarshal and ParseSitemap returned 0 entries.
func TestParseSitemapTolerantPriority(t *testing.T) {
	const xml = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://e.com/a</loc><priority>1.0</priority></url>
  <url><loc>https://e.com/b</loc><priority>high</priority></url>
  <url><loc>https://e.com/c</loc><priority>0.3</priority></url>
</urlset>`
	entries, isIndex, err := ParseSitemap([]byte(xml))
	if err != nil {
		t.Fatalf("ParseSitemap() error = %v; one bad <priority> must not error the whole document", err)
	}
	if isIndex {
		t.Errorf("urlset misdetected as index")
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (a bad priority must not discard sibling URLs): %+v", len(entries), entries)
	}
	if entries[0].Loc != "https://e.com/a" || entries[0].Priority != 1.0 {
		t.Errorf("entry[0] = %+v, want valid priority preserved", entries[0])
	}
	if entries[1].Loc != "https://e.com/b" || entries[1].Priority != 0.5 {
		t.Errorf("entry[1] = %+v, want bad priority defaulted to 0.5", entries[1])
	}
	if entries[2].Loc != "https://e.com/c" || entries[2].Priority != 0.3 {
		t.Errorf("entry[2] = %+v, want valid priority preserved", entries[2])
	}
}

// TestParseSitemapPriorityVariants guards the tolerant per-URL parse over the
// real-world malformed values fix #7 enumerates: "high", "0,5" (comma decimal),
// "100%", empty, and the non-finite "NaN"/"Inf" tokens encoding/xml's float
// parser accepts. Every one defaults to 0.5; none errors the document.
func TestParseSitemapPriorityVariants(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{`<priority>0.8</priority>`, 0.8},
		{`<priority>high</priority>`, 0.5},
		{`<priority>0,5</priority>`, 0.5}, // comma decimal — not a Go float
		{`<priority>100%</priority>`, 0.5},
		{`<priority></priority>`, 0.5}, // explicit-but-empty
		{``, 0.5},                      // absent element
		{`<priority>NaN</priority>`, 0.5},
		{`<priority>Inf</priority>`, 0.5},
		{`<priority>-1</priority>`, 0.5}, // non-positive
		{`<priority>  0.7  </priority>`, 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			xml := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
				`<url><loc>https://e.com/x</loc>` + tc.raw + `</url></urlset>`
			entries, _, err := ParseSitemap([]byte(xml))
			if err != nil {
				t.Fatalf("ParseSitemap(%q) error = %v; tolerant parse must not error", tc.raw, err)
			}
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1: %+v", len(entries), entries)
			}
			if entries[0].Priority != tc.want {
				t.Errorf("Priority = %v, want %v", entries[0].Priority, tc.want)
			}
		})
	}
}

// TestParseSitemapLogDropBranch guards that ParseSitemapLog logs the per-URL
// priority-drop branch (fix #7) at Debug, naming the offending loc, while still
// keeping the URL with the defaulted priority. A valid sibling is NOT logged.
func TestParseSitemapLogDropBranch(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, "debug")

	const xml = `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<url><loc>https://e.com/bad</loc><priority>high</priority></url>` +
		`<url><loc>https://e.com/ok</loc><priority>0.9</priority></url></urlset>`
	entries, _, err := ParseSitemapLog([]byte(xml), log)
	if err != nil {
		t.Fatalf("ParseSitemapLog() error = %v", err)
	}
	if len(entries) != 2 || entries[0].Priority != 0.5 || entries[1].Priority != 0.9 {
		t.Fatalf("entries = %+v, want bad defaulted to 0.5 and valid kept", entries)
	}
	out := buf.String()
	if !strings.Contains(out, "sitemap priority defaulted") {
		t.Errorf("drop-branch not logged; log = %q", out)
	}
	if !strings.Contains(out, "https://e.com/bad") {
		t.Errorf("drop log does not name the offending URL; log = %q", out)
	}
	if strings.Contains(out, "https://e.com/ok") {
		t.Errorf("valid-priority URL must not be logged as a drop; log = %q", out)
	}
}

// TestParseSitemapNilLogNoPanic guards that the nil-logger path (the
// ParseSitemap entrypoint) parses tolerantly without touching the logger.
func TestParseSitemapNilLogNoPanic(t *testing.T) {
	const xml = `<urlset><url><loc>https://e.com/a</loc><priority>bogus</priority></url></urlset>`
	entries, _, err := ParseSitemapLog([]byte(xml), nil)
	if err != nil || len(entries) != 1 || entries[0].Priority != 0.5 {
		t.Fatalf("ParseSitemapLog(nil log) entries=%+v err=%v, want one entry @0.5", entries, err)
	}
}

func TestParseSitemapGzip(t *testing.T) {
	t.Parallel()
	xml := `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<url><loc>https://ex.com/a</loc></url></urlset>`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(xml))
	_ = zw.Close()

	entries, isIndex, err := ParseSitemap(buf.Bytes())
	if err != nil || isIndex || len(entries) != 1 || entries[0].Loc != "https://ex.com/a" {
		t.Fatalf("gzip sitemap: entries=%v isIndex=%v err=%v", entries, isIndex, err)
	}
}
