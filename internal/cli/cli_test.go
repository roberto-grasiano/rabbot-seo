package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bi := BuildInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-06-01"}
	root := NewRootCmd(bi)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestVersionCommand(t *testing.T) {
	out, err := runRoot(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out, "1.2.3") {
		t.Errorf("version output %q missing version 1.2.3", out)
	}
}

func TestConfigPathCommand(t *testing.T) {
	out, err := runRoot(t, "config", "path")
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if !strings.Contains(out, "config.yaml") {
		t.Errorf("config path output %q missing config.yaml", out)
	}
}

func TestRootHasExpectedSubcommands(t *testing.T) {
	root := NewRootCmd(BuildInfo{})
	want := []string{"init", "version", "config", "service", "run"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("root command missing subcommand %q", w)
		}
	}
}

func TestDatabasePath(t *testing.T) {
	dataDir := filepath.Join(string(filepath.Separator), "var", "lib", "rabbot")
	cfg := &config.Config{DataDir: dataDir}
	got := databasePath(cfg)
	// databasePath joins with filepath.Join, so the expected path uses the
	// OS-native separator (\ on Windows, / elsewhere) — assert with filepath.Join,
	// not a hardcoded "/var/lib/rabbot/rabbot.db" literal.
	want := filepath.Join(dataDir, "rabbot.db")
	if got != want {
		t.Errorf("databasePath = %q, want %q", got, want)
	}
}

// TestDatabasePathResolvesDefault is the load-bearing regression test: with an
// empty DataDir (the default), databasePath must return the absolute per-OS
// default path — not the relative "rabbot.db" that would open a stray DB in
// the process CWD instead of the daemon's DB.
func TestDatabasePathResolvesDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	xdg.Reload()

	cfg := &config.Config{} // DataDir intentionally empty (the default)
	got := databasePath(cfg)

	wantDir := filepath.Join(tmp, "rabbot")
	want := filepath.Join(wantDir, "rabbot.db")
	if got != want {
		t.Errorf("databasePath(empty DataDir) = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("databasePath(empty DataDir) returned non-absolute path %q — opens stray CWD db", got)
	}
}

func TestControlAddr(t *testing.T) {
	cfg := &config.Config{}
	cfg.Control.Port = 7777
	got := controlAddr(cfg)
	if got != "127.0.0.1:7777" {
		t.Errorf("controlAddr = %q, want 127.0.0.1:7777", got)
	}
}

func TestNewControlClient(t *testing.T) {
	cfg := &config.Config{}
	cfg.Control.Port = 7777
	c, err := newControlClient(cfg)
	if err != nil {
		t.Fatalf("newControlClient: %v", err)
	}
	if c == nil {
		t.Fatal("newControlClient returned nil client")
	}
}
