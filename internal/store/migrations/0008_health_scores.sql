-- 0008_health_scores.sql — A6 health-score rollup history (site + segment, over time).
--
-- One row per (site, segment, change-in-score). segment_id NULL means the whole
-- site. Masses are the canonical integers; score is the derived 0..100 value,
-- recomputable from impact_mass/max_mass for explainability. breakdown is the
-- UNCAPPED per-rule mass JSON, used for ranking attribution only (the score math
-- uses the capped masses). computed_at is always stored UTC.

CREATE TABLE health_scores (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id       INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    segment_id    INTEGER REFERENCES segments(id) ON DELETE CASCADE, -- NULL = whole site
    computed_at   TIMESTAMP NOT NULL,            -- always UTC
    score         REAL    NOT NULL,              -- derived; masses are canonical
    impact_mass   INTEGER NOT NULL,
    max_mass      INTEGER NOT NULL,
    page_count    INTEGER NOT NULL,
    open_critical INTEGER NOT NULL,
    open_warning  INTEGER NOT NULL,
    open_info     INTEGER NOT NULL,
    breakdown     TEXT NOT NULL DEFAULT '{}'     -- {"rule_id": raw mass} for ranking
);
CREATE INDEX idx_health_scores_scope_time
    ON health_scores(site_id, segment_id, computed_at DESC);
