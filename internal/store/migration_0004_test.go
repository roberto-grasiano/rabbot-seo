package store

import (
	"context"
	"testing"
)

// TestMigration0004AddsChangesSnapshotIndex asserts the index exists after open.
func TestMigration0004AddsChangesSnapshotIndex(t *testing.T) {
	db := openTestDB(t)
	var name string
	err := db.Read().QueryRowContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_changes_snapshot_id'").Scan(&name)
	if err != nil {
		t.Fatalf("idx_changes_snapshot_id missing after migrations: %v", err)
	}
	if name != "idx_changes_snapshot_id" {
		t.Errorf("index name = %q, want idx_changes_snapshot_id", name)
	}
}
