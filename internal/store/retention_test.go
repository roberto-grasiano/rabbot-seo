package store

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// ─── seeding helpers (reused across retention tests) ───────────────────────

func seedSite(t *testing.T, db *DB, baseURL string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO sites (base_url, name, created_at, updated_at) VALUES (?,?,?,?)`,
			baseURL, "T", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		return e
	})
	if err != nil {
		t.Fatalf("seedSite: %v", err)
	}
	return id
}

func seedURL(t *testing.T, db *DB, siteID int64, u string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO urls (site_id, url, first_seen, next_check_at) VALUES (?,?,?,?)`,
			siteID, u, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		return e
	})
	if err != nil {
		t.Fatalf("seedURL: %v", err)
	}
	return id
}

// seedSnapshot inserts a snapshot at fetchedAt with the given raw_html (nil = none)
// using the production SaveSnapshot path, and returns its id.
func seedSnapshot(t *testing.T, db *DB, urlID int64, fetchedAt time.Time, raw []byte) int64 {
	t.Helper()
	id, err := db.SaveSnapshot(context.Background(), model.Snapshot{
		URLID:     urlID,
		FetchedAt: fetchedAt,
		RawHTML:   raw,
	})
	if err != nil {
		t.Fatalf("seedSnapshot: %v", err)
	}
	return id
}

func seedChange(t *testing.T, db *DB, urlID, snapshotID int64) {
	t.Helper()
	ctx := context.Background()
	err := db.WriteTx(ctx, func(tx Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO changes (url_id, snapshot_id, field, detected_at) VALUES (?,?,?,?)`,
			urlID, snapshotID, "title", "2026-01-01T00:00:00Z")
		return e
	})
	if err != nil {
		t.Fatalf("seedChange: %v", err)
	}
}

func seedFileSnapshot(t *testing.T, db *DB, siteID int64, kind model.FileSnapshotKind, fetchedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO file_snapshots (site_id, kind, fetched_at) VALUES (?,?,?)`,
			siteID, string(kind), fetchedAt)
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		return e
	})
	if err != nil {
		t.Fatalf("seedFileSnapshot: %v", err)
	}
	return id
}

func rawHTMLIsNull(t *testing.T, db *DB, snapID int64) bool {
	t.Helper()
	var isNull int
	if err := db.Read().QueryRow(
		"SELECT raw_html IS NULL FROM snapshots WHERE id=?", snapID).Scan(&isNull); err != nil {
		t.Fatalf("rawHTMLIsNull: %v", err)
	}
	return isNull == 1
}

func snapshotExists(t *testing.T, db *DB, snapID int64) bool {
	t.Helper()
	var n int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM snapshots WHERE id=?", snapID).Scan(&n); err != nil {
		t.Fatalf("snapshotExists: %v", err)
	}
	return n == 1
}

// ─── Layer 1 ───────────────────────────────────────────────────────────────

func TestNullStaleRawHTML(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	s1 := seedSnapshot(t, db, url, base.Add(1*time.Hour), []byte("<html>1</html>"))
	s2 := seedSnapshot(t, db, url, base.Add(2*time.Hour), []byte("<html>2</html>"))
	s3 := seedSnapshot(t, db, url, base.Add(3*time.Hour), []byte("<html>3</html>"))

	n, err := db.NullStaleRawHTML(ctx, 1)
	if err != nil {
		t.Fatalf("NullStaleRawHTML: %v", err)
	}
	if n != 2 {
		t.Errorf("nulled = %d, want 2", n)
	}
	if rawHTMLIsNull(t, db, s3) {
		t.Errorf("newest snapshot s3 raw_html was nulled, want retained")
	}
	if !rawHTMLIsNull(t, db, s1) || !rawHTMLIsNull(t, db, s2) {
		t.Errorf("older snapshots not nulled (s1null=%v s2null=%v)", rawHTMLIsNull(t, db, s1), rawHTMLIsNull(t, db, s2))
	}

	// Idempotent: a second run nulls nothing.
	n2, err := db.NullStaleRawHTML(ctx, 1)
	if err != nil {
		t.Fatalf("NullStaleRawHTML (2nd): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run nulled = %d, want 0", n2)
	}
}

