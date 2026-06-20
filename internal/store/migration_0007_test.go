package store

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestMigration0007CreatesSegmentIndexes asserts a fresh DB carries the two new
// indexes after migration 0007 applies cleanly (the upgrade arm — openTestDB
// applies every embedded migration in order, so reaching this state proves 0007
// applies on top of an already-built 0001-0006 schema).
func TestMigration0007CreatesSegmentIndexes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, idx := range []string{"idx_segments_site_name", "idx_url_segments_segment_id"} {
		var name string
		err := db.Read().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name)
		if err != nil {
			t.Fatalf("index %q missing after migrations: %v", idx, err)
		}
		if name != idx {
			t.Errorf("index name = %q, want %q", name, idx)
		}
	}
}

// TestMigration0007SegmentNameUniquePerSite asserts the new UNIQUE index rejects
// a duplicate (site_id, name) insert while allowing the same name in a different
// site (the index is composite on site_id, not name alone).
func TestMigration0007SegmentNameUniquePerSite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	siteA, err := db.AddSite(ctx, model.Site{BaseURL: "https://a.example.com", Name: "a", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite a: %v", err)
	}
	siteB, err := db.AddSite(ctx, model.Site{BaseURL: "https://b.example.com", Name: "b", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite b: %v", err)
	}

	insert := func(siteID int64, name string) error {
		return db.WriteTx(ctx, func(tx Tx) error {
			_, e := tx.ExecContext(ctx,
				`INSERT INTO segments (site_id, name, match_rule) VALUES (?, ?, ?)`,
				siteID, name, "^/blog/")
			return e
		})
	}

	if err := insert(siteA, "content"); err != nil {
		t.Fatalf("first insert (siteA, content): %v", err)
	}

	// Duplicate (site_id, name) must fail under the new unique index.
	if err := insert(siteA, "content"); err == nil {
		t.Fatalf("duplicate (siteA, content) insert succeeded; want UNIQUE violation")
	} else if !isUniqueViolation(err) {
		t.Fatalf("duplicate insert error = %v, want UNIQUE violation", err)
	}

	// Same name in a DIFFERENT site is allowed (uniqueness is per-site).
	if err := insert(siteB, "content"); err != nil {
		t.Fatalf("same name in siteB should be allowed: %v", err)
	}
}
