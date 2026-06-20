package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// GSC W1 store half: the search_metrics + url_index_status tables (migration
// 0011) and their upsert/read repo methods. PLUMBING ONLY — no signals/rules
// (index_status_discrepancy / google_canonical_mismatch / search_performance_shift
// are W2). The puller (internal/gsc) writes through these; W2's signal layer reads.
//
// URL identity uses the SHARED canonicalizer (canonicalURL, linkgraph.go) at the
// write boundary, exactly like UpsertURL and SyncOutEdges, so GSC rows land in the
// same keyspace as urls.url / link_edges.to_url. That is the join W2's signals
// depend on: "is this the canonical Google chose vs the one we declared" needs the
// GSC google_canonical/user_canonical strings to line up with the page's stored
// URL. Reads canonicalize the lookup key too, so a divergent-but-equivalent caller
// URL resolves the stored row. Per the link_edges precedent, the url column is
// TEXT (not a urls.id FK): GSC reports URLs Google knows about that Rabbot may not
// have admitted as urls rows, and those stay valid GSC subjects.
//
// All timestamps are stored UTC and re-stamped UTC on read (the lastcrawl.go
// discipline); date is the GSC 'YYYY-MM-DD' calendar-day bucket kept verbatim.

// SaveSearchMetrics upserts a batch of searchAnalytics.query rows in ONE write
// transaction. Each row keys on (site_id, url, query, date); a re-pull or backfill
// of the same grain REPLACES the metrics (ON CONFLICT DO UPDATE), so the daily
// pull is idempotent and the partial→final correction of a recent day overwrites
// cleanly. url is canonicalized at the write boundary so it matches urls.url.
//
// An empty/nil batch is a clean no-op (no transaction is opened) — the puller may
// legitimately have nothing to store for a property/day. The whole batch commits
// or rolls back together: a single bad row fails the pull rather than leaving a
// half-written day.
func (db *DB) SaveSearchMetrics(ctx context.Context, metrics []model.SearchMetric) error {
	if len(metrics) == 0 {
		return nil
	}
	return db.WriteTx(ctx, func(tx Tx) error {
		for _, m := range metrics {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO search_metrics (site_id, url, query, date, clicks, impressions, ctr, position)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(site_id, url, query, date) DO UPDATE SET
				   clicks = excluded.clicks,
				   impressions = excluded.impressions,
				   ctr = excluded.ctr,
				   position = excluded.position`,
				m.SiteID, canonicalURL(m.URL), m.Query, m.Date,
				m.Clicks, m.Impressions, m.CTR, m.Position); err != nil {
				return fmt.Errorf("upsert search metric (site=%d url=%q query=%q date=%q): %w",
					m.SiteID, m.URL, m.Query, m.Date, err)
			}
		}
		return nil
	})
}

// SearchMetricsForURL returns the stored (query, date) search-performance rows for
// one canonical URL within siteID, filtered to date >= since (a zero since returns
// all stored history). since is compared as a 'YYYY-MM-DD' day string (lexical
// order matches chronological order for that format); only the date PORTION is
// used, so any instant within a day includes that whole day. Rows are ordered
// date DESC, query ASC for a deterministic, newest-first read. The lookup URL is
// canonicalized so it matches the write keyspace.
func (db *DB) SearchMetricsForURL(ctx context.Context, siteID int64, url string, since time.Time) ([]model.SearchMetric, error) {
	canon := canonicalURL(url)
	q := `SELECT id, site_id, url, query, date, clicks, impressions, ctr, position
	        FROM search_metrics
	       WHERE site_id = ? AND url = ?`
	args := []any{siteID, canon}
	if !since.IsZero() {
		q += ` AND date >= ?`
		args = append(args, since.UTC().Format("2006-01-02"))
	}
	q += ` ORDER BY date DESC, query ASC`

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search metrics (site=%d url=%q): %w", siteID, url, err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.SearchMetric
	for rows.Next() {
		var m model.SearchMetric
		if scanErr := rows.Scan(&m.ID, &m.SiteID, &m.URL, &m.Query, &m.Date,
			&m.Clicks, &m.Impressions, &m.CTR, &m.Position); scanErr != nil {
			return nil, fmt.Errorf("scan search metric: %w", scanErr)
		}
		out = append(out, m)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate search metrics: %w", rowsErr)
	}
	return out, nil
}

// UpsertURLIndexStatus writes the latest urlInspection.index.inspect result for one
// URL, keyed (site_id, url): a fresh inspection REPLACES the prior one (ON CONFLICT
// DO UPDATE) so the table holds one current status per URL rather than an
// append-only history. url is canonicalized at the write boundary so it matches
// urls.url. InspectedAt is stored UTC; LastCrawlTime is stored UTC when set and
// SQL NULL when nil (Google reported no last crawl).
func (db *DB) UpsertURLIndexStatus(ctx context.Context, s model.URLIndexStatus) error {
	var lastCrawl any
	if s.LastCrawlTime != nil {
		lastCrawl = s.LastCrawlTime.UTC()
	}
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO url_index_status
			   (site_id, url, inspected_at, verdict, coverage_state, indexing_state,
			    robots_txt_state, page_fetch_state, google_canonical, user_canonical,
			    crawled_as, last_crawl_time)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(site_id, url) DO UPDATE SET
			   inspected_at = excluded.inspected_at,
			   verdict = excluded.verdict,
			   coverage_state = excluded.coverage_state,
			   indexing_state = excluded.indexing_state,
			   robots_txt_state = excluded.robots_txt_state,
			   page_fetch_state = excluded.page_fetch_state,
			   google_canonical = excluded.google_canonical,
			   user_canonical = excluded.user_canonical,
			   crawled_as = excluded.crawled_as,
			   last_crawl_time = excluded.last_crawl_time`,
			s.SiteID, canonicalURL(s.URL), s.InspectedAt.UTC(), s.Verdict, s.CoverageState,
			s.IndexingState, s.RobotsTxtState, s.PageFetchState, s.GoogleCanonical,
			s.UserCanonical, s.CrawledAs, lastCrawl)
		if err != nil {
			return fmt.Errorf("upsert url index status (site=%d url=%q): %w", s.SiteID, s.URL, err)
		}
		return nil
	})
}

