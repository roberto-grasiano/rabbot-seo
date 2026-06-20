package store

import "context"

// CountSiteURLs returns the number of URLs in the inventory for a site. Used by
// discovery to enforce the per-site page cap before enqueuing more.
func (db *DB) CountSiteURLs(ctx context.Context, siteID int64) (int, error) {
	var n int
	err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM urls WHERE site_id = ?`, siteID).Scan(&n)
	return n, err
}
