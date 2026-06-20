-- 0004_retention_indexes: index changes.snapshot_id for the retention sweep.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- The Layer 2 retention delete (store/retention.go) keeps any snapshot row that
-- recorded a change via NOT EXISTS (SELECT 1 FROM changes WHERE snapshot_id = ?).
-- changes is indexed by (url_id, detected_at) but not by snapshot_id alone, so the
-- existence probe would scan. This index makes it a lookup.

CREATE INDEX idx_changes_snapshot_id ON changes(snapshot_id);
