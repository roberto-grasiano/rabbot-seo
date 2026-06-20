package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// sitemapReconcileChunk bounds the number of URL strings bound into one IN-list
// UPDATE. SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999 on older builds and
// 32766 on modern ones; the freshly collected loc set can exceed either on an
// unbounded (max_pages_per_site: 0) site, so ReconcileSitemapMembership chunks
// the membership UPDATE to stay safely under the conservative limit. One spare
// binding (site_id) is reserved, so the chunk size sits below 999.
const sitemapReconcileChunk = 900

// sitemapCoverageSampleLimit caps the per-bucket sample URL list surfaced by
// SitemapCoverage. The read model carries exact counts; the samples are an
// at-a-glance aid, so a small fixed cap keeps the DTO bounded.
const sitemapCoverageSampleLimit = 10

// ReconcileSitemapMembership reconciles urls.in_sitemap against the freshly
// collected sitemap loc set for one site, inside a single write transaction.
//
//   - Rows whose url is present in locs are set in_sitemap=1.
//   - Rows whose url is absent from locs are set in_sitemap=0 — UNLESS
//     additiveOnly is true (an incomplete/partial collection), in which case the
//     flip-off pass is skipped so a truncated read can never masquerade as a mass
//     URL drop.
//
// Scheduling columns (next_check_at / interval / importance) and every other
// column are untouched — only in_sitemap is written. The "set present" UPDATE is
// chunked over locs to stay under SQLite's bind-variable ceiling; the "clear
// absent" UPDATE uses a NOT IN over the same chunks so it likewise never binds
// the whole set at once.
func (db *DB) ReconcileSitemapMembership(ctx context.Context, siteID int64, locs []string, additiveOnly bool) error {
	// De-duplicate locs up front so chunk boundaries don't repeat work and the
	// NOT-IN "clear absent" pass is computed against a stable membership set.
	seen := make(map[string]struct{}, len(locs))
	uniq := make([]string, 0, len(locs))
	for _, l := range locs {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		uniq = append(uniq, l)
	}

	return db.WriteTx(ctx, func(tx Tx) error {
		// 1) Set in_sitemap=1 for present rows, chunked.
		for _, chunk := range chunkStrings(uniq, sitemapReconcileChunk) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, siteID)
			for _, u := range chunk {
				args = append(args, u)
			}
			q := `UPDATE urls SET in_sitemap = 1 WHERE site_id = ? AND in_sitemap = 0 AND url IN (` +
				placeholders(len(chunk)) + `)`
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}

		// 2) Clear in_sitemap=0 for absent rows — skipped on a partial read.
		if additiveOnly {
			return nil
		}
		if len(uniq) == 0 {
			// Empty (complete) set: every flagged row is now absent → clear all.
			_, err := tx.ExecContext(ctx,
				`UPDATE urls SET in_sitemap = 0 WHERE site_id = ? AND in_sitemap = 1`, siteID)
			return err
		}
		// A single multi-chunk NOT IN over (A AND B AND …) is the set complement:
		// a row survives all chunks' NOT-IN only if it is in none of them, i.e.
		// absent from the whole set. Apply each chunk's NOT-IN narrowing in turn.
		//
		// We cannot express "NOT IN (full set)" in one statement under the bind
		// ceiling, so instead collect the ids to clear with a chunked membership
		// probe, then clear them. Build the keep-set check per chunk.
		return clearAbsentInSitemap(ctx, tx, siteID, uniq)
	})
}

// clearAbsentInSitemap sets in_sitemap=0 for every currently-flagged row of the
// site whose url is NOT in the keep set. It walks the flagged rows in batches and
// tests membership against the in-memory keep set, so it never binds the whole
// loc set into a single statement.
func clearAbsentInSitemap(ctx context.Context, tx Tx, siteID int64, keep []string) error {
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}

	// Collect the urls currently flagged in_sitemap=1 for this site, retaining
	// only those absent from the keep set.
	toClear, err := flaggedURLsAbsentFrom(ctx, tx, siteID, keepSet)
	if err != nil {
		return err
	}

	for _, chunk := range chunkStrings(toClear, sitemapReconcileChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, siteID)
		for _, u := range chunk {
			args = append(args, u)
		}
		q := `UPDATE urls SET in_sitemap = 0 WHERE site_id = ? AND in_sitemap = 1 AND url IN (` +
			placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// flaggedURLsAbsentFrom returns the urls of rows currently flagged in_sitemap=1
