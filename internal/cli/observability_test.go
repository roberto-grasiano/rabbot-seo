package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// writeScaffoldConfig writes the commented scaffold so the comment-preserving
// SetKeyYAML writer has a well-formed document to mutate (mirroring how init's
// other paths seed the file before mutating).
func writeScaffoldConfig(t *testing.T, cfgPath string) {
	t.Helper()
	if err := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0o600); err != nil {
		t.Fatalf("seed scaffold: %v", err)
	}
}

func newObservabilityInitForTest(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	c := newObservabilityCmd()
	c.SetArgs([]string{"init"})
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	return c, buf
}

// Criterion 10: `rabbot observability init` writes the bundle, sets metrics.addr
// to the loopback default when unset, prints the compose command + Grafana
// admin/admin warning + the daemon-restart note, never prompts, never execs
// docker.
func TestObservabilityInit_WritesBundleSetsAddrPrints(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir: %v", err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	writeScaffoldConfig(t, cfgPath)

	cmd, buf := newObservabilityInitForTest(t)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("observability init: %v", err)
	}

	// Bundle written under <config-dir>/observability/.
	bundleDir := filepath.Join(cfgDir, "observability")
	for _, rel := range []string{
		"docker-compose.observability.yml",
		"prometheus.yml",
		"grafana/dashboards/rabbot.json",
	} {
		if _, statErr := os.Stat(filepath.Join(bundleDir, rel)); statErr != nil {
			t.Fatalf("expected bundle file %q under %s: %v", rel, bundleDir, statErr)
		}
	}

	// metrics.addr set to the loopback default.
	loaded, lerr := config.Load(cfgPath, nil)
	if lerr != nil {
		t.Fatalf("reload config: %v", lerr)
	}
	if loaded.Metrics.Addr != metricsLoopbackAddr {
		t.Fatalf("metrics.addr = %q, want %q", loaded.Metrics.Addr, metricsLoopbackAddr)
	}

	got := buf.String()
	for _, want := range []string{
		"docker compose -f",                // the compose one-liner
		"docker-compose.observability.yml", // names the file
		"http://localhost:3000",            // dashboard URL
		"admin/admin",                      // credentials warning
		"restart",                          // daemon restart note
	} {
		if !strings.Contains(got, want) {
			t.Errorf("observability init output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// Criterion 10 (preserve a custom addr): a pre-existing non-empty metrics.addr
// survives a re-run — the generator only sets it when unset/empty.
func TestObservabilityInit_PreservesCustomAddr(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir: %v", err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	writeScaffoldConfig(t, cfgPath)

	const custom = "127.0.0.1:19999"
	if err := config.SetKeyYAML(cfgPath, "metrics.addr", custom); err != nil {
		t.Fatalf("seed custom metrics.addr: %v", err)
	}

	cmd, _ := newObservabilityInitForTest(t)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("observability init: %v", err)
	}

	loaded, lerr := config.Load(cfgPath, nil)
	if lerr != nil {
		t.Fatalf("reload config: %v", lerr)
	}
	if loaded.Metrics.Addr != custom {
		t.Fatalf("metrics.addr = %q, want preserved %q", loaded.Metrics.Addr, custom)
	}
}

// Criterion 10 (byte-identical re-run): a second `observability init` over the
// same config dir produces a byte-identical bundle (safe for an agent to retry).
func TestObservabilityInit_ByteIdenticalRerun(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir: %v", err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	writeScaffoldConfig(t, cfgPath)
	bundleDir := filepath.Join(cfgDir, "observability")

	cmd1, _ := newObservabilityInitForTest(t)
	if err := cmd1.Execute(); err != nil {
		t.Fatalf("observability init (first): %v", err)
	}
	first := readDirBytes(t, bundleDir)

	cmd2, _ := newObservabilityInitForTest(t)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("observability init (second): %v", err)
	}
	second := readDirBytes(t, bundleDir)

	if len(first) != len(second) {
		t.Fatalf("file set changed across runs: %d then %d files", len(first), len(second))
	}
	for rel, b1 := range first {
		if string(second[rel]) != string(b1) {
			t.Errorf("bundle file %q changed on re-run; want byte-identical", rel)
		}
	}
}

// readDirBytes reads every regular file under dir, keyed by its slash-path
// relative to dir.
func readDirBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // test path under t.TempDir
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// Criterion 10 (the --with-grafana parity): a non-TTY `init --with-grafana`
// performs the SAME generator writes and prints — the bundle lands and
// metrics.addr is set — through the shared generator seam, prompting nothing.
func TestInitWithGrafana_RunsGenerator(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatalf("ResolveConfigDir: %v", err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	// Force the non-TTY scaffold/headless route deterministically.
	prev := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prev })

	cmd := newInitCmd(BuildInfo{Version: "test"})
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--with-grafana",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init --with-grafana: %v", err)
	}

	bundleDir := filepath.Join(cfgDir, "observability")
	if _, statErr := os.Stat(filepath.Join(bundleDir, "prometheus.yml")); statErr != nil {
		t.Fatalf("--with-grafana did not write the bundle: %v", statErr)
	}
	loaded, lerr := config.Load(cfgPath, nil)
	if lerr != nil {
		t.Fatalf("reload config: %v", lerr)
	}
	if loaded.Metrics.Addr != metricsLoopbackAddr {
		t.Fatalf("--with-grafana metrics.addr = %q, want %q", loaded.Metrics.Addr, metricsLoopbackAddr)
	}
	if !strings.Contains(buf.String(), "admin/admin") {
		t.Errorf("--with-grafana output missing the Grafana credentials warning\n%s", buf.String())
	}
}
