package store

import (
	"context"
	"testing"
)

// gscColumn returns the PRAGMA table_info row (type, notnull, default) for the
// named column of `table`, or fails if the column is absent. It mirrors
// snapshotColumn (migration_0009_test.go) generalized to an arbitrary table.
func gscColumn(t *testing.T, db *DB, table, col string) (typ string, notNull int, dflt *string) {
	t.Helper()
	rows, err := db.Read().Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			nn        int
			dfltValue *string
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &nn, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == col {
			return colType, nn, dfltValue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s) rows: %v", table, err)
	}
	t.Fatalf("%s.%s column missing after migrations", table, col)
	return "", 0, nil
}

// TestMigration0011CreatesGSCTables asserts a FRESH DB (openTestDB applies every
// embedded migration on Open) carries the two GSC tables, their UNIQUE-key
// covering indexes, and the explicit range/lookup indexes.
func TestMigration0011CreatesGSCTables(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, tbl := range []string{"search_metrics", "url_index_status"} {
		var name string
		if err := db.Read().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name); err != nil {
			t.Fatalf("%s table missing after migrations: %v", tbl, err)
		}
	}

	for _, idx := range []string{"idx_search_metrics_site_date", "idx_url_index_status_site_url"} {
		var name string
		if err := db.Read().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name); err != nil {
			t.Fatalf("%s missing after migrations: %v", idx, err)
		}
	}
}

// TestMigration0011SearchMetricsColumns asserts search_metrics' column types and
// NOT-NULL flags. The grain key columns (site_id/url/query/date) are NOT NULL; the
// four metric columns are NOT NULL with a numeric default so a partial row is never
// NULL.
func TestMigration0011SearchMetricsColumns(t *testing.T) {
	db := openTestDB(t)

	wantType := map[string]string{
		"id": "INTEGER", "site_id": "INTEGER", "url": "TEXT", "query": "TEXT",
		"date": "TEXT", "clicks": "INTEGER", "impressions": "INTEGER",
		"ctr": "REAL", "position": "REAL",
	}
	wantNotNull := map[string]int{
		"site_id": 1, "url": 1, "query": 1, "date": 1,
		"clicks": 1, "impressions": 1, "ctr": 1, "position": 1,
	}
	for col, typ := range wantType {
		gotType, gotNN, _ := gscColumn(t, db, "search_metrics", col)
		if gotType != typ {
			t.Errorf("search_metrics.%s type = %q, want %q", col, gotType, typ)
		}
		if want, ok := wantNotNull[col]; ok && gotNN != want {
			t.Errorf("search_metrics.%s notnull = %d, want %d", col, gotNN, want)
		}
	}
}

// TestMigration0011URLIndexStatusColumns asserts url_index_status carries every
// inspection field. The string verdict fields default to ” (NOT NULL) so an
// upgraded/partial row reads back as an empty sentinel, not NULL; last_crawl_time
// is the only NULLABLE column (Google may report no last crawl).
func TestMigration0011URLIndexStatusColumns(t *testing.T) {
	db := openTestDB(t)

	wantType := map[string]string{
		"id": "INTEGER", "site_id": "INTEGER", "url": "TEXT",
		"inspected_at": "TIMESTAMP", "verdict": "TEXT", "coverage_state": "TEXT",
		"indexing_state": "TEXT", "robots_txt_state": "TEXT", "page_fetch_state": "TEXT",
		"google_canonical": "TEXT", "user_canonical": "TEXT", "crawled_as": "TEXT",
		"last_crawl_time": "TIMESTAMP",
	}
	for col, typ := range wantType {
		gotType, _, _ := gscColumn(t, db, "url_index_status", col)
		if gotType != typ {
			t.Errorf("url_index_status.%s type = %q, want %q", col, gotType, typ)
		}
	}

	// The string verdict fields are NOT NULL DEFAULT '' (empty sentinel, never NULL).
	for _, col := range []string{
		"verdict", "coverage_state", "indexing_state", "robots_txt_state",
		"page_fetch_state", "google_canonical", "user_canonical", "crawled_as",
	} {
		_, notNull, dflt := gscColumn(t, db, "url_index_status", col)
		if notNull != 1 {
			t.Errorf("url_index_status.%s notnull = %d, want 1", col, notNull)
		}
		if dflt == nil {
			t.Errorf("url_index_status.%s has NULL default, want '' ('')", col)
			continue
		}
		if *dflt != "''" {
			t.Errorf("url_index_status.%s default = %q, want \"''\"", col, *dflt)
		}
	}

	// inspected_at is NOT NULL (it is always stamped by the puller); last_crawl_time
	// is NULLABLE (Google may report none).
	if _, nn, _ := gscColumn(t, db, "url_index_status", "inspected_at"); nn != 1 {
		t.Errorf("url_index_status.inspected_at notnull = %d, want 1", nn)
	}
	if _, nn, _ := gscColumn(t, db, "url_index_status", "last_crawl_time"); nn != 0 {
		t.Errorf("url_index_status.last_crawl_time notnull = %d, want 0 (nullable)", nn)
	}
}
