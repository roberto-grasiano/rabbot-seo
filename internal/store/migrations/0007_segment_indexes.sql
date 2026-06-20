-- A7 segments: add the uniqueness + reverse-lookup indexes the wiring needs.
-- No table edits — the segments / url_segments tables shipped in 0001.
--
-- idx_segments_site_name makes SyncSiteSegments' upsert by (site_id, name)
-- deterministic: a segment name is unique within a site, so a config edit that
-- re-patterns "content" updates the one row instead of inserting a duplicate.
CREATE UNIQUE INDEX idx_segments_site_name ON segments(site_id, name);

-- The url_segments composite PK is (url_id, segment_id) — url_id-first — so it
-- cannot serve a segment-filtered read ("which URLs are in segment N"). This
-- reverse-direction index covers segment_id-first lookups for the read surfaces
-- and member-count queries.
CREATE INDEX idx_url_segments_segment_id ON url_segments(segment_id);
