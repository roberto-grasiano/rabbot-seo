package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// ─── Site management ──────────────────────────────────────────────────────

func (db *DB) AddSite(ctx context.Context, s model.Site) (int64, error) {
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sites (base_url, name, enabled, min_interval, max_interval, max_concurrency, speed_scale, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.BaseURL, s.Name, s.Enabled, s.MinInterval, s.MaxInterval, s.MaxConcurrency, s.SpeedScale, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("add site %q: %w", s.BaseURL, ErrSiteExists)
			}
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (db *DB) ListSites(ctx context.Context) ([]model.Site, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, base_url, name, enabled, min_interval, max_interval, max_concurrency, speed_scale, created_at, updated_at
		 FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Site
	for rows.Next() {
		var s model.Site
		if err := rows.Scan(&s.ID, &s.BaseURL, &s.Name, &s.Enabled, &s.MinInterval, &s.MaxInterval,
			&s.MaxConcurrency, &s.SpeedScale, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) GetSite(ctx context.Context, id int64) (model.Site, error) {
	return scanSite(db.Read().QueryRowContext(ctx,
		`SELECT id, base_url, name, enabled, min_interval, max_interval, max_concurrency, speed_scale, created_at, updated_at
		 FROM sites WHERE id = ?`, id))
}

func (db *DB) GetSiteByBaseURL(ctx context.Context, baseURL string) (model.Site, error) {
	return scanSite(db.Read().QueryRowContext(ctx,
		`SELECT id, base_url, name, enabled, min_interval, max_interval, max_concurrency, speed_scale, created_at, updated_at
		 FROM sites WHERE base_url = ?`, baseURL))
}

func scanSite(row *sql.Row) (model.Site, error) {
	var s model.Site
	err := row.Scan(&s.ID, &s.BaseURL, &s.Name, &s.Enabled, &s.MinInterval, &s.MaxInterval,
		&s.MaxConcurrency, &s.SpeedScale, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Site{}, ErrNotFound
	}
	return s, err
}

func (db *DB) SetSiteEnabled(ctx context.Context, id int64, enabled bool) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sites SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, time.Now().UTC(), id)
		return err
	})
}

// SetSiteThrottle updates a site's per-host concurrency and min recheck interval.
// reconcile uses it to apply (or lift) the verification-aware throttle on an
// EXISTING site row — AddSite seeds these on insert, but a re-reconcile of an
// already-present site (e.g. after a successful `rabbot verify` flips its proof
// to StateVerified) must be able to widen or restore the live values. The min
// interval is in seconds (matching model.Site.MinInterval).
func (db *DB) SetSiteThrottle(ctx context.Context, id int64, minIntervalSeconds int64, maxConcurrency int) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sites SET min_interval = ?, max_concurrency = ?, updated_at = ? WHERE id = ?`,
			minIntervalSeconds, maxConcurrency, time.Now().UTC(), id)
		return err
	})
}

// ─── URL inventory ────────────────────────────────────────────────────────

func (db *DB) UpsertURL(ctx context.Context, u model.URL) (int64, error) {
	// #5 shared canonicalizer: store url in the same canonical keyspace link_edges.to_url
	// is written in, so the link-graph JOINs (OrphanPages, bfsDepths, the export reads)
	// match and the discovery dedup via GetURL keys on the stored form. canonicalURL is
	// a no-op for an already-canonical or unparseable value, so this only ever folds a
	// divergent-but-equivalent URL onto its canonical row (the ON CONFLICT key).
	canonURL := canonicalURL(u.URL)
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO urls (site_id, url, first_seen, last_checked, next_check_at, interval, importance, depth, in_sitemap, status_type, etag, last_modified, last_fetch_class)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(site_id, url) DO UPDATE SET
			   next_check_at = excluded.next_check_at,
			   interval = excluded.interval,
			   importance = excluded.importance,
			   depth = excluded.depth,
			   in_sitemap = excluded.in_sitemap,
			   status_type = excluded.status_type,
			   etag = excluded.etag,
			   last_modified = excluded.last_modified,
			   last_fetch_class = excluded.last_fetch_class`,
			u.SiteID, canonURL, u.FirstSeen, u.LastChecked, u.NextCheckAt, u.Interval, u.Importance,
			u.Depth, u.InSitemap, string(u.StatusType), u.ETag, u.LastModified, string(u.LastFetchClass))
		if err != nil {
			return err
		}
		// LastInsertId is meaningful only on insert; fetch the row id either way.
		return tx.QueryRowContext(ctx, `SELECT id FROM urls WHERE site_id = ? AND url = ?`, u.SiteID, canonURL).Scan(&id)
	})
	return id, err
}

