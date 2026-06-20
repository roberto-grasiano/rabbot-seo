package store_test

// B3 store-layer microbenchmarks (Design §1; acceptance criteria 3, 5).
//
// These quantify the pure-Go modernc.org/sqlite write/read path at realistic
// scale — the perf-sensitive design choice rabbot publishes honestly. Three
// invariants are load-bearing and each is defended:
//
//   - FILE DB, NEVER :memory:. Every bench opens store.Open against a temp .db
//     FILE so the WAL + fsync write cost is REAL; a :memory: bench would publish
//     a fake-fast number. TestSaveSnapshotBenchUsesFileDB pins this (the bench
//     DB path ends in ".db" and the file exists on disk).
//   - REALISTIC raw_html. raw_html is a stored column, so SaveSnapshot's write
//     cost is only honest when the fixture carries a typical body. The fixture's
//     RawHTML is a ~60 KiB benchcorpus.Article page (deterministic, SHA-pinned
//     in internal/benchcorpus), so the persisted bytes match a real article.
//   - SETUP EXCLUDED. Every bench does its seeding before b.ResetTimer() so only
//     the measured operation is timed; all benches call b.ReportAllocs().
//
// Run (smoke, reflects the shipped CGO_ENABLED=0 static binary):
//
//	CGO_ENABLED=0 go test -run '^$' -bench . -benchtime=1x ./internal/store/...
//
// Run the harness guard tests (race needs cgo, so CGO is forced on for -race):
//
//	CGO_ENABLED=1 go test -race ./internal/store/...

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// benchDBPath returns a temp-FILE path ending in ".db" for the bench database.
// It is the single source of the "never :memory:" invariant: openBenchStore
// opens THIS path, and TestSaveSnapshotBenchUsesFileDB asserts it ends in ".db"
// and exists on disk after Open. Keep the open path and the asserted path the
// same function so the guard test cannot drift from what the benches actually
// open (a future "speed it up with :memory:" edit would have to change this
// helper, which fails the test).
func benchDBPath(tb testing.TB) string {
	tb.Helper()
	return filepath.Join(tb.TempDir(), "bench.db")
}

// openBenchStore opens a fresh on-disk store at benchDBPath and registers
// cleanup. The DB lives in a real temp FILE so WAL + fsync costs are measured.
func openBenchStore(tb testing.TB) (*store.DB, string) {
	tb.Helper()
	path := benchDBPath(tb)
	db, err := store.Open(context.Background(), path)
	if err != nil {
		tb.Fatalf("store.Open(%q) error = %v", path, err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db, path
}

// benchArticleHTML is the deterministic ~60 KiB article body persisted as
// raw_html. It is computed once (benchcorpus.Page is pure) and reused so the
// generation cost is never inside the timed loop. The Article class is the
// "typical page" size class (48-80 KiB band), so this is the realistic stored
// body, not a synthetic empty shell.
var benchArticleHTML = benchcorpus.Page(benchcorpus.Article, 1)

// benchSnapshot builds a snapshot fixture for the given urlID and index. RawHTML
// carries the typical ~60 KiB article body (raw_html is a stored column); the
// remaining fields are realistic non-empty SEO values so the 32-column INSERT
// writes representative data, not NULLs. fetched_at varies with index so a
// per-URL series has a deterministic ORDER BY for LatestSnapshot.
func benchSnapshot(urlID int64, index int) model.Snapshot {
	return model.Snapshot{
		URLID:                  urlID,
		FetchedAt:              time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute),
		HTTPStatus:             200,
		RedirectChain:          "",
		ResponseTimeMS:         123,
		Title:                  fmt.Sprintf("Benchmark Article %d", index),
		MetaDescription:        "A representative meta description for the store write/read benchmark fixture.",
		MetaRobots:             "index,follow",
		Canonical:              fmt.Sprintf("https://corpus.example/article/%d", index),
		Hreflang:               `[{"lang":"en","href":"https://corpus.example/article/x"}]`,
		Headings:               `[{"level":1,"text":"Heading"}]`,
		WordCount:              6400,
		ContentSHA256:          fmt.Sprintf("%064x", index), // deterministic 64-hex digest stand-in
		ContentSimhash:         uint64(index) * 0x9e3779b97f4a7c15,
		SchemaTypes:            "Article",
		InternalLinkCount:      24,
		ExternalLinkCount:      3,
		IncomingCanonicalCount: 1,
		ImageCount:             4,
		MissingAltCount:        1,
		Indexable:              true,
		IndexabilityReason:     "indexable",
		RenderMode:             model.RenderServerRendered,
		ExtractionSource:       "html",
		RawHTML:                benchArticleHTML,
	}
}

// benchSeedSite adds one enabled site (PopDueURLs's JOIN requires s.enabled = 1)
// and returns its id. It is bench-local (the package already has a seedSite
// helper with a different signature in incidents_test.go).
func benchSeedSite(tb testing.TB, db *store.DB) int64 {
	tb.Helper()
	id, err := db.AddSite(context.Background(), model.Site{
		BaseURL: "https://corpus.example", Name: "Corpus", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		tb.Fatalf("AddSite() error = %v", err)
	}
	return id
}

// BenchmarkSaveSnapshot measures one snapshot INSERT (32 columns incl. a ~60 KiB
// raw_html) into the single-writer BEGIN IMMEDIATE transaction — i.e. one real
// WAL append + fsync. SaveSnapshot is insert-only (no ON CONFLICT; only
// UpsertURL upserts), so every iteration is a genuine row insert. The url_id is
// rotated across a small pre-seeded set of URLs so rows spread across pages of
// the snapshots table rather than all hanging off one url_id.
func BenchmarkSaveSnapshot(b *testing.B) {
	ctx := context.Background()
	db, _ := openBenchStore(b)
	siteID := benchSeedSite(b, db)

	const urlFanout = 16
	urlIDs := make([]int64, urlFanout)
	for i := range urlIDs {
		id, err := db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: fmt.Sprintf("https://corpus.example/article/%d", i),
			FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600, Importance: 1,
		})
		if err != nil {
			b.Fatalf("UpsertURL() error = %v", err)
		}
		urlIDs[i] = id
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := benchSnapshot(urlIDs[i%urlFanout], i)
		if _, err := db.SaveSnapshot(ctx, snap); err != nil {
			b.Fatalf("SaveSnapshot() error = %v", err)
		}
	}
}