func TestTrimFileSnapshots(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	site := seedSite(t, db, "https://a.com")
	for i := 0; i < 5; i++ {
		seedFileSnapshot(t, db, site, model.FileKindRobots, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 3; i++ {
		seedFileSnapshot(t, db, site, model.FileKindSitemap, base.Add(time.Duration(i)*time.Hour))
	}

	n, err := db.TrimFileSnapshots(ctx, 2)
	if err != nil {
		t.Fatalf("TrimFileSnapshots: %v", err)
	}
	if n != 4 { // robots: 5-2=3 deleted, sitemap: 3-2=1 deleted
		t.Errorf("trimmed = %d, want 4", n)
	}

	count := func(kind model.FileSnapshotKind) int {
		var c int
		if err := db.Read().QueryRow(
			"SELECT COUNT(*) FROM file_snapshots WHERE site_id=? AND kind=?", site, string(kind)).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", kind, err)
		}
		return c
	}
	if got := count(model.FileKindRobots); got != 2 {
		t.Errorf("robots remaining = %d, want 2", got)
	}
	if got := count(model.FileKindSitemap); got != 2 {
		t.Errorf("sitemap remaining = %d, want 2", got)
	}
}

// The keystone safety test: an old, non-latest, change-less snapshot is deleted,
// but an equally-old snapshot that recorded a change is KEPT (with its change row),
// and the newest snapshot per URL is never deleted even when it is change-less.
func TestDeleteStaleSnapshotsPreservesHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")

	oldNoChange := seedSnapshot(t, db, url, now.Add(-40*day), nil) // eligible → DELETE
	oldChange := seedSnapshot(t, db, url, now.Add(-39*day), nil)   // has a change → KEEP
	recent := seedSnapshot(t, db, url, now.Add(-2*day), nil)       // younger than cutoff → KEEP
	latest := seedSnapshot(t, db, url, now.Add(-1*time.Hour), nil) // newest per URL → KEEP
	seedChange(t, db, url, oldChange)

	cutoff := now.Add(-30 * day)
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 1, 5000)
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	if snapshotExists(t, db, oldNoChange) {
		t.Errorf("oldNoChange should have been deleted")
	}
	for name, id := range map[string]int64{"oldChange": oldChange, "recent": recent, "latest": latest} {
		if !snapshotExists(t, db, id) {
			t.Errorf("%s (id=%d) was deleted, want kept", name, id)
		}
	}
	// The change row for oldChange must survive (it was never cascaded).
	var changeCount int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM changes WHERE snapshot_id=?", oldChange).Scan(&changeCount); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if changeCount != 1 {
		t.Errorf("change rows for oldChange = %d, want 1", changeCount)
	}
}

// Layer 2 drains an eligible set larger than one chunk across multiple batches.
func TestDeleteStaleSnapshotsChunking(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	seedSnapshot(t, db, url, now.Add(-1*time.Hour), nil) // latest, protected (rn=1)
	for i := 0; i < 8; i++ {
		seedSnapshot(t, db, url, now.Add(-(40-time.Duration(i))*day), nil) // all old, change-less
	}

	cutoff := now.Add(-30 * day)
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 1, 3) // chunk=3 → batches 3,3,2
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if n != 8 {
		t.Errorf("deleted = %d, want 8", n)
	}
	var remaining int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM snapshots WHERE url_id=?", url).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 1 { // only the protected latest
		t.Errorf("remaining = %d, want 1", remaining)
	}
}

// FIX A regression: with keep=0, the unclamped "rn <= 0" protection would protect
// NOTHING, so the only (and newest) change-less old snapshot per URL would be deleted,
// losing the diff baseline. The store floor (keep<1 → keep=1) must protect it.
func TestDeleteStaleSnapshotsKeepFloor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	// A single old, change-less snapshot — its own newest row (the diff baseline).
	only := seedSnapshot(t, db, url, now.Add(-40*day), nil)

	cutoff := now.Add(-30 * day)
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 0, 5000) // keep=0 → must floor to 1
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0 (newest per URL is always protected)", n)
	}
	if !snapshotExists(t, db, only) {
		t.Errorf("the only/newest snapshot was deleted — diff baseline lost")
	}
}

