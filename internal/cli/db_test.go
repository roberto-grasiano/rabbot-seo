package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestRunDBCompactRefusesWhenDaemonUp(t *testing.T) {
	// A live control endpoint answering /v1/health 200 means a daemon is up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := control.NewClientWithBaseURL(srv.URL, "tok")

	err := runDBCompact(context.Background(), client, filepath.Join(t.TempDir(), "k.db"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("err = %v, want a 'daemon running' refusal", err)
	}
}

func TestRunDBCompactCompactsWhenDaemonDown(t *testing.T) {
	// A server that is immediately closed → connection refused → ErrDaemonNotRunning.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	client := control.NewClientWithBaseURL(url, "tok")

	// Seed a DB with MODEST data — ~20 rows of ~4KB (~80KB total, ~20 pages),
	// well under SQLite's ~1000-page auto-checkpoint threshold so the checkpoint
	// does NOT fire on its own during the writes. We then delete the rows so VACUUM
	// has freelist pages to reclaim. The DB is WAL mode, so VACUUM rebuilds into the
	// -wal file; the main .db only shrinks after an explicit checkpoint(TRUNCATE).
	// Without the checkpoint fix in runDBCompact, os.Stat sees the un-shrunk .db and
	// reports "reclaimed 0".
	dbPath := filepath.Join(t.TempDir(), "k.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	chunk := make([]byte, 4*1024)
	var siteID int64
	_ = db.WriteTx(context.Background(), func(tx store.Tx) error {
		res, _ := tx.ExecContext(context.Background(),
			`INSERT INTO sites (base_url, name, created_at, updated_at) VALUES (?,?,?,?)`,
			"https://a.com", "T", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		siteID, _ = res.LastInsertId()
		return nil
	})
	var urlID int64
	_ = db.WriteTx(context.Background(), func(tx store.Tx) error {
		res, _ := tx.ExecContext(context.Background(),
			`INSERT INTO urls (site_id, url, first_seen, next_check_at) VALUES (?,?,?,?)`,
			siteID, "https://a.com/p", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		urlID, _ = res.LastInsertId()
		return nil
	})
	for i := 0; i < 20; i++ {
		_, _ = db.SaveSnapshot(context.Background(), model.Snapshot{
			URLID: urlID, FetchedAt: time.Now().UTC().Add(time.Duration(i) * time.Minute), RawHTML: chunk,
		})
	}
	_ = db.WriteTx(context.Background(), func(tx store.Tx) error {
		_, e := tx.ExecContext(context.Background(), "DELETE FROM snapshots")
		return e
	})
	_ = db.Close()

	before, _ := os.Stat(dbPath)
	var out bytes.Buffer
	if err := runDBCompact(context.Background(), client, dbPath, &out); err != nil {
		t.Fatalf("runDBCompact: %v", err)
	}

	// The on-disk .db must actually shrink after the compact+checkpoint.
	after, _ := os.Stat(dbPath)
	if before != nil && after != nil && after.Size() >= before.Size() {
		t.Errorf("db size after compact = %d, want < before %d", after.Size(), before.Size())
	}

	// The REPORTED reclaimed value must be positive: a real compaction that
	// reports "reclaimed 0" is the WAL-mode bug. Parse it from the writer output.
	report := out.String()
	if !strings.Contains(report, "reclaimed ") {
		t.Fatalf("output = %q, want a 'reclaimed N' line", report)
	}
	var bf, af, reclaimed int64
	if _, err := fmt.Sscanf(report, "compacted: %d → %d bytes (reclaimed %d)", &bf, &af, &reclaimed); err != nil {
		t.Fatalf("could not parse reclaimed from %q: %v", report, err)
	}
	if reclaimed <= 0 {
		t.Errorf("reported reclaimed = %d, want > 0 (WAL-mode compact reports zero without a post-VACUUM checkpoint)", reclaimed)
	}
}
