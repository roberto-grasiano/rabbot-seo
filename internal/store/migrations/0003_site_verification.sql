-- 0003_site_verification: add proof-of-control columns to the sites table.
-- Forward-only. Applied in a transaction with all other pending migrations.
--
-- DECISION: the proof record lives as columns ON sites (NOT a side table). It is
-- strictly 1:1 with a site, and the Phase 4 throttle resolver plus the
-- status/inspect read paths already SELECT from sites — single-table reads avoid
-- a JOIN on the scheduler hot path, and AUTOINCREMENT/CASCADE already tie
-- everything to sites(id). A 1:1 side-table would add a JOIN and a second write
-- for zero normalization benefit.
--
-- SECURITY: verification_state DEFAULT 'throttled' is the safe floor. Any
-- pre-existing site (added before verification existed) and any newly added site
-- reads back as throttled — the safe tier — until an explicit successful verify
-- flips it to 'verified' (or the operator skips to 'attested'). This is the
-- structural floor the Phase 4 throttle resolver reads. SQLite ADD COLUMN with a
-- constant DEFAULT backfills existing rows without a table rewrite.

ALTER TABLE sites ADD COLUMN verification_method TEXT      NOT NULL DEFAULT '';          -- '' | 'well_known' | 'dns' | 'meta'
ALTER TABLE sites ADD COLUMN verification_token  TEXT      NOT NULL DEFAULT '';          -- the krb_ token (public)
ALTER TABLE sites ADD COLUMN verification_state  TEXT      NOT NULL DEFAULT 'throttled'; -- 'verified' | 'attested' | 'throttled'
ALTER TABLE sites ADD COLUMN verified_at         TIMESTAMP;                              -- NULL until a successful verify
ALTER TABLE sites ADD COLUMN last_reverified_at  TIMESTAMP;                              -- NULL until the first (re)verify
