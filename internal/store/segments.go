package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// SegmentWithCount is a segment definition plus its live member count, for the
// read surfaces (rabbot segments, get_site, the site-detail control read).
type SegmentWithCount struct {
	model.Segment
	MemberCount int `db:"member_count"`
}

// segNameRow is a (id, name) pair from the segments table.
type segNameRow struct {
	id   int64
	name string
}

// scanSegmentNames returns every (id, name) for a site's segments, scanned
// inside the caller's write-tx. Extracted so the rows close is visible to
// sqlclosecheck (a deferred close inside an inline closure trips the linter).
func scanSegmentNames(ctx context.Context, tx Tx, siteID int64) ([]segNameRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM segments WHERE site_id = ?`, siteID)
	if err != nil {
		return nil, fmt.Errorf("list existing segments (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []segNameRow
	for rows.Next() {
		var r segNameRow
		if scanErr := rows.Scan(&r.id, &r.name); scanErr != nil {
			return nil, fmt.Errorf("scan existing segment: %w", scanErr)
		}
		out = append(out, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate existing segments: %w", rowsErr)
	}
	return out, nil
}

// segURLRow is a (id, url) pair from the urls table.
type segURLRow struct {
	id  int64
	url string
}

// scanSiteURLs returns every (id, url) for a site, scanned inside the caller's
// write-tx (no listing helper existed in store before A7).
func scanSiteURLs(ctx context.Context, tx Tx, siteID int64) ([]segURLRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, url FROM urls WHERE site_id = ?`, siteID)
	if err != nil {
		return nil, fmt.Errorf("list site urls (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []segURLRow
	for rows.Next() {
		var r segURLRow
		if scanErr := rows.Scan(&r.id, &r.url); scanErr != nil {
			return nil, fmt.Errorf("scan url row: %w", scanErr)
		}
		out = append(out, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate site urls: %w", rowsErr)
	}
	return out, nil
}

// SyncSiteSegments converges the segments table for siteID to exactly the given
// definitions: upsert by (site_id, name) updating match_rule, and delete any
// segment whose name is no longer present (the FK ON DELETE CASCADE clears that
// segment's url_segments rows). It returns the persisted name→id map so callers
// can build a registry without a follow-up read. The whole reconcile runs in one
// write-tx so a config reload never leaves the table half-converged.
//
// The upsert relies on the unique index idx_segments_site_name (migration 0007);
// without it ON CONFLICT(site_id, name) has no arbiter and the upsert would fail.
func (db *DB) SyncSiteSegments(ctx context.Context, siteID int64, defs []model.Segment) (map[string]int64, error) {
	ids := make(map[string]int64, len(defs))
	err := db.WriteTx(ctx, func(tx Tx) error {
		// Delete segments whose names are not in the incoming set. Build the
		// keep-set first; an empty config removes every segment for the site.
		keep := make(map[string]struct{}, len(defs))
		for _, d := range defs {
			keep[d.Name] = struct{}{}
		}
		existing, err := scanSegmentNames(ctx, tx, siteID)
		if err != nil {
			return err
		}
		var toDelete []int64
		for _, e := range existing {
			if _, ok := keep[e.name]; !ok {
				toDelete = append(toDelete, e.id)
			}
		}
		for _, id := range toDelete {
			// FK ON DELETE CASCADE clears this segment's url_segments rows.
			if _, derr := tx.ExecContext(ctx, `DELETE FROM segments WHERE id = ?`, id); derr != nil {
				return fmt.Errorf("delete removed segment (id=%d): %w", id, derr)
			}
		}

		// Upsert each definition; re-pattern updates match_rule and keeps the id.
		for _, d := range defs {
			if _, uerr := tx.ExecContext(ctx,
				`INSERT INTO segments (site_id, name, match_rule) VALUES (?, ?, ?)
				 ON CONFLICT(site_id, name) DO UPDATE SET match_rule = excluded.match_rule`,
				siteID, d.Name, d.MatchRule); uerr != nil {
				return fmt.Errorf("upsert segment (site=%d name=%q): %w", siteID, d.Name, uerr)
			}
			var id int64
			if serr := tx.QueryRowContext(ctx,
				`SELECT id FROM segments WHERE site_id = ? AND name = ?`,
				siteID, d.Name).Scan(&id); serr != nil {
				return fmt.Errorf("read segment id (site=%d name=%q): %w", siteID, d.Name, serr)
			}
			ids[d.Name] = id
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ReclassifySite rewrites every membership for siteID's URLs in one write-tx:
// it scans the site's urls rows, asks the injected match func which segment ids
// each URL belongs to, and replaces that URL's url_segments rows. The match func
// is injected so store stays config-free (the regexp matcher lives in
// internal/segments). The full clear-then-rewrite happens inside the transaction
// so a reader never observes a site with partial memberships.
//
// match returns the segment ids for a URL; an empty/nil slice means the URL is
// in no segment. Ids that don't belong to siteID would violate the conceptual
// model, but the caller (registry-backed) only ever returns this site's ids.
func (db *DB) ReclassifySite(ctx context.Context, siteID int64, match func(url string) []int64) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		// Snapshot (id, url) for the site up front so the scan does not race the
		// rewrites that follow on the same single-writer connection.
		urlRows, err := scanSiteURLs(ctx, tx, siteID)
		if err != nil {
			return err
		}
		for _, r := range urlRows {
			if err := setURLSegmentsTx(ctx, tx, r.id, match(r.url)); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetURLSegments idempotently sets urlID's segment memberships to exactly
// segmentIDs: it deletes the existing rows and inserts the new set in one
// write-tx, so re-applying the same set is a no-op and shrinking the set drops
// the removed memberships. An empty/nil slice clears all memberships.
func (db *DB) SetURLSegments(ctx context.Context, urlID int64, segmentIDs []int64) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		return setURLSegmentsTx(ctx, tx, urlID, segmentIDs)
	})
}

// setURLSegmentsTx is the in-transaction delete+insert for one URL's memberships,
// shared by SetURLSegments and ReclassifySite so both get identical semantics.
func setURLSegmentsTx(ctx context.Context, tx Tx, urlID int64, segmentIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM url_segments WHERE url_id = ?`, urlID); err != nil {
		return fmt.Errorf("clear url_segments (url=%d): %w", urlID, err)
	}
	seen := make(map[int64]struct{}, len(segmentIDs))
	for _, sid := range segmentIDs {
		if _, dup := seen[sid]; dup {
			continue // a URL can't be in the same segment twice; tolerate dup input
		}
		seen[sid] = struct{}{}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO url_segments (url_id, segment_id) VALUES (?, ?)`,
			urlID, sid); err != nil {
			return fmt.Errorf("insert url_segment (url=%d segment=%d): %w", urlID, sid, err)
		}
	}
	return nil
}

// ListSegments returns segment definitions plus live member counts. When siteID
// is non-nil, only that site's segments; when nil, every site's. A segment with
// zero members reports MemberCount 0 (LEFT JOIN, not INNER), so a freshly synced
// site still lists its empty segments. Ordered by (site_id, name) for stable
// table/JSON output.
func (db *DB) ListSegments(ctx context.Context, siteID *int64) ([]SegmentWithCount, error) {
	const base = `
		SELECT s.id, s.site_id, s.name, s.match_rule, COUNT(us.url_id) AS member_count
		FROM segments s
		LEFT JOIN url_segments us ON us.segment_id = s.id`
	const tail = `
		GROUP BY s.id, s.site_id, s.name, s.match_rule
		ORDER BY s.site_id, s.name`

	var (
		rows *sql.Rows
		err  error
	)
	if siteID != nil {
		rows, err = db.Read().QueryContext(ctx, base+`
		WHERE s.site_id = ?`+tail, *siteID)
	} else {
		rows, err = db.Read().QueryContext(ctx, base+tail)
	}
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SegmentWithCount
	for rows.Next() {
		var s SegmentWithCount
		if scanErr := rows.Scan(&s.ID, &s.SiteID, &s.Name, &s.MatchRule, &s.MemberCount); scanErr != nil {
			return nil, fmt.Errorf("scan segment: %w", scanErr)
		}
		out = append(out, s)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate segments: %w", rowsErr)
	}
	return out, nil
}

// SegmentIDByName resolves a segment name to its id within a site via the
// idx_segments_site_name unique index — a point lookup, no list scan. ok is
// false when the site has no segment with that name (errors-as-data).
func (db *DB) SegmentIDByName(ctx context.Context, siteID int64, name string) (int64, bool, error) {
	var id int64
	err := db.Read().QueryRowContext(ctx,
		`SELECT id FROM segments WHERE site_id = ? AND name = ?`, siteID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