// FIX B regression: SaveSnapshot must store fetched_at in UTC. modernc.org/sqlite
// stores time.Time as a TEXT timestamp, and DeleteStaleSnapshots compares
// "fetched_at < ?" against a UTC cutoff lexically. If a snapshot's true instant is
// newer than the cutoff but its LOCAL wall-clock string sorts before it, the row is
// wrongly deleted unless we normalize to UTC at storage.
func TestDeleteStaleSnapshotsTimezone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Fixed -8h zone; no LoadLocation (tzdata may be absent on the host).
	west := time.FixedZone("X", -8*3600)

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")

	// snapshotC is the genuinely-newest row (rn=1), so the rn<=keep clause — not the
	// age boundary — is what protects IT. This forces snapshotA's survival to depend
	// SOLELY on the fetched_at < cutoff boundary (the zone-correctness under test).
	cInstant := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).In(west)
	// snapshotA true instant 2026-04-01 05:00 UTC — 5h AFTER the cutoff, so its instant
	// is newer than the cutoff and it must SURVIVE. But in the -8h zone its wall clock
	// is "2026-03-31 21:00", whose stored string sorts BEFORE the cutoff string: under
	// the local-zone bug the lexical "fetched_at < cutoff" wrongly matches → deleted.
	aInstant := time.Date(2026, 4, 1, 5, 0, 0, 0, time.UTC).In(west)
	// snapshotB true instant 2026-03-01 (clearly older than the cutoff). Must be deleted.
	bInstant := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).In(west)
	snapshotC := seedSnapshot(t, db, url, cInstant, nil)
	snapshotA := seedSnapshot(t, db, url, aInstant, nil)
	snapshotB := seedSnapshot(t, db, url, bInstant, nil)

	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 1, 5000)
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if !snapshotExists(t, db, snapshotC) {
		t.Errorf("snapshotC (newest, rn=1) was deleted, want kept")
	}
	if !snapshotExists(t, db, snapshotA) {
		t.Errorf("snapshotA (true 2026-04-01 05:00 UTC, newer than cutoff) was deleted — local-zone lexical skew")
	}
	if snapshotExists(t, db, snapshotB) {
		t.Errorf("snapshotB (true 2026-03-01, older than cutoff) should have been deleted")
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only snapshotB)", n)
	}
}

// FIX D coverage: pin PARTITION BY url_id for Layer 1. urlB's newest snapshot is
// ABSOLUTELY OLDER than ALL of urlA's snapshots; a regression to a global ORDER BY
// would null urlB's newest. Per-URL partitioning must keep each URL's own newest.
func TestNullStaleRawHTMLMultiURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hour := time.Hour

	site := seedSite(t, db, "https://a.com")
	urlA := seedURL(t, db, site, "https://a.com/a")
	urlB := seedURL(t, db, site, "https://a.com/b")

	// urlA: two recent snapshots.
	aOld := seedSnapshot(t, db, urlA, now.Add(-10*hour), []byte("<a old>"))
	aNew := seedSnapshot(t, db, urlA, now.Add(-1*hour), []byte("<a new>"))
	// urlB: two snapshots, BOTH older than all of urlA's (its newest < urlA's oldest).
	bOld := seedSnapshot(t, db, urlB, now.Add(-100*hour), []byte("<b old>"))
	bNew := seedSnapshot(t, db, urlB, now.Add(-50*hour), []byte("<b new>"))

	if _, err := db.NullStaleRawHTML(ctx, 1); err != nil {
		t.Fatalf("NullStaleRawHTML: %v", err)
	}
	// Each URL's own newest retains raw_html — even urlB's globally-old newest.
	if rawHTMLIsNull(t, db, aNew) {
		t.Errorf("urlA newest raw_html nulled, want retained")
	}
	if rawHTMLIsNull(t, db, bNew) {
		t.Errorf("urlB newest raw_html nulled (global ORDER BY regression), want retained")
	}
	// Each URL's older row is nulled.
	if !rawHTMLIsNull(t, db, aOld) {
		t.Errorf("urlA older raw_html not nulled")
	}
	if !rawHTMLIsNull(t, db, bOld) {
		t.Errorf("urlB older raw_html not nulled")
	}
}

