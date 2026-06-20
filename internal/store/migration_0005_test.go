package store

import (
	"context"
	"testing"
)

// TestMigration0005CreatesAlertMembers asserts the alert_members table exists
// after a fresh open (migration 0005 applied cleanly).
func TestMigration0005CreatesAlertMembers(t *testing.T) {
	db := openTestDB(t)
	var name string
	err := db.Read().QueryRowContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='table' AND name='alert_members'").Scan(&name)
	if err != nil {
		t.Fatalf("alert_members table missing after migrations: %v", err)
	}
	if name != "alert_members" {
		t.Errorf("table name = %q, want alert_members", name)
	}
}
