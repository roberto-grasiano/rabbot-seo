package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// IssueFilter scopes ListIssues (contract §3.2). OpenOnly restricts to
// status='open'; Status (when non-nil) pins an exact lifecycle state; SiteID
// (when non-nil) joins urls→sites to scope by site; URLID (when non-nil) scopes
// to a single URL via the UNIQUE(url_id, rule_id) index (the rules engine's
// per-fetch reconcile path); Segment (when non-nil) joins url_segments→segments
// to scope by segment name (names are per-site, so an all-sites query filtered
// by name matches that name in any site).
type IssueFilter struct {
	OpenOnly bool
	Status   *model.IssueStatus
	SiteID   *int64
	URLID    *int64
	Severity *model.Severity
	Segment  *string
}

// UpsertIssue inserts or updates an issue, deduped on (url_id, rule_id). On
// conflict it refreshes status/severity/impact/last_seen/detail and returns the
// existing row id. opened_at is preserved across an open->open refresh, but a
// closed->open reopen takes the fresh opened_at (and clears closed_at) so the
// row does not look like it was never closed.
func (db *DB) UpsertIssue(ctx context.Context, iss model.Issue) (int64, error) {
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		detail := iss.Detail
		if detail == "" {
			detail = "{}"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issues (url_id, rule_id, status, severity, impact_points, opened_at, closed_at, last_seen_at, detail)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(url_id, rule_id) DO UPDATE SET
			   status        = excluded.status,
			   severity      = excluded.severity,
			   impact_points = excluded.impact_points,
			   opened_at     = CASE WHEN issues.status = 'closed' THEN excluded.opened_at ELSE issues.opened_at END,
			   closed_at     = excluded.closed_at,
			   last_seen_at  = excluded.last_seen_at,
			   detail        = excluded.detail`,
			iss.URLID, iss.RuleID, string(iss.Status), string(iss.Severity), iss.ImpactPoints,
			iss.OpenedAt, iss.ClosedAt, iss.LastSeenAt, detail); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT id FROM issues WHERE url_id = ? AND rule_id = ?`, iss.URLID, iss.RuleID).Scan(&id)
	})
	return id, err
}

// CloseIssue marks the open issue for (url_id, rule_id) as closed at `at`.
//
// Deliberately idempotent: a 0-row update (no matching open issue) is a no-op
// success, not an error — this is called from the rules engine's per-fetch
// reconcile where concurrent closes must be safe.
func (db *DB) CloseIssue(ctx context.Context, urlID int64, ruleID string, at time.Time) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE issues SET status = ?, closed_at = ?, last_seen_at = ?
			 WHERE url_id = ? AND rule_id = ? AND status = ?`,
			string(model.IssueClosed), at, at, urlID, ruleID, string(model.IssueOpen))
		return err
	})
}

// IgnoreIssue marks an issue ignored by primary key (CLI `issue ignore`).
func (db *DB) IgnoreIssue(ctx context.Context, id int64) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE issues SET status = ? WHERE id = ?`, string(model.IssueIgnored), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListIssues returns issues filtered by the IssueFilter (contract §3.2).
// OpenOnly restricts to status='open'; Status (when non-nil) pins an exact
// lifecycle state; SiteID (when non-nil) joins urls→sites to scope by site;
// Segment (when non-nil) joins url_segments→segments to scope by segment name.
func (db *DB) ListIssues(ctx context.Context, f IssueFilter) ([]model.Issue, error) {
	q := `SELECT i.id, i.url_id, i.rule_id, i.status, i.severity, i.impact_points,
	             i.opened_at, i.closed_at, i.last_seen_at, i.detail
	      FROM issues i`
	var (
		where    []string
		joinArgs []any // bound by ?-placeholders that sit in the JOIN clauses
		args     []any // bound by ?-placeholders in the WHERE clause
	)
	if f.SiteID != nil {
		q += ` JOIN urls u ON u.id = i.url_id`
		where = append(where, `u.site_id = ?`)
		args = append(args, *f.SiteID)
	}
	if f.Segment != nil {
		// Scope to issues whose URL is a member of a segment named *f.Segment.
		// Segment names are unique per site (idx_segments_site_name), so this
		// join adds at most one row per (url, name) — no count inflation. The
		// seg.name ? sits in the JOIN clause (textually before WHERE), so its
		// arg must precede the WHERE args: it is collected in joinArgs.
		q += ` JOIN url_segments us ON us.url_id = i.url_id` +
			` JOIN segments seg ON seg.id = us.segment_id AND seg.name = ?`
		joinArgs = append(joinArgs, *f.Segment)
	}
	if f.URLID != nil {
		where = append(where, `i.url_id = ?`)
		args = append(args, *f.URLID)
	}
	if f.Severity != nil {
		where = append(where, `i.severity = ?`)
		args = append(args, string(*f.Severity))
	}
	if f.OpenOnly {
		where = append(where, `i.status = ?`)
		args = append(args, string(model.IssueOpen))
	} else if f.Status != nil {
		where = append(where, `i.status = ?`)
		args = append(args, string(*f.Status))
	}
	for i, w := range where {
		if i == 0 {
			q += ` WHERE ` + w
		} else {
			q += ` AND ` + w
		}
	}
	q += ` ORDER BY i.impact_points DESC, i.opened_at DESC`
	// JOIN-clause placeholders bind before WHERE-clause placeholders.
	args = append(joinArgs, args...)

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.Issue
	for rows.Next() {
		var (
			iss      model.Issue
			closedAt sql.NullTime
			status   string
			severity string
		)
		if err := rows.Scan(&iss.ID, &iss.URLID, &iss.RuleID, &status, &severity, &iss.ImpactPoints,
			&iss.OpenedAt, &closedAt, &iss.LastSeenAt, &iss.Detail); err != nil {
			return nil, err
		}
		iss.Status = model.IssueStatus(status)
		iss.Severity = model.Severity(severity)
		if closedAt.Valid {
			t := closedAt.Time
			iss.ClosedAt = &t
		}
		out = append(out, iss)
	}
	return out, rows.Err()
}
