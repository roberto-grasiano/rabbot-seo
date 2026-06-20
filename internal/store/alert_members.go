package store

import (
	"context"
	"fmt"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// AddAlertMember records url as a live member of the open incident alertID. It
// is idempotent: re-adding an existing (alert_id, url) pair is a no-op (INSERT
// OR IGNORE), so a member URL that keeps failing across recheck cycles never
// inflates the membership set or errors. The (alert_id, url) PRIMARY KEY makes
// the conflict deterministic.
func (db *DB) AddAlertMember(ctx context.Context, alertID int64, url string) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO alert_members (alert_id, url) VALUES (?, ?)`,
			alertID, url); err != nil {
			return fmt.Errorf("add alert member (alert=%d url=%q): %w", alertID, url, err)
		}
		return nil
	})
}

// RemoveAlertMember deletes url from the open incident alertID's member set and
// returns how many members remain for that incident. The delete and the COUNT
// run in one WriteTx so the returned count cannot race a concurrent add/remove
// of the same incident. remaining==0 means the recovered URL was the last live
// member — the caller (alerts pipeline Resolve) should close the incident only
// then; remaining>0 means siblings are still broken and the incident stays open.
// Removing a URL that was never a member is not an error: the count simply
// reflects the unchanged set.
func (db *DB) RemoveAlertMember(ctx context.Context, alertID int64, url string) (remaining int, err error) {
	err = db.WriteTx(ctx, func(tx Tx) error {
		if _, derr := tx.ExecContext(ctx,
			`DELETE FROM alert_members WHERE alert_id = ? AND url = ?`,
			alertID, url); derr != nil {
			return fmt.Errorf("remove alert member (alert=%d url=%q): %w", alertID, url, derr)
		}
		if cerr := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM alert_members WHERE alert_id = ?`,
			alertID).Scan(&remaining); cerr != nil {
			return fmt.Errorf("count alert members (alert=%d): %w", alertID, cerr)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return remaining, nil
}

// HasOpenIncidentMember reports whether an OPEN incident exists for fingerprint and
// url is already a tracked member of it. It is the idempotency probe for signal
// evaluators that run on a fixed cadence (the daily GSC pull): a still-firing per-URL
// signal whose URL is already a member of an open incident must NOT re-Ingest (which
// would re-notify every tick), so the evaluator calls this first and skips the Ingest
// when it returns true — firing only on a genuine state change (a new URL, or a
// recurrence after the incident closed). A URL that is NOT yet a member (or has no open
// incident) returns false, so a newly-affected URL still Ingests (registering its
// membership and notifying). The join is keyed on the incident fingerprint so it shares
// the alerts pipeline's incident identity exactly.
func (db *DB) HasOpenIncidentMember(ctx context.Context, fingerprint, url string) (bool, error) {
	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM alert_members m
		   JOIN alerts a ON a.id = m.alert_id
		  WHERE a.fingerprint = ? AND a.status = ? AND m.url = ?`,
		fingerprint, string(model.AlertOpen), url).Scan(&n); err != nil {
		return false, fmt.Errorf("has open incident member (fp=%q url=%q): %w", fingerprint, url, err)
	}
	return n > 0, nil
}

// CountAlertMembers returns the number of live member URLs for incident alertID.
func (db *DB) CountAlertMembers(ctx context.Context, alertID int64) (int, error) {
	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alert_members WHERE alert_id = ?`, alertID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count alert members (alert=%d): %w", alertID, err)
	}
	return n, nil
}