// LatestURLIndexStatus returns the stored index status for one canonical URL within
// siteID. ok is false (zero value, no error) when the URL has never been inspected
// — the LatestFileSnapshot not-found contract. The lookup URL is canonicalized so
// it matches the write keyspace. Timestamps read back UTC; last_crawl_time stays
// nil when SQL NULL.
func (db *DB) LatestURLIndexStatus(ctx context.Context, siteID int64, url string) (model.URLIndexStatus, bool, error) {
	canon := canonicalURL(url)
	var (
		s         model.URLIndexStatus
		lastCrawl sql.NullTime
	)
	err := db.Read().QueryRowContext(ctx,
		`SELECT id, site_id, url, inspected_at, verdict, coverage_state, indexing_state,
		        robots_txt_state, page_fetch_state, google_canonical, user_canonical,
		        crawled_as, last_crawl_time
		   FROM url_index_status WHERE site_id = ? AND url = ?`,
		siteID, canon).Scan(
		&s.ID, &s.SiteID, &s.URL, &s.InspectedAt, &s.Verdict, &s.CoverageState, &s.IndexingState,
		&s.RobotsTxtState, &s.PageFetchState, &s.GoogleCanonical, &s.UserCanonical,
		&s.CrawledAs, &lastCrawl)
	if errors.Is(err, sql.ErrNoRows) {
		return model.URLIndexStatus{}, false, nil
	}
	if err != nil {
		return model.URLIndexStatus{}, false, fmt.Errorf("read url index status (site=%d url=%q): %w", siteID, url, err)
	}
	s.InspectedAt = s.InspectedAt.UTC()
	if lastCrawl.Valid {
		t := lastCrawl.Time.UTC()
		s.LastCrawlTime = &t
	}
	return s, true, nil
}
