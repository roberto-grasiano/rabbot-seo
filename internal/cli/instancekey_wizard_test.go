package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestEnsureWizardInstanceKeyCreatesDataDir pins the first-run fix: the wizard
// (`rabbot init`) is the first thing a user runs, so the data dir does not yet
// exist when the per-instance key is minted. DataDirPath is a pure resolver that
// does NOT create the directory, so calling LoadOrCreateInstanceKey on the bare
// path ENOENTs and the wizard never starts. ensureWizardInstanceKey must create
// the data dir (using the SAME override instanceKeyPath uses) before writing the
// key.
//
// A NON-EXISTENT nested override under t.TempDir() keeps the whole test off the
// real home dir / XDG dirs (no global pollution, no dependence on $HOME).
func TestEnsureWizardInstanceKeyCreatesDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "does", "not", "exist")
	cfg := config.Config{DataDir: dataDir}

	// The key path resolves under the not-yet-created data dir.
	keyPath := instanceKeyPath(&cfg)
	if keyPath != filepath.Join(dataDir, "instance.key") {
		t.Fatalf("instanceKeyPath resolved to %q, want %q", keyPath, filepath.Join(dataDir, "instance.key"))
	}

	// Prove the bug is load-bearing: minting the key directly on the bare,
	// not-yet-created data dir fails with ENOENT (this is exactly what regressed
	// launchWizard on a fresh install).
	if _, err := verify.LoadOrCreateInstanceKey(keyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: want ENOENT writing key into a missing dir, got %v", err)
	}

	// The fix: ensureWizardInstanceKey creates the dir first, then mints the key.
	key, err := ensureWizardInstanceKey(cfg)
	if err != nil {
		t.Fatalf("ensureWizardInstanceKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("want a 32-byte key, got %d bytes", len(key))
	}

	// The data dir now exists.
	if info, derr := os.Stat(dataDir); derr != nil || !info.IsDir() {
		t.Fatalf("data dir not created: stat err=%v", derr)
	}

	// The key file exists with 0600 perms.
	info, serr := os.Stat(keyPath)
	if serr != nil {
		t.Fatalf("instance.key not written: %v", serr)
	}
	// Windows maps NTFS attrs onto Go's mode bits, so an exact 0600
	// assertion can't hold there; assert perms only where they exist.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("instance.key perms = %o, want 600", perm)
		}
	}

	// Re-loading returns the SAME key (idempotent: dir already exists, key reused).
	key2, err := ensureWizardInstanceKey(cfg)
	if err != nil {
		t.Fatalf("ensureWizardInstanceKey (reload): %v", err)
	}
	if string(key2) != string(key) {
		t.Fatal("re-load minted a different key; must reuse the persisted one")
	}
}
