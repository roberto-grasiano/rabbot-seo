package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// TestMaybePrintSleepNudge_BothArms pins criterion 10 (seam stub, both arms): with
// sleepyHostFn stubbed true the nudge prints exactly once; stubbed false it is absent.
func TestMaybePrintSleepNudge_BothArms(t *testing.T) {
	t.Run("sleeper host prints nudge exactly once", func(t *testing.T) {
		prev := sleepyHostFn
		sleepyHostFn = func() bool { return true }
		t.Cleanup(func() { sleepyHostFn = prev })

		var buf bytes.Buffer
		maybePrintSleepNudge(&buf)
		if got := strings.Count(buf.String(), wizard.SleepNudge); got != 1 {
			t.Fatalf("nudge printed %d times, want exactly 1", got)
		}
	})

	t.Run("non-sleeper host is silent", func(t *testing.T) {
		prev := sleepyHostFn
		sleepyHostFn = func() bool { return false }
		t.Cleanup(func() { sleepyHostFn = prev })

		var buf bytes.Buffer
		maybePrintSleepNudge(&buf)
		if strings.Contains(buf.String(), wizard.SleepNudge) {
			t.Fatalf("non-sleeper host must not print the nudge, got %q", buf.String())
		}
	})
}

// TestHeadlessNeverPrintsSleepNudge pins criterion 10's headless arm: the headless
// init path (--site … --start) never prints the nudge, regardless of the stub — the
// nudge is wizard-only, so scripts stay byte-stable.
func TestHeadlessNeverPrintsSleepNudge(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// Force the stub true so a leak would be caught.
	prevSleep := sleepyHostFn
	sleepyHostFn = func() bool { return true }
	t.Cleanup(func() { sleepyHostFn = prevSleep })

	// Stub the run-now seam so no real daemon launches.
	prevStart := startDaemonFn
	startDaemonFn = func(*cobra.Command, BuildInfo) error { return nil }
	t.Cleanup(func() { startDaemonFn = prevStart })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--start",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if strings.Contains(out.String(), wizard.SleepNudge) {
		t.Fatalf("headless path leaked the sleep nudge:\n%s", out.String())
	}
}
