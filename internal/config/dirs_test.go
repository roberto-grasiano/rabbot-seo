package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"
)

// TestDataDirPath verifies the pure resolver used by read-only CLI commands.
func TestDataDirPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	xdg.Reload()

	// Empty override: must resolve to per-OS default — NOT a relative/empty path.
	got := DataDirPath("")
	want := filepath.Join(tmp, appName)
	if got != want {
		t.Errorf("DataDirPath(\"\") = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("DataDirPath(\"\") returned non-absolute path %q", got)
	}

	// Explicit override: returned verbatim.
	got2 := DataDirPath("/explicit/path")
	if got2 != "/explicit/path" {
		t.Errorf("DataDirPath(\"/explicit/path\") = %q, want %q", got2, "/explicit/path")
	}
}

// TestDataDirPathParityWithResolveDataDir asserts that DataDirPath("") returns
// the same path that ResolveDataDir("") resolves to (mkdir side-effect aside).
func TestDataDirPathParityWithResolveDataDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	xdg.Reload()

	resolved, err := ResolveDataDir("")
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}

	pure := DataDirPath("")
	if pure != resolved {
		t.Errorf("DataDirPath(\"\") = %q, ResolveDataDir(\"\") = %q — must match", pure, resolved)
	}

	// ResolveDataDir creates the dir; DataDirPath does not (only the resolved path exists).
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("ResolveDataDir should have created %q: %v", resolved, err)
	}
}

func TestConfigFilePath(t *testing.T) {
	dir := t.TempDir()
	got := ConfigFilePath(dir)
	want := filepath.Join(dir, "config.yaml")
	if got != want {
		t.Errorf("ConfigFilePath = %q, want %q", got, want)
	}
}

func TestResolveDataDirHonorsOverride(t *testing.T) {
	want := t.TempDir()
	got, err := ResolveDataDir(want)
	if err != nil {
		t.Fatalf("ResolveDataDir error: %v", err)
	}
	if got != want {
		t.Errorf("ResolveDataDir(%q) = %q, want override returned verbatim", want, got)
	}
}

func TestResolveDataDirDefaultNonEmpty(t *testing.T) {
	got, err := ResolveDataDir("")
	if err != nil {
		t.Fatalf("ResolveDataDir error: %v", err)
	}
	if got == "" {
		t.Error("ResolveDataDir(\"\") returned empty path; expected a per-OS default")
	}
}

func TestResolveConfigDirDefaultNonEmpty(t *testing.T) {
	got, err := ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir error: %v", err)
	}
	if got == "" {
		t.Error("ResolveConfigDir returned empty path; expected a per-OS default")
	}
}

// TestConfigDirPathHonorsOverride is the cross-platform hermeticity regression
// test: an explicitly-set XDG_CONFIG_HOME override MUST be honored on every OS
// (not just Linux). os.UserConfigDir consults the var only on Unix, so on
// macOS/Windows the override was silently ignored and tests wrote to the real
// user config dir, polluting each other. This pins the env-first contract so the
// CI runners on macos-latest/windows-latest stay isolated. The assertion is
// path-separator agnostic (filepath.Join), so it is byte-correct on any GOOS and
// runnable on Linux.
func TestConfigDirPathHonorsOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", override)

	got, err := ConfigDirPath()
	if err != nil {
		t.Fatalf("ConfigDirPath error: %v", err)
	}
	want := filepath.Join(override, appName)
	if got != want {
		t.Errorf("ConfigDirPath() with XDG_CONFIG_HOME=%q = %q, want %q (override must win on every OS)", override, got, want)
	}
}

// TestResolveConfigDirHonorsOverride proves the creating resolver routes through
// the same override-honoring path and actually creates the directory under the
// override (so the daemon, CLI, and tests all agree on one isolated location).
func TestResolveConfigDirHonorsOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", override)

	got, err := ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir error: %v", err)
	}
	want := filepath.Join(override, appName)
	if got != want {
		t.Errorf("ResolveConfigDir() with XDG_CONFIG_HOME=%q = %q, want %q", override, got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("ResolveConfigDir should have created %q: %v", got, err)
	}
}

// TestConfigDirPathFallsBackWhenUnset pins that with no override set, the resolver
// defers to the per-OS default (os.UserConfigDir) and never returns empty — the
// default behavior stays unchanged. An unset (or non-absolute) override must not
// short-circuit the per-OS default.
func TestConfigDirPathFallsBackWhenUnset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")

	got, err := ConfigDirPath()
	if err != nil {
		t.Fatalf("ConfigDirPath error: %v", err)
	}
	if got == "" {
		t.Fatal("ConfigDirPath returned empty with no override; expected the per-OS default")
	}
	// It must end with the app subdir under whatever per-OS base was chosen.
	if filepath.Base(got) != appName {
		t.Errorf("ConfigDirPath() = %q, want a path ending in %q", got, appName)
	}
}
