package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

const alertColumns = `id, site_id, fingerprint, group_key, severity, status, affected_count,
	first_detected_at, last_updated_at, last_notified_at, auto_closed_at, payload_summary`

// scanAlert reads one alerts row (handles the two nullable timestamps).
func scanAlert(s interface {
	Scan(dest ...any) error
}) (model.Alert, error) {
	var (
		a                      model.Alert
		lastNotified, autoClsd sql.NullTime
		severity, status       string
	)
	if err := s.Scan(&a.ID, &a.SiteID, &a.Fingerprint, &a.GroupKey, &severity, &status,
		&a.AffectedCount, &a.FirstDetectedAt, &a.LastUpdatedAt,
		&lastNotified, &autoClsd, &a.PayloadSummary); err != nil {
		return model.Alert{}, err
	}
	a.Severity = model.Severity(severity)
	a.Status = model.AlertStatus(status)
	if lastNotified.Valid {
		t := lastNotified.Time
		a.LastNotifiedAt = &t
	}
	if autoClsd.Valid {
		t := autoClsd.Time
		a.AutoClosedAt = &t
	}
	return a, nil
}

// OpenIncident inserts a new incident row and returns its id.
func (db *DB) OpenIncident(ctx context.Context, a model.Alert) (int64, error) {
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		payload := a.PayloadSummary
		if payload == "" {
			payload = "{}"
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO alerts (site_id, fingerprint, group_key, severity, status, affected_count,
			   first_detected_at, last_updated_at, last_notified_at, auto_closed_at, payload_summary)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.SiteID, a.Fingerprint, a.GroupKey, string(a.Severity), string(a.Status), a.AffectedCount,
			a.FirstDetectedAt, a.LastUpdatedAt, a.LastNotifiedAt, a.AutoClosedAt, payload)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// UpdateIncident persists accrued state (affected_count, timestamps, payload)
// for an existing incident, keyed by id.
func (db *DB) UpdateIncident(ctx context.Context, a model.Alert) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		payload := a.PayloadSummary
		if payload == "" {
			payload = "{}"
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE alerts SET severity = ?, status = ?, affected_count = ?, last_updated_at = ?,
			   last_notified_at = ?, auto_closed_at = ?, payload_summary = ?
			 WHERE id = ?`,
			string(a.Severity), string(a.Status), a.AffectedCount, a.LastUpdatedAt,
			a.LastNotifiedAt, a.AutoClosedAt, payload, a.ID)
		return err
	})
}

// GetOpenIncident returns the open incident for a fingerprint, if any.
func (db *DB) GetOpenIncident(ctx context.Context, fingerprint string) (model.Alert, bool, error) {
	row := db.Read().QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alerts WHERE fingerprint = ? AND status = ?
		 ORDER BY last_updated_at DESC LIMIT 1`,
		fingerprint, string(model.AlertOpen))
	a, err := scanAlert(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Alert{}, false, nil
	}
	if err != nil {
		return model.Alert{}, false, err
	}
	return a, true, nil
}

// CloseIncident closes an incident by id, stamping closed time. When
// autoClosed is true the auto_closed_at column is set (24h sweep). A manual
// close (autoClosed=false) leaves auto_closed_at untouched so any prior
// timestamp is preserved rather than erased to NULL.
//
// Deliberately idempotent: a 0-row update (already closed / unknown id) is a
// no-op success, not an error — this is called from the rules engine and the
// auto-close sweep where concurrent closes must be safe.
func (db *DB) CloseIncident(ctx context.Context, id int64, at time.Time, autoClosed bool) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		if autoClosed {
			_, err := tx.ExecContext(ctx,
				`UPDATE alerts SET status = ?, last_updated_at = ?, auto_closed_at = ? WHERE id = ?`,
				string(model.AlertClosed), at, at, id)
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE alerts SET status = ?, last_updated_at = ? WHERE id = ?`,
			string(model.AlertClosed), at, id)
		return err
	})
}

// ListOpenIncidents returns all incidents with status = open, newest first
// (drives the auto-close sweep).
func (db *DB) ListOpenIncidents(ctx context.Context) ([]model.Alert, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT `+alertColumns+` FROM alerts WHERE status = ? ORDER BY last_updated_at DESC`,
		string(model.AlertOpen))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