// FIX D coverage: pin PARTITION BY url_id for Layer 2. Each URL independently keeps
// its own newest (rn=1) and deletes its own older change-less rows, even though
// urlB's newest is globally older than all of urlA's.
func TestDeleteStaleSnapshotsMultiURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	urlA := seedURL(t, db, site, "https://a.com/a")
	urlB := seedURL(t, db, site, "https://a.com/b")

	// urlA: an old change-less row + a recent newest.
	aOld := seedSnapshot(t, db, urlA, now.Add(-40*day), nil)
	aNew := seedSnapshot(t, db, urlA, now.Add(-1*time.Hour), nil)
	// urlB: two old change-less rows; its newest is older than ALL of urlA's.
	bOld := seedSnapshot(t, db, urlB, now.Add(-100*day), nil)
	bNew := seedSnapshot(t, db, urlB, now.Add(-50*day), nil)

	cutoff := now.Add(-30 * day)
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 1, 5000)
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if n != 2 { // aOld + bOld
		t.Errorf("deleted = %d, want 2", n)
	}
	// Each URL's own newest survives (rn=1 within its own partition).
	if !snapshotExists(t, db, aNew) {
		t.Errorf("urlA newest deleted, want kept")
	}
	if !snapshotExists(t, db, bNew) {
		t.Errorf("urlB newest deleted (global ORDER BY regression), want kept")
	}
	// Each URL's older change-less row is deleted.
	if snapshotExists(t, db, aOld) {
		t.Errorf("urlA old change-less row not deleted")
	}
	if snapshotExists(t, db, bOld) {
		t.Errorf("urlB old change-less row not deleted")
	}
}

// FIX E coverage: the rn<=keep latest-protection in isolation from the age filter.
// ALL of this URL's snapshots are older than the cutoff and change-less, so the age
// filter alone would delete every row. Only the rn<=keep NOT IN clause can save the
// newest — proving the latest-protection works on overage rows (catches "rn < keep"
// off-by-one / a dropped NOT IN clause).
func TestDeleteStaleSnapshotsProtectsOverageLatest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	newest := seedSnapshot(t, db, url, now.Add(-31*day), nil) // rn=1, but past cutoff
	seedSnapshot(t, db, url, now.Add(-40*day), nil)
	seedSnapshot(t, db, url, now.Add(-50*day), nil)

	cutoff := now.Add(-30 * day) // all three are older than this
	n, err := db.DeleteStaleSnapshots(ctx, cutoff, 1, 5000)
	if err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2 (the two older rows)", n)
	}
	if !snapshotExists(t, db, newest) {
		t.Errorf("newest overage row (rn=1) deleted — rn<=keep protection failed")
	}
	var remaining int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM snapshots WHERE url_id=?", url).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1", remaining)
	}
}

func TestApplyRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	// 3 snapshots with raw_html; oldest two are old + change-less (Layer-2 eligible).
	seedSnapshot(t, db, url, now.Add(-40*day), []byte("old1"))
	seedSnapshot(t, db, url, now.Add(-39*day), []byte("old2"))
	latest := seedSnapshot(t, db, url, now.Add(-1*time.Hour), []byte("new"))
	for i := 0; i < 5; i++ {
		seedFileSnapshot(t, db, site, model.FileKindRobots, now.Add(time.Duration(i)*time.Hour))
	}

	res, err := db.ApplyRetention(ctx, RetentionPolicy{
		RawHTMLKeep:       1,
		SnapshotMaxAge:    30 * day,
		FileSnapshotsKeep: 2,
		Chunk:             5000,
	}, now)
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	// Layer 1 ran before Layer 2 deleted the old rows, so it nulled both old ones (2).
	if res.RawHTMLNulled != 2 {
		t.Errorf("RawHTMLNulled = %d, want 2", res.RawHTMLNulled)
	}
	if res.SnapshotsDeleted != 2 {
		t.Errorf("SnapshotsDeleted = %d, want 2", res.SnapshotsDeleted)
	}
	if res.FileSnapshotsTrimmed != 3 {
		t.Errorf("FileSnapshotsTrimmed = %d, want 3", res.FileSnapshotsTrimmed)
	}
	if rawHTMLIsNull(t, db, latest) {
		t.Errorf("latest raw_html nulled, want retained")
	}
}