// BenchmarkLatestSnapshot measures the indexed read-back of the most-recent
// snapshot for a URL (SELECT ... ORDER BY fetched_at DESC, id DESC LIMIT 1),
// including the ~60 KiB raw_html column scan into the model.Snapshot. A series
// of snapshots is seeded for one URL before the timer starts so the ORDER BY
// does real ranking work over multiple rows.
func BenchmarkLatestSnapshot(b *testing.B) {
	ctx := context.Background()
	db, _ := openBenchStore(b)
	siteID := benchSeedSite(b, db)

	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://corpus.example/article/0",
		FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600, Importance: 1,
	})
	if err != nil {
		b.Fatalf("UpsertURL() error = %v", err)
	}
	const seedHistory = 32
	for i := 0; i < seedHistory; i++ {
		if _, err := db.SaveSnapshot(ctx, benchSnapshot(urlID, i)); err != nil {
			b.Fatalf("SaveSnapshot(seed %d) error = %v", i, err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.LatestSnapshot(ctx, urlID); err != nil {
			b.Fatalf("LatestSnapshot() error = %v", err)
		}
	}
}

// BenchmarkPopDueURLs measures one scheduler pop — the indexed
// importance-ordered scan over a 10k-URL inventory the daemon runs on every
// tick (SELECT ... JOIN sites ... WHERE next_check_at <= ? AND enabled = 1
// ORDER BY importance DESC, next_check_at ASC LIMIT 50). batch=50 matches the
// shipped scheduler's Batch literal. The 10k URLs are seeded in ONE write
// transaction (10k individual UpsertURL calls would be 10k fsyncs) before the
// timer starts; importance and next_check_at are varied so the ORDER BY ranks
// real spread, and all rows are due (next_check_at in the past) so the LIMIT 50
// is the binding constraint.
func BenchmarkPopDueURLs(b *testing.B) {
	ctx := context.Background()
	db, _ := openBenchStore(b)
	siteID := benchSeedSite(b, db)

	const total = 10_000
	const batch = 50
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour) // all due

	// Batch-seed in a single BEGIN IMMEDIATE transaction (one fsync) rather than
	// 10k separate UpsertURL transactions. The INSERT mirrors UpsertURL's column
	// list; rows.Close()/rows.Err() are not needed (ExecContext, not a query).
	if err := db.WriteTx(ctx, func(tx store.Tx) error {
		for i := 0; i < total; i++ {
			// Vary importance and next_check_at so ORDER BY does real ranking.
			importance := float64(i%100) / 100.0
			next := base.Add(time.Duration(i) * time.Second)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO urls (site_id, url, first_seen, next_check_at, interval, importance, depth, in_sitemap, status_type, etag, last_modified, last_fetch_class)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				siteID, fmt.Sprintf("https://corpus.example/p/%d", i), now, next, int64(600),
				importance, 0, false, string(model.StatusPage), "", "", string(model.FetchOK)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed 10k URLs: %v", err)
	}

	// Sanity: the seed produced a full batch's worth of due rows, so the bench
	// measures a real LIMIT-50 scan, not an empty result. (Outside the timer.)
	if due, err := db.PopDueURLs(ctx, now, batch); err != nil {
		b.Fatalf("PopDueURLs(warmup) error = %v", err)
	} else if len(due) != batch {
		b.Fatalf("warmup PopDueURLs returned %d rows, want %d (seed is wrong)", len(due), batch)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.PopDueURLs(ctx, now, batch); err != nil {
			b.Fatalf("PopDueURLs() error = %v", err)
		}
	}
}

// TestSaveSnapshotBenchUsesFileDB is the falsifiable guard for lesson 2: the
// store benchmarks must run against an on-disk .db FILE (so WAL + fsync costs
// are real), NEVER :memory: (which would publish a fake-fast write number). It
// asserts the bench DB path the benches open ends in ".db", is not a :memory:
// or shared-cache DSN, and that store.Open actually materialized a file on disk
// at that path. A future "optimize the bench with :memory:" change would have
// to edit benchDBPath/openBenchStore — which this test then fails.
func TestSaveSnapshotBenchUsesFileDB(t *testing.T) {
	_, path := openBenchStore(t)

	if got := filepath.Ext(path); got != ".db" {
		t.Errorf("bench DB path extension = %q, want %q (path %q)", got, ".db", path)
	}
	if path == ":memory:" || filepath.Base(path) == ":memory:" {
		t.Errorf("bench DB path is an in-memory DSN (%q); benches must use an on-disk FILE so WAL+fsync costs are real", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bench DB file %q not on disk after store.Open: %v (an in-memory DB would leave nothing on disk)", path, err)
	}
	if info.IsDir() {
		t.Errorf("bench DB path %q is a directory, want a file", path)
	}
}

// TestStoreBenchHarnessIsHonest exercises the exact code paths the three store
// benchmarks measure, one iteration each, and asserts they actually persist and
// read what they claim — so a future edit that turns a bench into a no-op (e.g.
// a SaveSnapshot that silently fails, or a PopDueURLs seed that returns no rows)
// fails here rather than publishing a meaningless number.
func TestStoreBenchHarnessIsHonest(t *testing.T) {
	ctx := context.Background()
	db, _ := openBenchStore(t)
	siteID := benchSeedSite(t, db)

	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://corpus.example/article/0",
		FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600, Importance: 1,
	})
	if err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	// SaveSnapshot path: the fixture must carry the realistic ~60 KiB raw_html,
	// and the row must round-trip via LatestSnapshot with raw_html intact.
	if len(benchArticleHTML) < 48<<10 || len(benchArticleHTML) > 80<<10 {
		t.Fatalf("benchArticleHTML = %d bytes, want the Article band [48KiB,80KiB] (raw_html write cost would be unrealistic)", len(benchArticleHTML))
	}
	if _, err := db.SaveSnapshot(ctx, benchSnapshot(urlID, 0)); err != nil {
		t.Fatalf("SaveSnapshot() error = %v", err)
	}
	got, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot() error = %v", err)
	}
	if len(got.RawHTML) != len(benchArticleHTML) {
		t.Errorf("round-tripped raw_html = %d bytes, want %d (the stored column the write bench measures)", len(got.RawHTML), len(benchArticleHTML))
	}
	if got.Title != "Benchmark Article 0" {
		t.Errorf("round-tripped title = %q, want %q", got.Title, "Benchmark Article 0")
	}

	// PopDueURLs path: seed a handful of due URLs and confirm the bench's pop
	// returns rows in importance-desc order (the ORDER BY the bench measures).
	for i := 0; i < 5; i++ {
		if _, err := db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: fmt.Sprintf("https://corpus.example/p/%d", i),
			FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC().Add(-time.Hour),
			Interval: 600, Importance: float64(i) / 10.0,
		}); err != nil {
			t.Fatalf("UpsertURL(due %d) error = %v", i, err)
		}
	}
	due, err := db.PopDueURLs(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("PopDueURLs() error = %v", err)
	}
	if len(due) == 0 {
		t.Fatal("PopDueURLs returned no due rows; the bench would measure an empty scan")
	}
	for i := 1; i < len(due); i++ {
		if due[i-1].Importance < due[i].Importance {
			t.Errorf("PopDueURLs not ordered by importance DESC: %v then %v", due[i-1].Importance, due[i].Importance)
		}
	}
}