// for the site that are NOT present in keepSet.
func flaggedURLsAbsentFrom(ctx context.Context, tx Tx, siteID int64, keepSet map[string]struct{}) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT url FROM urls WHERE site_id = ? AND in_sitemap = 1`, siteID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var toClear []string
	for rows.Next() {
		var u string
		if scanErr := rows.Scan(&u); scanErr != nil {
			return nil, scanErr
		}
		if _, ok := keepSet[u]; !ok {
			toClear = append(toClear, u)
		}
	}
	return toClear, rows.Err()
}

// chunkStrings splits s into sub-slices of at most size elements.
func chunkStrings(s []string, size int) [][]string {
	if size <= 0 || len(s) == 0 {
		if len(s) == 0 {
			return nil
		}
		return [][]string{s}
	}
	var out [][]string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// placeholders returns "?,?,…,?" with n placeholders (n >= 1).
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// ─── Coverage read model ──────────────────────────────────────────────────

// sitemapCoverageBlock is the coverage object persisted inside a
// FileKindSitemap snapshot's ParsedEntries JSON. sitemapped_unadmitted (declared
// in the sitemap but never admitted into the urls inventory — page-cap
// exhaustion, same-host/SSRF rejects) cannot be derived from the urls table, so
// it is computed at refresh time and read back from here.
type sitemapCoverageBlock struct {
	SitemappedUncrawled  int `json:"sitemapped_uncrawled"`
	SitemappedUnadmitted int `json:"sitemapped_unadmitted"`
	CrawledNotInSitemap  int `json:"crawled_not_in_sitemap"`
}

// sitemapParsedEntries is the versioned JSON document stored in a sitemap
// snapshot's ParsedEntries. Only the fields SitemapCoverage needs are modeled.
type sitemapParsedEntries struct {
	Coverage sitemapCoverageBlock `json:"coverage"`
}

// SitemapCoverageResult is the coverage read model surfaced by the CLI verb,
// control endpoint, and MCP tool. Counts are exact (live SQL for the urls-derived
// buckets, the persisted snapshot block for the unadmitted bucket); the Sample*
// lists are bounded at sitemapCoverageSampleLimit. A site with no persisted
// sitemap snapshot returns the zero value with HasSitemap=false.
type SitemapCoverageResult struct {
	HasSitemap bool `json:"has_sitemap"`
	SeedStatus int  `json:"seed_status"`

	SitemappedUncrawled  int `json:"sitemapped_uncrawled"`
	SitemappedUnadmitted int `json:"sitemapped_unadmitted"`
	CrawledNotInSitemap  int `json:"crawled_not_in_sitemap"`

	SampleUncrawled    []string `json:"sample_uncrawled"`
	SampleNotInSitemap []string `json:"sample_not_in_sitemap"`
}

// SitemapCoverage combines the two pure-SQL urls-derived counts
// (sitemapped_uncrawled = in_sitemap=1 AND last_checked IS NULL;
// crawled_not_in_sitemap = in_sitemap=0 AND last_checked IS NOT NULL), the latest
// FileKindSitemap snapshot's coverage block (for sitemapped_unadmitted + seed
// status), and a bounded sample of URLs per bucket. A site without a sitemap
// snapshot yields the zero value with HasSitemap=false (the urls-derived counts
// are also reported as zero — without a watched sitemap, drift is undefined). It
// performs no writes; mirrors the BuildReport read pattern.
func (db *DB) SitemapCoverage(ctx context.Context, siteID int64) (SitemapCoverageResult, error) {
	var res SitemapCoverageResult

	latest, ok, err := db.LatestFileSnapshot(ctx, siteID, model.FileKindSitemap)
	if err != nil {
		return SitemapCoverageResult{}, err
	}
	if !ok {
		// No watched sitemap → zero value, has_sitemap=false.
		return SitemapCoverageResult{}, nil
	}
	res.HasSitemap = true
	res.SeedStatus = latest.HTTPStatus

	// sitemapped_unadmitted comes from the persisted coverage block. A malformed
	// or empty ParsedEntries decodes to a zero block rather than erroring — the
	// urls-derived counts remain authoritative.
	if latest.ParsedEntries != "" {
		var pe sitemapParsedEntries
		if jsonErr := json.Unmarshal([]byte(latest.ParsedEntries), &pe); jsonErr == nil {
			res.SitemappedUnadmitted = pe.Coverage.SitemappedUnadmitted
		}
	}

	if res.SitemappedUncrawled, err = db.countURLs(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND in_sitemap = 1 AND last_checked IS NULL`,
		siteID); err != nil {
		return SitemapCoverageResult{}, err
	}
	if res.CrawledNotInSitemap, err = db.countURLs(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND in_sitemap = 0 AND last_checked IS NOT NULL`,
		siteID); err != nil {
		return SitemapCoverageResult{}, err
	}

	if res.SampleUncrawled, err = db.sampleURLs(ctx,
		`SELECT url FROM urls WHERE site_id = ? AND in_sitemap = 1 AND last_checked IS NULL
		 ORDER BY url LIMIT ?`, siteID); err != nil {
		return SitemapCoverageResult{}, err
	}
	if res.SampleNotInSitemap, err = db.sampleURLs(ctx,
		`SELECT url FROM urls WHERE site_id = ? AND in_sitemap = 0 AND last_checked IS NOT NULL
		 ORDER BY url LIMIT ?`, siteID); err != nil {
		return SitemapCoverageResult{}, err
	}

	return res, nil
}

// SitemapLiveCountsResult are the urls-derived coverage counts computed live from
// the current inventory. It is the store's own struct (store cannot import
// scheduler — that would cycle, since scheduler imports store); the supervisor
// wiring adapts it to scheduler.SitemapLiveCounts.
type SitemapLiveCountsResult struct {
	SitemappedUncrawled int
	CrawledNotInSitemap int
	InSitemapTotal      int
}

// SitemapLiveCounts computes the urls-derived coverage counts live from the
// current inventory: sitemapped-but-uncrawled (in_sitemap=1 AND last_checked IS
// NULL), crawled-but-absent (in_sitemap=0 AND last_checked IS NOT NULL), and the
// total rows flagged in_sitemap=1. The sitemap watch (SideTimers.RefreshSitemap)
// calls this AFTER reconciling membership and BEFORE persisting the new snapshot —
// so unlike SitemapCoverage (which reads the latest persisted snapshot block) it
// returns real counts even on the very first pass, when no snapshot exists yet.
// InSitemapTotal lets the watch derive sitemapped_unadmitted = |declared locs| −
// InSitemapTotal. It performs no writes.
func (db *DB) SitemapLiveCounts(ctx context.Context, siteID int64) (SitemapLiveCountsResult, error) {
	var c SitemapLiveCountsResult
	var err error
	if c.SitemappedUncrawled, err = db.countURLs(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND in_sitemap = 1 AND last_checked IS NULL`,
		siteID); err != nil {
		return SitemapLiveCountsResult{}, err
	}
	if c.CrawledNotInSitemap, err = db.countURLs(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND in_sitemap = 0 AND last_checked IS NOT NULL`,
		siteID); err != nil {
		return SitemapLiveCountsResult{}, err
	}
	if c.InSitemapTotal, err = db.countURLs(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND in_sitemap = 1`,
		siteID); err != nil {
		return SitemapLiveCountsResult{}, err
	}
	return c, nil
}

// countURLs runs a scalar COUNT(*) query bound to siteID.
func (db *DB) countURLs(ctx context.Context, q string, siteID int64) (int, error) {
	var n int
	if err := db.Read().QueryRowContext(ctx, q, siteID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// sampleURLs runs a bucket query (bound siteID + LIMIT) and returns the urls.
func (db *DB) sampleURLs(ctx context.Context, q string, siteID int64) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, q, siteID, sitemapCoverageSampleLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