func (db *DB) GetURL(ctx context.Context, siteID int64, normalizedURL string) (model.URL, error) {
	// #5: key on the canonical form so a lookup with a divergent-but-equivalent URL
	// (or a caller that passes a raw, not-yet-normalized value, e.g. discovery's
	// dedup probe) resolves the canonically-stored row. No-op when already canonical.
	var (
		q    = `SELECT id, site_id, url, first_seen, last_checked, next_check_at, interval, importance, depth, in_sitemap, status_type, etag, last_modified, last_fetch_class FROM urls WHERE url = ?`
		args = []any{canonicalURL(normalizedURL)}
	)
	if siteID != 0 {
		q += ` AND site_id = ?`
		args = append(args, siteID)
	}
	row := db.Read().QueryRowContext(ctx, q, args...)
	var (
		u  model.URL
		st string
		fc string
	)
	err := row.Scan(&u.ID, &u.SiteID, &u.URL, &u.FirstSeen, &u.LastChecked, &u.NextCheckAt, &u.Interval,
		&u.Importance, &u.Depth, &u.InSitemap, &st, &u.ETag, &u.LastModified, &fc)
	if errors.Is(err, sql.ErrNoRows) {
		return model.URL{}, ErrNotFound
	}
	if err != nil {
		return model.URL{}, err
	}
	u.StatusType = model.StatusType(st)
	u.LastFetchClass = model.FetchClass(fc)
	return u, nil
}

func (db *DB) UpdateURLSchedule(ctx context.Context, id int64, nextCheckAt time.Time, interval int64, lastFetch model.FetchClass, etag, lastModified string) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE urls SET next_check_at = ?, interval = ?, last_fetch_class = ?, etag = ?, last_modified = ?, last_checked = ? WHERE id = ?`,
			nextCheckAt, interval, string(lastFetch), etag, lastModified, time.Now().UTC(), id)
		return err
	})
}

// ─── Scheduler ────────────────────────────────────────────────────────────

func (db *DB) PopDueURLs(ctx context.Context, now time.Time, batch int) ([]model.URL, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT u.id, u.site_id, u.url, u.first_seen, u.last_checked, u.next_check_at, u.interval, u.importance, u.depth, u.in_sitemap, u.status_type, u.etag, u.last_modified, u.last_fetch_class
		 FROM urls u JOIN sites s ON s.id = u.site_id
		 WHERE u.next_check_at <= ? AND s.enabled = 1
		 ORDER BY u.importance DESC, u.next_check_at ASC
		 LIMIT ?`, now, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.URL
	for rows.Next() {
		var (
			u  model.URL
			st string
			fc string
		)
		if err := rows.Scan(&u.ID, &u.SiteID, &u.URL, &u.FirstSeen, &u.LastChecked, &u.NextCheckAt, &u.Interval,
			&u.Importance, &u.Depth, &u.InSitemap, &st, &u.ETag, &u.LastModified, &fc); err != nil {
			return nil, err
		}
		u.StatusType = model.StatusType(st)
		u.LastFetchClass = model.FetchClass(fc)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ─── Snapshots / change log (M1 owns SaveSnapshot/LatestSnapshot/GetURLHistory) ──

func (db *DB) SaveSnapshot(ctx context.Context, snap model.Snapshot) (int64, error) {
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO snapshots (url_id, fetched_at, http_status, redirect_chain, response_time_ms, title, meta_description, meta_robots, x_robots_tag, canonical, canonical_type, hreflang, headings, word_count, content_sha256, content_simhash, jsonld, jsonld_invalid_count, schema_types, internal_link_count, external_link_count, incoming_canonical_count, incoming_redirect_count, image_count, missing_alt_count, og, twitter, indexable, indexability_reason, render_mode, extraction_source, raw_html)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			// fetched_at is stored in UTC (.UTC() also strips the monotonic reading) so
			// the lexical TIMESTAMP comparison used by retention (DeleteStaleSnapshots'
			// "fetched_at < cutoff") is instant-correct across zones — the fetcher stamps
			// it with local-zone time.Now().
			snap.URLID, snap.FetchedAt.UTC(), snap.HTTPStatus, snap.RedirectChain, snap.ResponseTimeMS, snap.Title,
			snap.MetaDescription, snap.MetaRobots, snap.XRobotsTag, snap.Canonical, snap.CanonicalType,
			snap.Hreflang, snap.Headings, snap.WordCount, snap.ContentSHA256, int64(snap.ContentSimhash),
			snap.JSONLD, snap.JSONLDInvalidCount, snap.SchemaTypes, snap.InternalLinkCount, snap.ExternalLinkCount,
			snap.IncomingCanonicalCount, snap.IncomingRedirectCount, snap.ImageCount, snap.MissingAltCount,
			snap.OG, snap.Twitter, snap.Indexable, snap.IndexabilityReason,
			string(snap.RenderMode), snap.ExtractionSource, snap.RawHTML)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (db *DB) LatestSnapshot(ctx context.Context, urlID int64) (model.Snapshot, error) {
	row := db.Read().QueryRowContext(ctx,
		`SELECT id, url_id, fetched_at, http_status, redirect_chain, response_time_ms, title, meta_description, meta_robots, x_robots_tag, canonical, canonical_type, hreflang, headings, word_count, content_sha256, content_simhash, jsonld, jsonld_invalid_count, schema_types, internal_link_count, external_link_count, incoming_canonical_count, incoming_redirect_count, image_count, missing_alt_count, og, twitter, indexable, indexability_reason, render_mode, extraction_source, raw_html
		 FROM snapshots WHERE url_id = ? ORDER BY fetched_at DESC, id DESC LIMIT 1`, urlID)
	var (
		s          model.Snapshot
		simhash    int64
		renderMode string
	)
	err := row.Scan(&s.ID, &s.URLID, &s.FetchedAt, &s.HTTPStatus, &s.RedirectChain, &s.ResponseTimeMS, &s.Title,
		&s.MetaDescription, &s.MetaRobots, &s.XRobotsTag, &s.Canonical, &s.CanonicalType, &s.Hreflang, &s.Headings,
		&s.WordCount, &s.ContentSHA256, &simhash, &s.JSONLD, &s.JSONLDInvalidCount, &s.SchemaTypes, &s.InternalLinkCount, &s.ExternalLinkCount,
		&s.IncomingCanonicalCount, &s.IncomingRedirectCount, &s.ImageCount, &s.MissingAltCount, &s.OG, &s.Twitter,
		&s.Indexable, &s.IndexabilityReason, &renderMode, &s.ExtractionSource, &s.RawHTML)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Snapshot{}, ErrNotFound
	}
	if err != nil {
		return model.Snapshot{}, err
	}
	s.ContentSimhash = uint64(simhash)
	s.RenderMode = model.RenderMode(renderMode)
	return s, nil
}

