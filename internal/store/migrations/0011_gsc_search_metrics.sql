-- 0011_gsc_search_metrics.sql — GSC W1 storage: Search Analytics + URL Inspection.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- The first "intelligence" slice persists Google Search Console's ground truth so
-- W2 can correlate it against Rabbot's own crawl history. W1 is PLUMBING ONLY:
-- these two tables + the upsert/read repo methods. No signals/rules/alerts here.
--
--   search_metrics — searchAnalytics.query rows at the (page, query, date) grain:
--     clicks / impressions / ctr / position. dataState=final is the puller's
--     concern (the latest ~3 days are partial); the store records whatever date is
--     handed in. UNIQUE(site_id, url, query, date) makes a re-pull / backfill of
--     the same day idempotent (ON CONFLICT ... DO UPDATE refreshes the metrics).
--
--   url_index_status — urlInspection.index.inspect's latest per-URL verdict:
--     verdict / coverage_state / indexing_state / robots_txt_state /
--     page_fetch_state / google_canonical / user_canonical / crawled_as +
--     last_crawl_time. UNIQUE(site_id, url) keeps ONE latest status per URL
--     (the upsert overwrites the prior inspection — this is a "current state"
--     table, not an append-only history).
--
-- Keying is (site_id, url) — NOT a urls.id foreign key. GSC reports URLs Google
-- knows about, which may not be admitted urls rows (the link_edges.to_url
-- precedent: "a linked-but-never-admitted target stays a valid question subject").
-- url is TEXT in the same canonical keyspace urls.url / link_edges.to_url use
-- (the repo applies the shared canonicalizer at the write boundary), so W2's
-- index_status_discrepancy / google_canonical_mismatch signals can join GSC rows
-- back to Rabbot pages by exact string. site_id REFERENCES sites(id) with
-- ON DELETE CASCADE so removing a site drops its GSC rows (PRAGMA foreign_keys=ON,
-- internal/store/sqlite.go).
--
-- All timestamps are TIMESTAMP affinity bound as UTC time.Time (the schema_migrations
-- + snapshots precedent), NEVER RFC3339 strings: lexical range scans on the indexes
-- below stay instant-correct across zones. date is a plain TEXT 'YYYY-MM-DD' day
-- bucket from GSC (a calendar day, not an instant), kept as the API returns it.
--
-- Indexes mirror 0004_retention_indexes / 0007_segment_indexes: the (site_id, date)
-- index serves the time-windowed search-performance reads; (site_id, url) on the
-- inspection table is the per-URL latest-status lookup. The two UNIQUE constraints
-- already create covering indexes for their exact key tuples; the extra
-- idx_search_metrics_site_date supports date-range scans that ignore query/url.

CREATE TABLE IF NOT EXISTS search_metrics (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    url         TEXT    NOT NULL,                 -- canonical, same keyspace as urls.url
    query       TEXT    NOT NULL,                 -- the search query string
    date        TEXT    NOT NULL,                 -- 'YYYY-MM-DD' GSC day bucket
    clicks      INTEGER NOT NULL DEFAULT 0,
    impressions INTEGER NOT NULL DEFAULT 0,
    ctr         REAL    NOT NULL DEFAULT 0,
    position    REAL    NOT NULL DEFAULT 0,
    UNIQUE(site_id, url, query, date)             -- idempotent re-pull/backfill
);
CREATE INDEX IF NOT EXISTS idx_search_metrics_site_date ON search_metrics(site_id, date);

CREATE TABLE IF NOT EXISTS url_index_status (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id          INTEGER   NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    url              TEXT      NOT NULL,           -- canonical, same keyspace as urls.url
    inspected_at     TIMESTAMP NOT NULL,           -- UTC, when Rabbot pulled the inspection
    verdict          TEXT      NOT NULL DEFAULT '',
    coverage_state   TEXT      NOT NULL DEFAULT '',
    indexing_state   TEXT      NOT NULL DEFAULT '',
    robots_txt_state TEXT      NOT NULL DEFAULT '',
    page_fetch_state TEXT      NOT NULL DEFAULT '',
    google_canonical TEXT      NOT NULL DEFAULT '',
    user_canonical   TEXT      NOT NULL DEFAULT '',
    crawled_as       TEXT      NOT NULL DEFAULT '',
    last_crawl_time  TIMESTAMP,                     -- UTC; NULL when Google reports none
    UNIQUE(site_id, url)                            -- one latest status per URL (upsert)
);
CREATE INDEX IF NOT EXISTS idx_url_index_status_site_url ON url_index_status(site_id, url);
