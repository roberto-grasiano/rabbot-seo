package store

import (
	"context"
	"time"
)

// Package retention ops bound DB growth. They are CASCADE-safe: see the spec
// docs/superpowers/specs/2026-06-09-snapshot-retention-design.md. changes/issues
// are never pruned; any snapshot that recorded a change is never deleted.

// NullStaleRawHTML sets raw_html = NULL on every snapshot except the newest `keep`
// per URL. Stored raw_html has no reader and diff.Compare never consults it, so
// this reclaims the bulk of disk without affecting change detection. `keep` is
// floored to 1 so the newest snapshot per URL always retains its body regardless of
// the caller — a keep=0 must never null every row. Returns the number of rows nulled.
func (db *DB) NullStaleRawHTML(ctx context.Context, keep int) (int64, error) {
	if keep < 1 {
		keep = 1
	}
	const q = `
UPDATE snapshots SET raw_html = NULL
WHERE raw_html IS NOT NULL
  AND id IN (
    SELECT id FROM (
      SELECT id, ROW_NUMBER() OVER (PARTITION BY url_id ORDER BY fetched_at DESC, id DESC) AS rn
      FROM snapshots WHERE raw_html IS NOT NULL
    ) WHERE rn > ?
  )`
	var affected int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, q, keep)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

// TrimFileSnapshots keeps the newest `keep` file snapshots per (site_id, kind) and
// deletes the rest. file_snapshots has no child tables, so this is unconditionally
// safe. `keep` is floored to 2 so diff.CompareFile always has a prior to diff against,
// regardless of the caller — a keep<2 must never strip the file-diff baseline.
// Returns rows deleted.
func (db *DB) TrimFileSnapshots(ctx context.Context, keep int) (int64, error) {
	if keep < 2 {
		keep = 2
	}
	const q = `
DELETE FROM file_snapshots
WHERE id IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY site_id, kind ORDER BY fetched_at DESC, id DESC) AS rn
    FROM file_snapshots
  ) WHERE rn > ?
)`
	var affected int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, q, keep)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}

// DeleteStaleSnapshots deletes snapshot rows older than cutoff that are NOT among
// the newest `keep` per URL and that recorded NO changes. It is CASCADE-safe by
// construction: only change-less rows are eligible, so the changes-table cascade
// removes nothing of value, and the newest snapshot per URL (needed by
// LatestSnapshot for the next diff) is always retained. `keep` is floored to 1, so
// the newest snapshot per URL is always protected regardless of the caller — even if
// an unvalidated config supplies keep=0. Work is done in batches of `chunk` (≤0 →
// 5000), each in its own transaction, so the single writer is never held long enough
// to stall the crawl. Returns total rows deleted.
func (db *DB) DeleteStaleSnapshots(ctx context.Context, cutoff time.Time, keep, chunk int) (int64, error) {
	if keep < 1 {
		keep = 1
	}
	if chunk <= 0 {
		chunk = 5000
	}
	const q = `
DELETE FROM snapshots
WHERE id IN (
  SELECT s.id FROM snapshots s
  WHERE s.fetched_at < ?
    AND s.id NOT IN (
      SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (PARTITION BY url_id ORDER BY fetched_at DESC, id DESC) AS rn
        FROM snapshots
      ) WHERE rn <= ?
    )
    AND NOT EXISTS (SELECT 1 FROM changes c WHERE c.snapshot_id = s.id)
  LIMIT ?
)`
	var total int64
	for {
		var n int64
		err := db.WriteTx(ctx, func(tx Tx) error {
			res, err := tx.ExecContext(ctx, q, cutoff, keep, chunk)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(chunk) {
			break // last (partial or empty) batch
		}
	}
	return total, nil
}

// RetentionPolicy is the resolved set of retention knobs for one sweep.
type RetentionPolicy struct {
	RawHTMLKeep       int           // newest snapshots per URL that retain raw_html (≥1)
	SnapshotMaxAge    time.Duration // Layer 2 cutoff age; ≤0 disables Layer 2
	FileSnapshotsKeep int           // newest robots/sitemap snapshots per (site,kind)
	Chunk             int           // Layer 2 batch size; ≤0 → 5000
}

// RetentionResult reports what one sweep removed.
type RetentionResult struct {
	RawHTMLNulled        int64
	SnapshotsDeleted     int64
	FileSnapshotsTrimmed int64
}

// ApplyRetention runs all retention layers in order against `now`. Layer 1 (null
// stale raw_html) always runs when RawHTMLKeep ≥ 1; Layer 2 (delete change-less,
// non-latest old rows) runs only when SnapshotMaxAge > 0; the file-snapshot trim
// runs when FileSnapshotsKeep ≥ 1. Each layer is independently transactional, so a
// later-layer error still leaves earlier layers committed (and reported).
func (db *DB) ApplyRetention(ctx context.Context, p RetentionPolicy, now time.Time) (RetentionResult, error) {
	var r RetentionResult
	var err error
	if p.RawHTMLKeep >= 1 {
		if r.RawHTMLNulled, err = db.NullStaleRawHTML(ctx, p.RawHTMLKeep); err != nil {
			return r, err
		}
	}
	if p.SnapshotMaxAge > 0 {
		cutoff := now.Add(-p.SnapshotMaxAge)
		if r.SnapshotsDeleted, err = db.DeleteStaleSnapshots(ctx, cutoff, p.RawHTMLKeep, p.Chunk); err != nil {
			return r, err
		}
	}
	if p.FileSnapshotsKeep >= 1 {
		if r.FileSnapshotsTrimmed, err = db.TrimFileSnapshots(ctx, p.FileSnapshotsKeep); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Compact runs VACUUM to rebuild the database file, returning freelist pages to the
// OS. VACUUM cannot run inside a transaction, so it executes directly on the writer
// pool (autocommit), not via WriteTx. It briefly takes an exclusive lock; callers
// must ensure no other process holds the database (the `db compact` CLI guard
// refuses to run while the daemon is up).
func (db *DB) Compact(ctx context.Context) error {
	_, err := db.writeDB.ExecContext(ctx, "VACUUM")
	return err
}
