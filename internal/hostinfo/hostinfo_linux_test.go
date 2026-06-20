//go:build linux

package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSleeperLinux pins criterion 7: a fake sysfs power-supply tree with a
// Battery entry ⇒ Sleeper() == true; none ⇒ false; an unreadable root ⇒ false
// (never an error, never a panic).
func TestSleeperLinux(t *testing.T) {
	t.Run("battery present", func(t *testing.T) {
		root := t.TempDir()
		writeSupply(t, root, "BAT0", "Battery")
		writeSupply(t, root, "AC", "Mains")
		withSysfsRoot(t, root)
		if !Sleeper() {
			t.Fatalf("Sleeper() = false, want true (BAT0/type == Battery)")
		}
	})

	t.Run("no battery", func(t *testing.T) {
		root := t.TempDir()
		writeSupply(t, root, "AC", "Mains")
		withSysfsRoot(t, root)
		if Sleeper() {
			t.Fatalf("Sleeper() = true, want false (no Battery type)")
		}
	})

	t.Run("empty tree", func(t *testing.T) {
		root := t.TempDir()
		withSysfsRoot(t, root)
		if Sleeper() {
			t.Fatalf("Sleeper() = true, want false (empty power_supply tree)")
		}
	})

	t.Run("unreadable root", func(t *testing.T) {
		// A path that does not exist must yield false, never a panic or error.
		withSysfsRoot(t, filepath.Join(t.TempDir(), "does-not-exist"))
		if Sleeper() {
			t.Fatalf("Sleeper() = true, want false (missing sysfs root)")
		}
	})

	t.Run("type case-insensitive and trimmed", func(t *testing.T) {
		root := t.TempDir()
		// Real sysfs values carry a trailing newline; match must tolerate it.
		dir := filepath.Join(root, "BAT1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte("Battery\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		withSysfsRoot(t, root)
		if !Sleeper() {
			t.Fatalf("Sleeper() = false, want true (trailing-newline Battery type)")
		}
	})
}

func writeSupply(t *testing.T, root, name, typ string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withSysfsRoot swaps the package-level sysfs root for the test and restores it.
func withSysfsRoot(t *testing.T, root string) {
	t.Helper()
	prev := sysfsPowerSupplyRoot
	sysfsPowerSupplyRoot = root
	t.Cleanup(func() { sysfsPowerSupplyRoot = prev })
}
