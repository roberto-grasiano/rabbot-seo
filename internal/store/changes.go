package store

import (
	"context"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// RecordChanges persists a batch of detected changes atomically. An empty/nil
// batch is a no-op. All rows commit together inside one BEGIN IMMEDIATE tx.
func (db *DB) RecordChanges(ctx context.Context, changes []model.Change) error {
	if len(changes) == 0 {
		return nil
	}
	return db.WriteTx(ctx, func(tx Tx) error {
		const q = `INSERT INTO changes (url_id, snapshot_id, field, old_value, new_value, change_class, detected_at)
		           VALUES (?, ?, ?, ?, ?, ?, ?)`
		for _, c := range changes {
			// detected_at is stored in UTC (.UTC() also strips the monotonic
			// reading) so the lexical TIMESTAMP comparison used by readers —
			// GetURLHistory and report.go ("detected_at >= ?" / MAX(detected_at)
			// against a UTC cutoff) — is instant-correct across zones. The
			// detector stamps it with local-zone time.Now().
			if _, err := tx.ExecContext(ctx, q,
				c.URLID, c.SnapshotID, c.Field, c.OldValue, c.NewValue,
				string(c.ChangeClass), c.DetectedAt.UTC()); err != nil {
				return err
			}
		}
		return nil
	})
}
