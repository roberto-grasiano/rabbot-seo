package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// This file owns the proof-of-control read/write path. The proof record lives as
// columns ON the sites table (migration 0003): strictly 1:1 with a site, so a
// dedicated UPDATE/SELECT here avoids widening the fixed scanSite column list in
// repo_m1.go (which would break its scan arity in this phase). store -> verify is
// a clean one-way edge: store imports verify only for the ProofRecord type.

// SaveVerification writes a proof record onto the site row. It is a parameterized
// single-writer UPDATE (never string-formatted SQL); a RowsAffected==0 result
// means the site does not exist, surfaced as ErrNotFound so a write to a missing
// id is never a silent no-op. The nullable timestamps are stored as NULL when
// zero (verified_at is zero for an attested-only record).
func (db *DB) SaveVerification(ctx context.Context, siteID int64, rec verify.ProofRecord) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sites
			    SET verification_method = ?,
			        verification_token  = ?,
			        verification_state  = ?,
			        verified_at         = ?,
			        last_reverified_at  = ?,
			        updated_at          = ?
			  WHERE id = ?`,
			string(rec.Method),
			rec.Token,
			string(rec.State),
			nullTime(rec.VerifiedAt),
			nullTime(rec.LastReverifiedAt),
			time.Now().UTC(),
			siteID,
		)
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

// GetVerification reads the proof record for a site. A site that has never been
// verified reads back as StateThrottled (the migration's DEFAULT) with empty
// method/token and zero timestamps. A missing site returns ErrNotFound.
func (db *DB) GetVerification(ctx context.Context, siteID int64) (verify.ProofRecord, error) {
	var (
		method     string
		token      string
		state      string
		verifiedAt sql.NullTime
		reverified sql.NullTime
	)
	err := db.Read().QueryRowContext(ctx,
		`SELECT verification_method, verification_token, verification_state, verified_at, last_reverified_at
		   FROM sites WHERE id = ?`, siteID).
		Scan(&method, &token, &state, &verifiedAt, &reverified)
	if errors.Is(err, sql.ErrNoRows) {
		return verify.ProofRecord{}, ErrNotFound
	}
	if err != nil {
		return verify.ProofRecord{}, err
	}
	rec := verify.ProofRecord{
		SiteID: siteID,
		Method: verify.Method(method),
		Token:  token,
		State:  verify.State(state),
	}
	if verifiedAt.Valid {
		rec.VerifiedAt = verifiedAt.Time
	}
	if reverified.Valid {
		rec.LastReverifiedAt = reverified.Time
	}
	return rec, nil
}

// nullTime maps a zero time.Time to a SQL NULL and a set time to itself, so a
// not-yet-verified record stores NULL in verified_at/last_reverified_at.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}