func (db *DB) GetURLHistory(ctx context.Context, urlID int64, since time.Time) ([]model.Change, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, url_id, snapshot_id, field, old_value, new_value, change_class, detected_at
		 FROM changes WHERE url_id = ? AND detected_at >= ? ORDER BY detected_at DESC, id DESC`, urlID, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Change
	for rows.Next() {
		var (
			c  model.Change
			cc string
		)
		if err := rows.Scan(&c.ID, &c.URLID, &c.SnapshotID, &c.Field, &c.OldValue, &c.NewValue, &cc, &c.DetectedAt); err != nil {
			return nil, err
		}
		c.ChangeClass = model.ChangeClass(cc)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ─── File-level entities ──────────────────────────────────────────────────

func (db *DB) SaveFileSnapshot(ctx context.Context, fs model.FileSnapshot) (int64, error) {
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO file_snapshots (site_id, kind, fetched_at, content_sha256, parsed_entries, http_status)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			// fetched_at is stored in UTC (.UTC() also strips the monotonic
			// reading) so LatestFileSnapshot ("ORDER BY fetched_at DESC") and
			// TrimFileSnapshots order instant-correctly across zones — the
			// fetcher stamps it with local-zone time.Now().
			fs.SiteID, string(fs.Kind), fs.FetchedAt.UTC(), fs.ContentSHA256, fs.ParsedEntries, fs.HTTPStatus)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (db *DB) LatestFileSnapshot(ctx context.Context, siteID int64, kind model.FileSnapshotKind) (model.FileSnapshot, bool, error) {
	row := db.Read().QueryRowContext(ctx,
		`SELECT id, site_id, kind, fetched_at, content_sha256, parsed_entries, http_status
		 FROM file_snapshots WHERE site_id = ? AND kind = ? ORDER BY fetched_at DESC, id DESC LIMIT 1`,
		siteID, string(kind))
	var (
		fs    model.FileSnapshot
		kind2 string
	)
	err := row.Scan(&fs.ID, &fs.SiteID, &kind2, &fs.FetchedAt, &fs.ContentSHA256, &fs.ParsedEntries, &fs.HTTPStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FileSnapshot{}, false, nil
	}
	if err != nil {
		return model.FileSnapshot{}, false, err
	}
	fs.Kind = model.FileSnapshotKind(kind2)
	return fs, true, nil
}
