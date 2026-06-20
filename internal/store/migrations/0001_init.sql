-- 0001_init.sql — initial Rabbot-SEO schema (lean MVP; no cwv/enrichment).

CREATE TABLE sites (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    base_url        TEXT    NOT NULL UNIQUE,
    name            TEXT    NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    min_interval    INTEGER NOT NULL DEFAULT 600,
    max_interval    INTEGER NOT NULL DEFAULT 86400,
    max_concurrency INTEGER NOT NULL DEFAULT 2,
    speed_scale     INTEGER NOT NULL DEFAULT 100,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL
);

CREATE TABLE urls (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id          INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    url              TEXT    NOT NULL,
    first_seen       TIMESTAMP NOT NULL,
    last_checked     TIMESTAMP,
    next_check_at    TIMESTAMP NOT NULL,
    interval         INTEGER NOT NULL DEFAULT 600,
    importance       REAL    NOT NULL DEFAULT 0,
    depth            INTEGER NOT NULL DEFAULT 0,
    in_sitemap       INTEGER NOT NULL DEFAULT 0,
    status_type      TEXT    NOT NULL DEFAULT 'page',
    etag             TEXT    NOT NULL DEFAULT '',
    last_modified    TEXT    NOT NULL DEFAULT '',
    last_fetch_class TEXT    NOT NULL DEFAULT 'ok',
    UNIQUE(site_id, url)
);
CREATE INDEX idx_urls_next_check_at ON urls(next_check_at);
CREATE INDEX idx_urls_site_id ON urls(site_id);

CREATE TABLE snapshots (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    url_id                   INTEGER NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    fetched_at               TIMESTAMP NOT NULL,
    http_status              INTEGER NOT NULL DEFAULT 0,
    redirect_chain           TEXT    NOT NULL DEFAULT '[]',
    response_time_ms         INTEGER NOT NULL DEFAULT 0,
    title                    TEXT    NOT NULL DEFAULT '',
    meta_description         TEXT    NOT NULL DEFAULT '',
    meta_robots              TEXT    NOT NULL DEFAULT '',
    x_robots_tag             TEXT    NOT NULL DEFAULT '',
    canonical                TEXT    NOT NULL DEFAULT '',
    canonical_type           TEXT    NOT NULL DEFAULT '',
    hreflang                 TEXT    NOT NULL DEFAULT '[]',
    headings                 TEXT    NOT NULL DEFAULT '{}',
    word_count               INTEGER NOT NULL DEFAULT 0,
    content_sha256           TEXT    NOT NULL DEFAULT '',
    content_simhash          INTEGER NOT NULL DEFAULT 0,
    jsonld                   TEXT    NOT NULL DEFAULT '[]',
    schema_types             TEXT    NOT NULL DEFAULT '[]',
    internal_link_count      INTEGER NOT NULL DEFAULT 0,
    external_link_count      INTEGER NOT NULL DEFAULT 0,
    incoming_canonical_count INTEGER NOT NULL DEFAULT 0,
    incoming_redirect_count  INTEGER NOT NULL DEFAULT 0,
    image_count              INTEGER NOT NULL DEFAULT 0,
    missing_alt_count        INTEGER NOT NULL DEFAULT 0,
    og                       TEXT    NOT NULL DEFAULT '{}',
    twitter                  TEXT    NOT NULL DEFAULT '{}',
    indexable                INTEGER NOT NULL DEFAULT 0,
    indexability_reason      TEXT    NOT NULL DEFAULT '',
    raw_html                 BLOB
);
CREATE INDEX idx_snapshots_url_id_fetched_at ON snapshots(url_id, fetched_at DESC);

CREATE TABLE changes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    url_id       INTEGER NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    snapshot_id  INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    field        TEXT    NOT NULL,
    old_value    TEXT    NOT NULL DEFAULT '',
    new_value    TEXT    NOT NULL DEFAULT '',
    change_class TEXT    NOT NULL DEFAULT 'substantive',
    detected_at  TIMESTAMP NOT NULL
);
CREATE INDEX idx_changes_url_id_detected_at ON changes(url_id, detected_at DESC);

CREATE TABLE issues (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    url_id        INTEGER NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    rule_id       TEXT    NOT NULL,
    status        TEXT    NOT NULL DEFAULT 'open',
    severity      TEXT    NOT NULL DEFAULT 'info',
    impact_points INTEGER NOT NULL DEFAULT 0,
    opened_at     TIMESTAMP NOT NULL,
    closed_at     TIMESTAMP,
    last_seen_at  TIMESTAMP NOT NULL,
    detail        TEXT    NOT NULL DEFAULT '{}',
    UNIQUE(url_id, rule_id)
);
CREATE INDEX idx_issues_status ON issues(status);

CREATE TABLE alerts (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id           INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    fingerprint       TEXT    NOT NULL,
    group_key         TEXT    NOT NULL,
    severity          TEXT    NOT NULL DEFAULT 'info',
    status            TEXT    NOT NULL DEFAULT 'open',
    affected_count    INTEGER NOT NULL DEFAULT 0,
    first_detected_at TIMESTAMP NOT NULL,
    last_updated_at   TIMESTAMP NOT NULL,
    last_notified_at  TIMESTAMP,
    auto_closed_at    TIMESTAMP,
    payload_summary   TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint);
CREATE INDEX idx_alerts_site_id_status ON alerts(site_id, status);

CREATE TABLE segments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    match_rule TEXT    NOT NULL
);
CREATE INDEX idx_segments_site_id ON segments(site_id);

CREATE TABLE url_segments (
    url_id     INTEGER NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
    PRIMARY KEY (url_id, segment_id)
);

CREATE TABLE file_snapshots (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id        INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    kind           TEXT    NOT NULL,
    fetched_at     TIMESTAMP NOT NULL,
    content_sha256 TEXT    NOT NULL DEFAULT '',
    parsed_entries TEXT    NOT NULL DEFAULT '{}',
    http_status    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_file_snapshots_site_kind ON file_snapshots(site_id, kind, fetched_at DESC);
