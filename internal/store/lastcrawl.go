package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LastCrawlAt returns the most recent crawl time across enabled sites' URLs.
// ok is false (with a zero time, no error) when nothing has been crawled yet:
// either there are no enabled URLs, or every URL is seeded-but-never-crawled
// (zero last_checked). Used by the daemon status response so `status` reports a
// real last-crawl time instead of a blank.
func (db *DB) LastCrawlAt(ctx context.Context) (time.Time, bool, error) {
	var t *time.Time
	err := db.Read().QueryRowContext(ctx,
		`SELECT u.last_checked FROM urls u JOIN sites s ON s.id = u.site_id
		 WHERE s.enabled = 1 AND u.last_checked IS NOT NULL
		 ORDER BY u.last_checked DESC LIMIT 1`).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if t == nil || t.IsZero() {
		return time.Time{}, false, nil
	}
	return t.UTC(), true, nil
}
