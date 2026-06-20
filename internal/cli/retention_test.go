package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestBuildRetentionPolicy(t *testing.T) {
	cfg := config.Defaults()
	p := buildRetentionPolicy(&cfg)
	if p.RawHTMLKeep != 1 || p.FileSnapshotsKeep != 10 {
		t.Errorf("policy keeps = (%d,%d), want (1,10)", p.RawHTMLKeep, p.FileSnapshotsKeep)
	}
	if p.SnapshotMaxAge != 720*time.Hour {
		t.Errorf("SnapshotMaxAge = %v, want 720h", p.SnapshotMaxAge)
	}
	if p.Chunk != 5000 {
		t.Errorf("Chunk = %d, want 5000", p.Chunk)
	}
}

func openCLITestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRegisterRetentionSweepGating(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openCLITestStore(t)

	// Enabled → registers exactly one job.
	cfg := config.Defaults()
	s1, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s1.Shutdown() })
	registered, err := registerRetentionSweep(context.Background(), logger, s1, db, &cfg)
	if err != nil {
		t.Fatalf("registerRetentionSweep: %v", err)
	}
	if !registered || len(s1.Jobs()) != 1 {
		t.Errorf("enabled: registered=%v jobs=%d, want true,1", registered, len(s1.Jobs()))
	}

	// Disabled → registers nothing.
	cfg.Retention.Enabled = false
	s2, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s2.Shutdown() })
	registered, err = registerRetentionSweep(context.Background(), logger, s2, db, &cfg)
	if err != nil {
		t.Fatalf("registerRetentionSweep (disabled): %v", err)
	}
	if registered || len(s2.Jobs()) != 0 {
		t.Errorf("disabled: registered=%v jobs=%d, want false,0", registered, len(s2.Jobs()))
	}
}
