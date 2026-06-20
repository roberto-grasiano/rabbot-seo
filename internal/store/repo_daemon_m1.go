package store

import (
	"context"
	"time"
)

// ─── Daemon / control-plane helpers ───────────────────────────────────────

// CountDueURLs returns how many enabled-site URLs are due for a recheck at now.
// The filter mirrors PopDueURLs exactly so status surfaces ("N due") agree with
// what the scheduler will actually pop.
func (db *DB) CountDueURLs(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM urls u JOIN sites s ON s.id = u.site_id
		 WHERE u.next_check_at <= ? AND s.enabled = 1`, now).Scan(&n)
	return n, err
}

// EnqueueRecheck forces matching URLs due now by setting next_check_at = now.
//
//   - target == "":  every URL belonging to an enabled site.
//   - otherwise:     the URL whose address equals target, plus every URL of the
//     site whose base_url equals target (so a site URL or a page URL both work)
//     — restricted to enabled sites, matching what PopDueURLs/CountDueURLs pop.
//
// It returns the number of URL rows updated.
func (db *DB) EnqueueRecheck(ctx context.Context, target string, now time.Time) (int, error) {
	var affected int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		var (
			q    string
			args []any
		)
		if target == "" {
			q = `UPDATE urls SET next_check_at = ?
			     WHERE site_id IN (SELECT id FROM sites WHERE enabled = 1)`
			args = []any{now}
		} else {
			// Scope to enabled sites only: PopDueURLs/CountDueURLs filter on
			// enabled = 1, so touching a disabled site's URL would report
			// Queued > 0 for a row the scheduler never pops (a silent no-op).
			q = `UPDATE urls SET next_check_at = ?
			     WHERE (url = ? OR site_id IN (SELECT id FROM sites WHERE base_url = ?))
			       AND site_id IN (SELECT id FROM sites WHERE enabled = 1)`
			args = []any{now, target, target}
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return int(affected), err
}

// DeleteSite removes a site by id. The schema declares ON DELETE CASCADE on every
// child table and the connection hook sets PRAGMA foreign_keys = ON, so the site's
// urls, snapshots, changes and file_snapshots are removed automatically.
func (db *DB) DeleteSite(ctx context.Context, id int64) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
		return err
	})
}