// SnapshotMaxAge ≤ 0 disables Layer 2; Layer 1 still runs.
func TestApplyRetentionDisablesLayer2(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	seedSnapshot(t, db, url, now.Add(-40*day), []byte("old"))
	seedSnapshot(t, db, url, now.Add(-1*time.Hour), []byte("new"))

	res, err := db.ApplyRetention(ctx, RetentionPolicy{
		RawHTMLKeep:       1,
		SnapshotMaxAge:    0, // disabled
		FileSnapshotsKeep: 2,
		Chunk:             5000,
	}, now)
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if res.SnapshotsDeleted != 0 {
		t.Errorf("SnapshotsDeleted = %d, want 0 (Layer 2 disabled)", res.SnapshotsDeleted)
	}
	var total int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM snapshots WHERE url_id=?", url).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 2 {
		t.Errorf("snapshots remaining = %d, want 2", total)
	}
}

func TestCompactShrinksDatabase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	big := make([]byte, 64*1024)
	for i := 0; i < 60; i++ {
		seedSnapshot(t, db, url, base.Add(time.Duration(i)*time.Minute), big)
	}

	pageCount := func() int {
		var n int
		if err := db.Read().QueryRow("PRAGMA page_count").Scan(&n); err != nil {
			t.Fatalf("page_count: %v", err)
		}
		return n
	}
	before := pageCount()

	// Free the pages, then compact.
	if err := db.WriteTx(ctx, func(tx Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM snapshots")
		return e
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := pageCount()
	if after >= before {
		t.Errorf("page_count after compact = %d, want < before %d", after, before)
	}
}

// NullStaleRawHTML floors keep to 1: a keep=0 must NOT null the newest snapshot's
// body (it would otherwise null every row, since rn > 0 matches all).
func TestNullStaleRawHTMLKeepFloor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	site := seedSite(t, db, "https://a.com")
	url := seedURL(t, db, site, "https://a.com/p")
	s1 := seedSnapshot(t, db, url, base.Add(1*time.Hour), []byte("<html>1</html>"))
	newest := seedSnapshot(t, db, url, base.Add(2*time.Hour), []byte("<html>2</html>"))

	n, err := db.NullStaleRawHTML(ctx, 0) // floored to 1
	if err != nil {
		t.Fatalf("NullStaleRawHTML: %v", err)
	}
	if n != 1 {
		t.Errorf("nulled = %d, want 1 (keep floored to 1)", n)
	}
	if rawHTMLIsNull(t, db, newest) {
		t.Errorf("newest raw_html nulled with keep=0, want retained (floored to 1)")
	}
	if !rawHTMLIsNull(t, db, s1) {
		t.Errorf("older raw_html not nulled, want nulled")
	}
}

// TrimFileSnapshots floors keep to 2: a keep<2 must NOT strip the file-diff baseline
// (diff.CompareFile needs a prior). keep=0 would otherwise delete every row.
func TestTrimFileSnapshotsKeepFloor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	site := seedSite(t, db, "https://a.com")
	for i := 0; i < 4; i++ {
		seedFileSnapshot(t, db, site, model.FileKindRobots, base.Add(time.Duration(i)*time.Hour))
	}

	count := func() int {
		var c int
		if err := db.Read().QueryRow(
			"SELECT COUNT(*) FROM file_snapshots WHERE site_id=? AND kind=?",
			site, string(model.FileKindRobots)).Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c
	}

	n, err := db.TrimFileSnapshots(ctx, 0) // floored to 2
	if err != nil {
		t.Fatalf("TrimFileSnapshots: %v", err)
	}
	if n != 2 { // 4 - floor(2) = 2 deleted
		t.Errorf("trimmed = %d, want 2 (keep floored to 2)", n)
	}
	if got := count(); got != 2 {
		t.Errorf("remaining = %d, want 2 (a prior must survive for CompareFile)", got)
	}
}
