package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestRecordChangesAndHistory(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

	// Seed site→url→snapshot so the changes FKs (url_id, snapshot_id) resolve
	// under the M0 connection hook's PRAGMA foreign_keys = ON.
	urlID := seedURL(t, st, "changes.com")
	snapID, err := st.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200})
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// RecordChanges persists a batch in one BEGIN IMMEDIATE transaction.
	if err := st.RecordChanges(ctx, []model.Change{
		{URLID: urlID, SnapshotID: snapID, Field: "title", OldValue: "Old", NewValue: "New",
			ChangeClass: model.ChangeSubstantive, DetectedAt: now},
		{URLID: urlID, SnapshotID: snapID, Field: "canonical", OldValue: "/a", NewValue: "/b",
			ChangeClass: model.ChangeSubstantive, DetectedAt: now},
	}); err != nil {
		t.Fatalf("RecordChanges: %v", err)
	}
	// Empty batch is a no-op (no error, no rows).
	if err := st.RecordChanges(ctx, nil); err != nil {
		t.Fatalf("RecordChanges(nil): %v", err)
	}
	hist, err := st.GetURLHistory(ctx, urlID, now.Add(-time.Hour)) // M1-owned reader
	if err != nil {
		t.Fatalf("GetURLHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("GetURLHistory = %d changes, want 2", len(hist))
	}
}

// TestRecordChangesStoresUTC guards the store-layer UTC invariant for the
// changes.detected_at column. modernc.org/sqlite serializes a time.Time as its
// wall-clock TEXT and compares it lexically, so readers like GetURLHistory and
// report.go ("detected_at >= ?" against a UTC cutoff) only get instant-correct
// results if RecordChanges normalizes to UTC at storage. A change stamped in a
// west-of-UTC zone has a wall-clock string that sorts BEFORE its true UTC
// instant; without .UTC() at the store boundary the lexical cutoff wrongly
// drops a change that is actually newer than the cutoff.
func TestRecordChangesStoresUTC(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Fixed -8h zone; no LoadLocation (tzdata may be absent on the host).
	west := time.FixedZone("X", -8*3600)
	urlID := seedURL(t, st, "changes-utc.test")

	// True instant 2026-04-01 05:00 UTC — 5h AFTER the cutoff, so it must be
	// returned. In the -8h zone its wall clock is "2026-03-31 21:00", whose
	// stored string sorts BEFORE the UTC cutoff string: under a verbatim
	// (non-UTC) store the lexical "detected_at >= cutoff" wrongly excludes it.
	detected := time.Date(2026, 4, 1, 5, 0, 0, 0, time.UTC).In(west)
	snapID, err := st.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: detected, HTTPStatus: 200})
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := st.RecordChanges(ctx, []model.Change{
		{URLID: urlID, SnapshotID: snapID, Field: "title", OldValue: "a", NewValue: "b",
			ChangeClass: model.ChangeSubstantive, DetectedAt: detected},
	}); err != nil {
		t.Fatalf("RecordChanges: %v", err)
	}

	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	hist, err := st.GetURLHistory(ctx, urlID, cutoff)
	if err != nil {
		t.Fatalf("GetURLHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("GetURLHistory after cutoff = %d changes, want 1 (detected_at not stored in UTC → lexical skew)", len(hist))
	}
	// The read-back instant must equal the original instant (not the wall clock).
	if !hist[0].DetectedAt.Equal(detected) {
		t.Errorf("DetectedAt round-trip = %v, want instant %v", hist[0].DetectedAt, detected)
	}
}
