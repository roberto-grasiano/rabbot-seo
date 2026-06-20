-- 0006_jsonld_invalid_count: count of JSON-LD <script> blocks that failed to
-- parse during extraction, per snapshot.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- A4 (structured-data validation) hardens the extractor so one malformed JSON-LD
-- block no longer voids the whole snapshots.jsonld column: only parseable blocks
-- are stored, and the number of rejected blocks is recorded here. The
-- structured_data_invalid_json rule fails (warning) while this is > 0, surfacing
-- markup that no parser — Rabbot's or a search engine's — can read.
--
-- NOT NULL DEFAULT 0 backfills every existing snapshot row to 0 on ALTER, so the
-- upgrade path is value-correct without a data migration.

ALTER TABLE snapshots ADD COLUMN jsonld_invalid_count INTEGER NOT NULL DEFAULT 0;
