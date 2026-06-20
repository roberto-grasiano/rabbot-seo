package fsatomic

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWrite_RoundTripAndMode is the baseline: writing to a fresh path creates
// the file with the requested content and the requested owner-only file mode,
// and creates any missing parent directories at the requested dir mode.
func TestWrite_RoundTripAndMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "nested", "deeper", "out.json")
	data := []byte(`{"hello":"world"}` + "\n")

	if err := Write(path, data, 0o600, 0o700); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content round-trip mismatch: got %q want %q", got, data)
	}

	// File written at the requested mode.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Created parent dir at the requested dir mode.
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if runtime.GOOS != "windows" && di.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir mode = %o, want 0700", di.Mode().Perm())
	}
}

// TestWrite_OverwriteTightensModeAndLeavesNoTemp guards the durability/security
// contract: overwriting a pre-existing file with looser perms replaces it
// atomically with complete new content, tightens the final file to the
// requested mode, and leaves no stray temp file behind in the directory.
func TestWrite_OverwriteTightensModeAndLeavesNoTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Pre-existing file with loose 0644 perms — Write must tighten to 0600.
	if err := os.WriteFile(path, []byte("old: content\nthat is longer\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newData := []byte("new: 1\n")
	if err := Write(path, newData, 0o600, 0o700); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(newData) {
		t.Fatalf("overwrite content wrong: got %q want %q", got, newData)
	}

	// Perms tightened to owner-only even though the prior file was 0644.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("want 0600, got %o", fi.Mode().Perm())
	}

	// No temp file leaked into the directory: only config.yaml remains.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only config.yaml, got %v", names)
	}
}

// TestWrite_ExistingDirIsNotChmodded confirms Write does not loosen/alter an
// EXISTING parent directory's mode — MkdirAll is a no-op on an existing dir, so
// the dirMode argument applies only to dirs Write actually creates. (config
// writes into a pre-created config dir and must not disturb it.)
func TestWrite_ExistingDirIsNotChmodded(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("unix file modes only")
	}

	dir := t.TempDir()
	// Set the existing dir to a distinctive 0755 that differs from dirMode.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "out.txt")

	if err := Write(path, []byte("x"), 0o600, 0o700); err != nil {
		t.Fatalf("Write: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir mode changed: got %o, want 0755 (Write must not chmod an existing dir)", di.Mode().Perm())
	}
}

// TestWrite_DirFsyncPathDirCreatedAndDurable exercises the full create-dirs +
// parent-dir-fsync path on a freshly created nested directory. The parent-dir
// fsync is best-effort (some platforms refuse), so success is measured by the
// file being present, complete, and correctly moded — a botched sync ordering
// (e.g. syncing a closed fd as a hard error) would surface here as a write
// failure or truncated content.
func TestWrite_DirFsyncPathDirCreatedAndDurable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// A deep, freshly created path forces MkdirAll + a parent-dir fsync on a
	// directory Write itself created.
	path := filepath.Join(root, "a", "b", "c", "payload.bin")
	data := []byte(strings.Repeat("durable-bytes-", 1024))

	if err := Write(path, data, 0o600, 0o700); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("durable content mismatch: got %d bytes want %d bytes", len(got), len(data))
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", fi.Mode().Perm())
	}
}

// TestWrite_ErrorWhenDirIsAFile confirms a sane wrapped error (not a panic)
// when the parent path cannot be created because a component is a regular file.
func TestWrite_ErrorWhenDirIsAFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Make "root/file" a regular file, then try to write under it as if a dir.
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	path := filepath.Join(blocker, "child", "out.txt")

	err := Write(path, []byte("y"), 0o600, 0o700)
	if err == nil {
		t.Fatalf("expected error when a path component is a regular file, got nil")
	}
}
