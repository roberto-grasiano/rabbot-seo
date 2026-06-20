-- 0009_render_mode.sql — A8 hydration/render-mode classification, per snapshot.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- A8 (hydration extraction) records HOW each page delivers its SEO content:
--   * render_mode mirrors precheck.RenderKind's HINT values
--     (server_rendered | hydrated | head_only_shell | client_shell | unknown).
--   * extraction_source records the provenance composition of the snapshot's
--     extracted fields (e.g. "dom", "dom+next_data", "dom+flight").
--
-- Both default to '' so the upgrade path is value-correct without a data
-- migration: every pre-A8 snapshot row backfills to '' on ALTER. The empty
-- render_mode reads back as model.RenderMode("") and surfaces as "unknown"
-- on render surfaces (the zero value is deliberately the Unknown sentinel).

ALTER TABLE snapshots ADD COLUMN render_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN extraction_source TEXT NOT NULL DEFAULT '';
