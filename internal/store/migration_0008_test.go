package store

import (
	"context"
	"testing"
)

// TestMigration0008CreatesHealthScores asserts a fresh DB carries the
// health_scores table and its scope-time index after migration 0008 applies
// cleanly on top of an already-built 0001-0007 schema (openTestDB applies every
// embedded migration in order).
func TestMigration0008CreatesHealthScores(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var tbl string
	if err := db.Read().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='health_scores'").Scan(&tbl); err != nil {
		t.Fatalf("health_scores table missing after migrations: %v", err)
	}

	var idx string
	if err := db.Read().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_health_scores_scope_time'").Scan(&idx); err != nil {
		t.Fatalf("idx_health_scores_scope_time missing after migrations: %v", err)
	}

	// segment_id must be NULLABLE (NULL = whole-site scope); every other column
	// in the spec's SQL is NOT NULL.
	wantNotNull := map[string]int{
		"id": 0, "site_id": 1, "segment_id": 0, "computed_at": 1, "score": 1,
		"impact_mass": 1, "max_mass": 1, "page_count": 1, "open_critical": 1,
		"open_warning": 1, "open_info": 1, "breakdown": 1,
	}
	rows, err := db.Read().QueryContext(ctx, "PRAGMA table_info(health_scores)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue *string
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		got[name] = notNull
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for name, want := range wantNotNull {
		g, ok := got[name]
		if !ok {
			t.Errorf("health_scores missing column %q", name)
			continue
		}
		if g != want {
			t.Errorf("column %q notNull = %d, want %d", name, g, want)
		}
	}
}
