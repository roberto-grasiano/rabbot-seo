-- 0010_link_edges.sql — A9 link-graph LITE: the internal-link edge set + BFS depth.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- A9 ("ship the questions, not the graph") persists the deduped, absolute,
-- fragment-stripped, same-host internal-link set the extractor already returns
-- (the []string consumed once for discovery and dropped today). The delta over
-- time IS the monitor signal (page_orphaned / inlink_loss / click_depth_regression)
-- and the bounded get_link_graph export is the launch-demo asset.
--
-- Keyed by urls, NOT snapshots — snapshot retention (internal/store/retention.go,
-- DeleteStaleSnapshots) must never erode the graph. PRAGMA foreign_keys = ON is
-- set (internal/store/sqlite.go) so deleting a site cascades its edges, and
-- deleting a source URL cascades that page's out-edges.
--
-- to_url is TEXT, NOT a foreign key: a linked-but-never-admitted target
-- (page-cap overflow in the discoverer) stays a valid question subject. Targets
-- and the strings discovery admits share one extraction path, so exact-string
-- joins against urls.url line up.
--
-- URL identity is EXACT-STRING (fragment-stripped only): /a, /a/, and /a?utm=…
-- are three distinct nodes — a documented LITE limitation. urlx normalization is
-- a separate future change (it would move discovery identity with it).
--
-- PK (from_url_id, to_url): one row per directed edge; a page's out-edge set is
-- replaced incrementally per snapshot (SyncOutEdges), never recomputed wholesale.
-- The (site_id, to_url) index serves the inbound questions (what_links_to /
-- blast_radius / orphan inventory), which all scan by target.

CREATE TABLE link_edges (
    site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    from_url_id INTEGER NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    to_url      TEXT    NOT NULL,   -- absolute, fragment-stripped, as extracted
    first_seen  TIMESTAMP NOT NULL, -- always UTC
    last_seen   TIMESTAMP NOT NULL, -- always UTC
    PRIMARY KEY (from_url_id, to_url)
);
CREATE INDEX idx_link_edges_site_to ON link_edges(site_id, to_url);

-- graph_depth: shortest click-depth from the site root over link_edges, written
-- back by the periodic BFS sweep. NULL = not yet swept (or unreachable from root)
-- — the click_depth_regression signal never fires on a NULL prior depth.
ALTER TABLE urls ADD COLUMN graph_depth INTEGER;
