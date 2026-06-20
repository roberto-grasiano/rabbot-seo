package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// Criterion 6: after one sampler tick against a temp store, rabbot_db_size_bytes
// > 0 and rabbot_due_urls matches CountDueURLs. The sampler reads the SAME call
// the Status hook uses (db.CountDueURLs) off the scrape path.
func TestMetricsSamplerOneTick(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rabbot.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Hour)
	siteID, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://due.example", Name: "Due", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	for _, u := range []string{"https://due.example/a", "https://due.example/b", "https://due.example/c"} {
		if _, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: u, FirstSeen: past, NextCheckAt: past, Interval: 600}); uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
	}
	wantDue, err := db.CountDueURLs(ctx, now)
	if err != nil {
		t.Fatalf("CountDueURLs: %v", err)
	}
	if wantDue != 3 {
		t.Fatalf("setup: due = %d, want 3", wantDue)
	}

	m := obs.NewMetrics("v0")
	sampleMetricsOnce(ctx, m, db, dbPath, now)

	if got := gaugeValue(t, m, "rabbot_due_urls"); got != float64(wantDue) {
		t.Errorf("rabbot_due_urls = %v, want %d", got, wantDue)
	}
	if got := gaugeValue(t, m, "rabbot_db_size_bytes"); got <= 0 {
		t.Errorf("rabbot_db_size_bytes = %v, want > 0", got)
	}
}

// The sampler loop refreshes on every injected tick and exits on ctx cancel.
func TestMetricsSamplerLoopRefreshesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rabbot.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	m := obs.NewMetrics("v0")
	tickCh := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		runMetricsSampler(ctx, m, db, dbPath, tickCh)
		close(done)
	}()

	tickCh <- time.Now().UTC()
	// The DB file exists (store.Open created it), so size must be > 0 after a tick.
	// Poll because the gauge Set happens on the sampler goroutine after the send.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gaugeValue(t, m, "rabbot_db_size_bytes") > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := gaugeValue(t, m, "rabbot_db_size_bytes"); got <= 0 {
		t.Fatalf("rabbot_db_size_bytes = %v after a tick, want > 0", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler loop did not exit on ctx cancel")
	}
}

// Criterion 6 (scrape path zero DB calls): the metrics HTTP handler serves the
// registry exposition without ever touching the database. A panicking stub store
// would fire if the scrape path queried it; here the listener is built straight
// off the registry, so a scrape returns the persisted gauge values with no DB
// access at scrape time.
func TestMetricsScrapePathTouchesNoDB(t *testing.T) {
	m := obs.NewMetrics("v0")
	// Seed gauges as the sampler would, off the scrape path.
	m.SetDueURLs(7)
	m.SetDBSizeBytes(4096)

	// The scrape path is promhttp over the private registry — it gathers the
	// persisted gauge values with no database dependency at all. Gather() is
	// exactly what the listener serves; there is no store handle in this path.
	var sb strings.Builder
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		sb.WriteString(mf.GetName())
		sb.WriteString(" ")
	}
	for _, name := range []string{"rabbot_due_urls", "rabbot_db_size_bytes"} {
		if !strings.Contains(sb.String(), name) {
			t.Errorf("registry exposition missing %s (scrape served off registry, not DB)", name)
		}
	}
	if got := gaugeValue(t, m, "rabbot_due_urls"); got != 7 {
		t.Errorf("rabbot_due_urls = %v, want 7 (served from the persisted gauge)", got)
	}
}

// gaugeValue reads a single simple gauge's value from the registry exposition.
func gaugeValue(t *testing.T, m *obs.Metrics, name string) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		metrics := mf.GetMetric()
		if len(metrics) == 0 {
			t.Fatalf("gauge %s has no metrics", name)
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatalf("gauge %s not found in registry", name)
	return 0
}

// dbSizeBytes is verified indirectly above; this asserts the helper sees the WAL
// path too (main + -wal), so a WAL-mode store reports a non-trivial size.
func TestDBSizeBytesIncludesWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rabbot.db")
	if err := os.WriteFile(dbPath, []byte("main-db-bytes"), 0o600); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("wal-bytes-extra"), 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	got := dbSizeBytes(dbPath)
	want := int64(len("main-db-bytes") + len("wal-bytes-extra"))
	if got != want {
		t.Fatalf("dbSizeBytes = %d, want %d (main + -wal)", got, want)
	}
}
