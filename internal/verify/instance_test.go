package verify

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateInstanceKey_MintsThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.key")

	k1, err := LoadOrCreateInstanceKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(k1) != instanceKeyBytes {
		t.Fatalf("key length = %d, want %d", len(k1), instanceKeyBytes)
	}

	// File must be 0600 (POSIX only — Windows maps NTFS attrs onto the mode
	// bits, so assert perms only where they exist).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm = %o, want 600", perm)
		}
	}

	// Second load returns the SAME key (stable, not re-minted).
	k2, err := LoadOrCreateInstanceKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(k1) != string(k2) {
		t.Fatalf("key changed across loads")
	}
}

func TestLoadOrCreateInstanceKey_MalformedFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.key")
	if err := os.WriteFile(path, []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateInstanceKey(path); err == nil {
		t.Fatal("expected error for malformed key file, got nil")
	}
}
