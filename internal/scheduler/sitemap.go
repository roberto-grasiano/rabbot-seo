package scheduler

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// defaultPriority is the sitemap-protocol default <priority> (used for absent,
// empty, or unparseable/non-finite values).
const defaultPriority = 0.5

// SitemapEntry is one URL from a sitemap (or a child sitemap from an index).
type SitemapEntry struct {
	Loc      string
	Priority float64
}

// sitemapURL decodes <priority> as a raw string, not a float64: a single
// non-numeric value ("high", "0,5", "100%") would otherwise make encoding/xml's
// ParseFloat error and abort xml.Unmarshal, discarding the ENTIRE document
// (fix #7). The raw text is parsed tolerantly per-URL by parsePriority.
type sitemapURL struct {
	Loc      string `xml:"loc"`
	Priority string `xml:"priority"`
}

// parsePriority tolerantly parses a raw <priority> value. Any value that is
// empty, unparseable as a Go float, non-positive, or non-finite (NaN/±Inf —
// strconv.ParseFloat accepts those tokens) falls back to the protocol default
// of 0.5. ok reports whether raw was a usable finite-positive number; the
// caller logs the !ok branch as a per-URL priority drop.
func parsePriority(raw string) (pri float64, ok bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return defaultPriority, false
	}
	return v, true
}

type urlset struct {
	XMLName xml.Name     `xml:"urlset"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapindex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	Sitemaps []sitemapURL `xml:"sitemap"`
}

// ParseSitemap parses a sitemap.xml or sitemap index. isIndex is true when the
// document is a <sitemapindex> (entries are child-sitemap URLs). Missing or
// malformed priorities default to 0.5 per the sitemap protocol (see fix #7).
func ParseSitemap(data []byte) (entries []SitemapEntry, isIndex bool, err error) {
	return ParseSitemapLog(data, nil)
}

// ParseSitemapLog is ParseSitemap with an optional logger: when log != nil,
// each <priority> that fails the tolerant parse (and is defaulted to 0.5) is
// recorded at Debug level — the per-URL drop branch. A nil logger disables that
// logging (the ParseSitemap entrypoint and tests). A single bad <priority>
// never aborts the document; only the offending URL's priority is defaulted.
func ParseSitemapLog(data []byte, log *slog.Logger) (entries []SitemapEntry, isIndex bool, err error) {
	data = maybeGunzip(data)
	trimmed := strings.TrimSpace(string(data))
	if strings.Contains(trimmed, "<sitemapindex") {
		var idx sitemapindex
		if err := xml.Unmarshal(data, &idx); err != nil {
			return nil, true, err
		}
		for _, s := range idx.Sitemaps {
			loc := strings.TrimSpace(s.Loc)
			if loc != "" {
				// Child-sitemap entries carry no meaningful per-document priority;
				// the protocol default keeps the discovery sort stable.
				entries = append(entries, SitemapEntry{Loc: loc, Priority: defaultPriority})
			}
		}
		return entries, true, nil
	}

	var us urlset
	if err := xml.Unmarshal(data, &us); err != nil {
		return nil, false, err
	}
	for _, u := range us.URLs {
		loc := strings.TrimSpace(u.Loc)
		if loc == "" {
			continue
		}
		// Tolerant per-URL parse: "high"/"0,5"/"100%"/empty and the non-finite
		// "NaN"/"Inf" tokens (which strconv.ParseFloat accepts and which slip past
		// a bare pri<=0 guard) all default to 0.5 instead of erroring the whole
		// document or reaching discovery's priority sort as a poison value.
		pri, ok := parsePriority(u.Priority)
		if !ok && log != nil {
			log.Debug("sitemap priority defaulted",
				obs.KeyComponent, "scheduler", obs.KeyURL, loc,
				"raw_priority", strings.TrimSpace(u.Priority), "priority", pri)
		}
		entries = append(entries, SitemapEntry{Loc: loc, Priority: pri})
	}
	return entries, false, nil
}

// ApplyBudget truncates entries to at most max (0 = unlimited).
func ApplyBudget(entries []SitemapEntry, max int) []SitemapEntry {
	if max <= 0 || len(entries) <= max {
		return entries
	}
	return entries[:max]
}

// maybeGunzip transparently decompresses a gzip-magic body (.xml.gz sitemaps are
// commonly served as raw application/gzip, which the HTTP transport does NOT
// auto-decompress — only Content-Encoding: gzip is transparent). Non-gzip input
// is returned unchanged.
func maybeGunzip(data []byte) []byte {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data
	}
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data
	}
	defer func() { _ = r.Close() }()
	dec, err := io.ReadAll(io.LimitReader(r, 64<<20)) // cap decompressed size
	if err != nil {
		return data
	}
	return dec
}
