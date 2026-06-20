package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// persistCmd returns a *cobra.Command wired with discard buffers, for the
// persistWizardResult tests (which exercise the post-wizard persistence without a
// real TTY). Output is captured so a test can assert no secret leaks.
func persistCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	return c
}

// TestInitWizardCancelExitsCleanly pins the abort UX: when the wizard reports a
// user cancel (wizard.ErrCancelled), init must exit cleanly (nil error) so the
// caller does not print a failure + usage dump for a normal abort.
func TestInitWizardCancelExitsCleanly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return true }
	t.Cleanup(func() { isTTY = prev })

	prevWiz := launchWizardFn
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		return wizard.ErrCancelled
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wizard cancel should exit cleanly, got error: %v", err)
	}
}

// TestLoadWizardConfigPrefersDiskOverFactory pins the scope-form prefill source:
// on a re-run, the wizard's Defaults must come from the user's loaded config
// (e.g. 5m/12h/50), NOT the factory defaults (10m/24h/100). Before the fix the
// Deps were assembled with config.Defaults(), so a configured user saw factory
// values and clicking through could overwrite their settings.
func TestLoadWizardConfigPrefersDiskOverFactory(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	const custom = `crawler:
  contact_email: "ops@example.com"
defaults:
  min_interval: 5m
  max_interval: 12h
  speed_scale: 50
sites: []
`
	if err := os.WriteFile(cfgPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	got := loadWizardConfig(cfgPath)
	if got.Defaults.MinInterval != "5m" {
		t.Errorf("MinInterval = %q, want loaded 5m (got factory?)", got.Defaults.MinInterval)
	}
	if got.Defaults.MaxInterval != "12h" {
		t.Errorf("MaxInterval = %q, want loaded 12h (got factory?)", got.Defaults.MaxInterval)
	}
	if got.Defaults.SpeedScale != 50 {
		t.Errorf("SpeedScale = %d, want loaded 50 (got factory?)", got.Defaults.SpeedScale)
	}
}

// TestLoadWizardConfigFallsBackToFactory pins the first-run case: with no config
// file on disk, loadWizardConfig returns the factory defaults so the scope form
// still pre-fills with 10m/24h/100.
func TestLoadWizardConfigFallsBackToFactory(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "does-not-exist.yaml")

	got := loadWizardConfig(cfgPath)
	want := config.Defaults()
	if got.Defaults.MinInterval != want.Defaults.MinInterval {
		t.Errorf("MinInterval = %q, want factory %q", got.Defaults.MinInterval, want.Defaults.MinInterval)
	}
	if got.Defaults.SpeedScale != want.Defaults.SpeedScale {
		t.Errorf("SpeedScale = %d, want factory %d", got.Defaults.SpeedScale, want.Defaults.SpeedScale)
	}
}

func TestInitHeadlessSetupWritesConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload() // adrg/xdg caches paths at init; re-read after Setenv

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Crawler.ContactEmail != "ops@example.com" {
		t.Errorf("contact_email = %q", cfg.Crawler.ContactEmail)
	}
	if len(cfg.Sites) != 1 || cfg.Sites[0].URL != "https://example.com" {
		t.Errorf("sites = %+v", cfg.Sites)
	}
	if !strings.Contains(out.String(), "Rabbot-SEO/9.9.9") {
		t.Errorf("expected UA in output, got: %s", out.String())
	}
}

// TestInitHeadlessRejectsBadContactEmail pins that the headless --contact-email
// is validated as an email (reusing Phase 1's config.ValidateEmail via
// setup.Plan.Validate): a non-email value (a bare URL) is rejected with
// ErrContactEmailInvalid, and no config with that value is left behind.
func TestInitHeadlessRejectsBadContactEmail(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "https://example.com/contact", // a URL, not an email
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a validation error for a non-email --contact-email, got nil")
	}
	if !errors.Is(err, setup.ErrContactEmailInvalid) {
		t.Fatalf("error = %v, want %v", err, setup.ErrContactEmailInvalid)
	}

	// The invalid value must not have been written to a config.
	cfgDir, derr := config.ResolveConfigDir()
	if derr != nil {
		t.Fatal(derr)
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		cfg, lerr := config.Load(cfgPath, nil)
		if lerr == nil && cfg.Crawler.ContactEmail == "https://example.com/contact" {
			t.Fatal("the invalid contact email was written to config")
		}
	}
}

func TestInitHeadlessRequiresAttestation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--contact-email", "ops@example.com", "--site", "https://example.com"}) // no --i-am-authorized
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error without --i-am-authorized")
	}
}

// TestInitSetupFlagsWithoutContactOrSiteErrors guards against silent intent
// loss: if a user passes any setup-only flag (e.g. --min-interval/--speed/
// --i-am-authorized/--name/--max-interval) but omits the required
// --contact-email/--site, init must surface the specific validation error rather
// than quietly scaffolding the empty default template and exiting 0.
func TestInitSetupFlagsWithoutContactOrSiteErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr error
	}{
		{
			name:    "min-interval alone",
			args:    []string{"--min-interval", "5m"},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name:    "speed alone",
			args:    []string{"--speed", "50"},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name:    "max-interval alone",
			args:    []string{"--max-interval", "24h"},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name:    "name alone",
			args:    []string{"--name", "My Site"},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name:    "i-am-authorized alone",
			args:    []string{"--i-am-authorized"},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name:    "contact-email present but no site",
			args:    []string{"--contact-email", "ops@example.com", "--i-am-authorized"},
			wantErr: setup.ErrNoSites,
		},
		{
			name:    "site present, authorized, but no contact-email",
			args:    []string{"--site", "https://example.com", "--i-am-authorized"},
			wantErr: setup.ErrContactEmailRequired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			xdg.Reload()

			cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for args %v, got nil (silent scaffold)", tc.args)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}

			// And it must NOT have silently written a config that ignores intent.
			cfgDir, derr := config.ResolveConfigDir()
			if derr != nil {
				t.Fatal(derr)
			}
			cfgPath := config.ConfigFilePath(cfgDir)
			if _, statErr := os.Stat(cfgPath); statErr == nil {
				cfg, lerr := config.Load(cfgPath, nil)
				if lerr == nil && cfg.Crawler.ContactEmail == "" && len(cfg.Sites) == 0 {
					t.Fatalf("setup flags were dropped: wrote empty scaffold instead of erroring")
				}
			}
		})
	}
}

// TestInitConnectClaudeProject pins the headless Connect-Claude wiring: with
// --connect-claude project, init writes ./.mcp.json (in a temp cwd) carrying our
// rabbot entry, advisory and non-blocking on the rest of setup.
func TestInitConnectClaudeProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// Run in a temp cwd so ./.mcp.json lands in an isolated dir.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--connect-claude", "project",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(workdir, ".mcp.json"))
	if err != nil {
		t.Fatalf("expected ./.mcp.json to be written: %v", err)
	}
	if !strings.Contains(string(raw), "\"rabbot\"") || !strings.Contains(string(raw), "\"mcp\"") {
		t.Fatalf(".mcp.json missing our entry: %s", raw)
	}
}

// TestInitConnectClaudePrintWritesNoFile pins the default/advisory behavior: with
// --connect-claude print (the default), init writes NO file but prints the snippet
// so a headless user still gets a copy.
func TestInitConnectClaudePrintWritesNoFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	var errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--connect-claude", "print",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(workdir, ".mcp.json")); statErr == nil {
		t.Fatal("print target must not write ./.mcp.json")
	}
	if !strings.Contains(errOut.String(), "mcpServers") {
		t.Fatalf("print target should emit the snippet to stderr, got: %s", errOut.String())
	}
}

// TestInitConnectOnlyPrintRemoteScaffoldsNothing pins #87-print: `rabbot init
// --connect-claude print --connect-remote you@host` with NO setup flags and no TTY
// must ONLY print the remote MCP snippet — it must NOT scaffold a config.yaml
// (the #87 regression: this fell through to the scaffold branch, wrote config.yaml and
// "wrote ...", and never emitted the snippet). Falsifiable: the pre-fix code wrote
// the config and printed no snippet.
func TestInitConnectOnlyPrintRemoteScaffoldsNothing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prevTTY := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prevTTY })

	var out, errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--connect-claude", "print",
		"--connect-remote", "you@vps",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// No config scaffold was written (print is print-only).
	cfgDir, _ := config.ResolveConfigDir()
	if _, statErr := os.Stat(config.ConfigFilePath(cfgDir)); statErr == nil {
		t.Fatal("print-only connect must NOT scaffold config.yaml")
	}
	// stdout must not announce a config write.
	if strings.Contains(out.String(), "wrote") {
		t.Fatalf("print-only connect must not write a config; stdout=%s", out.String())
	}
	// The remote SSH snippet is printed to stderr (ssh transport + the dest).
	if !strings.Contains(errOut.String(), "mcpServers") || !strings.Contains(errOut.String(), "ssh") {
		t.Fatalf("expected the remote MCP snippet on stderr, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "you@vps") {
		t.Fatalf("remote snippet should carry the SSH dest, got: %s", errOut.String())
	}
}

// TestInitConnectOnlyPrintLocalScaffoldsNothing pins the local arm of #87-print:
// `rabbot init --connect-claude print` (no remote, no setup flags, no TTY) prints
// the LOCAL snippet and writes nothing.
func TestInitConnectOnlyPrintLocalScaffoldsNothing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prevTTY := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prevTTY })

	var out, errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--connect-claude", "print"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, _ := config.ResolveConfigDir()
	if _, statErr := os.Stat(config.ConfigFilePath(cfgDir)); statErr == nil {
		t.Fatal("print-only connect must NOT scaffold config.yaml")
	}
	if strings.Contains(out.String(), "wrote") {
		t.Fatalf("print-only connect must not write a config; stdout=%s", out.String())
	}
	if !strings.Contains(errOut.String(), "mcpServers") {
		t.Fatalf("expected the local MCP snippet on stderr, got: %s", errOut.String())
	}
}

// TestInitConnectClaudeWriteErrorIsAdvisory pins the load-bearing advisory contract
// of the headless Connect-Claude step: a write FAILURE is surfaced to stderr but
// NEVER fails setup (init still exits 0 and the config/site are still written).
// The connect-write seam is forced to error so the test is deterministic (no
// dependence on an unwritable cwd).
func TestInitConnectClaudeWriteErrorIsAdvisory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	prev := connectWriteFn
	connectWriteFn = func(string) (string, error) { return "", errors.New("read-only target") }
	t.Cleanup(func() { connectWriteFn = prev })

	var errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--connect-claude", "claude-desktop",
	})
	// Advisory: a connect-write error must NOT fail setup.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init must stay non-fatal on a connect-write error, got: %v", err)
	}
	// The rest of setup still completed: the config file was written.
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Fatalf("config was not written despite advisory connect-write failure: %v", statErr)
	}
	// The advisory error is surfaced to stderr (not swallowed).
	if !strings.Contains(errOut.String(), "connect-claude: could not write claude-desktop config") {
		t.Fatalf("advisory connect-write error not surfaced to stderr, got: %s", errOut.String())
	}
}

// TestInitConnectClaudeBakesCustomDataDir pins Phase 6 Task 6 for the headless
// path: when the daemon runs under a NON-DEFAULT data_dir, the locally-written
// (and printed) MCP snippet must bake `--data-dir <dir>` so the mcp child Claude
// later spawns is launched with the same data dir the daemon uses. (This baking is
// forward-compat only — the child reads control.token from the CONFIG dir and opens
// no DB, so --data-dir does not govern Hop-2 reachability today; the test pins the
// baking mechanism, not a token/DB coherence claim.) The custom dir arrives via
// RABBOT_DATA_DIR (config precedence: env > file), which config.Load resolves.
func TestInitConnectClaudeBakesCustomDataDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// A non-default data_dir the daemon uses; must be baked into the snippet.
	customDataDir := filepath.Join(root, "custom-data")
	t.Setenv("RABBOT_DATA_DIR", customDataDir)

	// Run in a temp cwd so ./.mcp.json lands in an isolated dir.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	var errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--connect-claude", "project",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// The custom data dir is baked into JSON, which escapes the path separator
	// (\ → \\ on Windows). Assert against the JSON-encoded form so the substring
	// matches on every OS — a Windows path C:\...\custom-data appears as
	// C:\\...\\custom-data in the snippet. encodedDir is the marshaled path with
	// the surrounding quotes stripped, i.e. exactly the substring the JSON carries.
	enc, merr := json.Marshal(customDataDir)
	if merr != nil {
		t.Fatalf("marshal customDataDir: %v", merr)
	}
	encodedDir := strings.Trim(string(enc), `"`)

	// The written ./.mcp.json must bake the custom data dir into the launch args.
	raw, err := os.ReadFile(filepath.Join(workdir, ".mcp.json"))
	if err != nil {
		t.Fatalf("expected ./.mcp.json to be written: %v", err)
	}
	if !strings.Contains(string(raw), "--data-dir") || !strings.Contains(string(raw), encodedDir) {
		t.Fatalf(".mcp.json did not bake the custom data_dir (expected --data-dir %s): %s", customDataDir, raw)
	}
	// The printed snippet (stderr) must also bake it, so a headless user copying
	// it by hand gets a dir-coherent entry.
	if !strings.Contains(errOut.String(), "--data-dir") || !strings.Contains(errOut.String(), encodedDir) {
		t.Fatalf("printed snippet did not bake the custom data_dir (expected --data-dir %s): %s", customDataDir, errOut.String())
	}
}

// TestPersistWizardResultConnectMCP pins the wizard-side wiring: the collected MCP
// target + write decision flow into the writer seam in persistWizardResult WITHOUT
// a TTY (the same no-TTY seam the proof intent uses).
func TestPersistWizardResultConnectMCP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	var gotTarget string
	var called bool
	prev := connectWriteFn
	connectWriteFn = func(target string) (string, error) {
		called = true
		gotTarget = target
		return "/tmp/.mcp.json", nil
	}
	t.Cleanup(func() { connectWriteFn = prev })

	in := wizard.Inputs{
		ContactEmail:  "ops@example.com",
		Authorized:    true,
		AttestedAt:    time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites:         []wizard.SiteDraft{{URL: "https://example.com"}},
		ConnectMCP:    true,
		ConnectTarget: "claude-desktop",
	}
	var errOut strings.Builder
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, &errOut); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}
	if !called {
		t.Fatal("connect writer was not invoked despite ConnectMCP=true")
	}
	if gotTarget != "claude-desktop" {
		t.Fatalf("connect target = %q, want claude-desktop", gotTarget)
	}
	// Finding 3: the wizard path must surface the write outcome (the written path),
	// mirroring the headless emitConnectClaude feedback — not silently drop it.
	if !strings.Contains(errOut.String(), "connect-claude: wrote /tmp/.mcp.json") {
		t.Fatalf("persistWizardResult did not surface the connect-write path; errOut = %q", errOut.String())
	}
}

// TestPersistWizardResultConnectMCPWriteError pins that a connect-write FAILURE is
// surfaced as a non-fatal advisory line to errOut and never fails the wizard
// persistence — the same advisory contract the headless path has.
func TestPersistWizardResultConnectMCPWriteError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	prev := connectWriteFn
	connectWriteFn = func(string) (string, error) { return "", errors.New("read-only cwd") }
	t.Cleanup(func() { connectWriteFn = prev })

	in := wizard.Inputs{
		ContactEmail:  "ops@example.com",
		Authorized:    true,
		AttestedAt:    time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites:         []wizard.SiteDraft{{URL: "https://example.com"}},
		ConnectMCP:    true,
		ConnectTarget: "project",
	}
	var errOut strings.Builder
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, &errOut); err != nil {
		t.Fatalf("persistWizardResult must stay non-fatal on a connect-write error, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "connect-claude: could not write project config") {
		t.Fatalf("persistWizardResult did not surface the advisory write error; errOut = %q", errOut.String())
	}
}

// TestPersistWizardResultConnectMCPSkipped pins that no writer call happens when
// the user declined Connect-Claude.
func TestPersistWizardResultConnectMCPSkipped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	called := false
	prev := connectWriteFn
	connectWriteFn = func(string) (string, error) { called = true; return "", nil }
	t.Cleanup(func() { connectWriteFn = prev })

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites:        []wizard.SiteDraft{{URL: "https://example.com"}},
		ConnectMCP:   false,
	}
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}
	if called {
		t.Fatal("connect writer ran despite ConnectMCP=false")
	}
}

// TestInitHeadlessWritesSlackNotifier pins the alerts step: --slack-webhook
// writes a {slack,slack-webhook,<url>} notifier to config, preserving an ${ENV}
// interpolation token literally. The live send is stubbed via sendTestAlertFn.
//
// #84 UPDATE: this test drives an ${ENV} token with the env var UNSET. The fix
// REQUIRES that the immediate test alert is SKIPPED in that case (an unresolved
// token has nothing valid to POST — POSTing the literal "${...}" string was the
// #84 "unsupported protocol scheme" bug). So we now assert the send seam is
// NOT called and a skip note is printed; the literal token is still written to
// config for the daemon to interpolate at runtime. The resolve arm (env SET → the
// seam receives the interpolated URL) is covered by
// TestApplyAlertsStepInterpolatesEnvTokenForTest.
func TestInitHeadlessWritesSlackNotifier(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// Ensure the token does NOT resolve in this process.
	_ = os.Unsetenv("RABBOT_SLACK_WEBHOOK")

	var called bool
	prev := sendTestAlertFn
	sendTestAlertFn = func(_ context.Context, _ string, _ *http.Client) error {
		called = true
		return nil
	}
	t.Cleanup(func() { sendTestAlertFn = prev })

	var errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--slack-webhook", "${RABBOT_SLACK_WEBHOOK}",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// The notifier URL is interpolated at Load; with the env var unset, koanf
	// leaves the literal token in place. Read the raw config to assert the
	// literal token round-tripped, then Load for the structural shape.
	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${RABBOT_SLACK_WEBHOOK}") {
		t.Fatalf("env-interpolation token not written literally:\n%s", raw)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("want 1 notifier, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	if cfg.Notifiers[0].Name != "slack" || cfg.Notifiers[0].Type != "slack-webhook" {
		t.Fatalf("notifier shape wrong: %+v", cfg.Notifiers[0])
	}
	// #84: an UNRESOLVED env token must NOT trigger the immediate test alert (would
	// POST the literal "${...}" string), and a skip note is surfaced instead.
	if called {
		t.Fatal("test-alert seam was invoked for an unresolved env token (would POST the literal token)")
	}
	if !strings.Contains(errOut.String(), "skipping the immediate test alert") {
		t.Fatalf("expected a skip note for the unresolved env token, got: %s", errOut.String())
	}
}

// TestInitHeadlessSlackWebhookNotEchoed pins the secret guard at the CLI layer:
// a real webhook value passed to --slack-webhook must never appear in stdout or
// stderr (it is never echoed; the success line carries no URL).
func TestInitHeadlessSlackWebhookNotEchoed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error { return nil }
	t.Cleanup(func() { sendTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--slack-webhook", "https://hooks.slack.com/SECRET",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("webhook leaked to stdout:\n%s", out.String())
	}
	if strings.Contains(errOut.String(), "SECRET") {
		t.Fatalf("webhook leaked to stderr:\n%s", errOut.String())
	}
}

// TestInitHeadlessTestAlertFailureNonFatal pins that a test-alert send failure is
// advisory: init still exits 0 and prints a warning that does NOT contain the
// webhook (the seam returns a scrubbed error like the real notifier would).
func TestInitHeadlessTestAlertFailureNonFatal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error {
		return errors.New("slack webhook \"slack-test\": <redacted-webhook-url> 500")
	}
	t.Cleanup(func() { sendTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--slack-webhook", "https://hooks.slack.com/SECRET",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a test-alert failure must be non-fatal, got: %v", err)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(errOut.String(), "SECRET") {
		t.Fatalf("webhook leaked in warning output:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "test alert") {
		t.Fatalf("expected a test-alert warning on stderr, got: %s", errOut.String())
	}
	// The config was still written despite the advisory failure.
	cfgDir, _ := config.ResolveConfigDir()
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("notifier not persisted despite advisory send failure: %+v", cfg.Notifiers)
	}
}

// TestApplyAlertsStepBoundsTestAlertContext pins that the advisory test-alert is
// sent under a BOUNDED context, so a pathological 429 backoff loop can never stall
// onboarding for minutes (spec: the test-alert is best-effort and must not block
// setup). The send seam receives a context with a deadline no further out than the
// 30s bound; the ambient cmd.Context() (effectively context.Background()) has none,
// so without the wrap this fails.
func TestApplyAlertsStepBoundsTestAlertContext(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	var gotDeadline time.Time
	var hadDeadline bool
	prev := sendTestAlertFn
	sendTestAlertFn = func(ctx context.Context, _ string, _ *http.Client) error {
		gotDeadline, hadDeadline = ctx.Deadline()
		return nil
	}
	t.Cleanup(func() { sendTestAlertFn = prev })

	if err := applyAlertsStep(persistCmd(), cfgPath, "https://hooks.slack.com/SECRET"); err != nil {
		t.Fatalf("applyAlertsStep: %v", err)
	}
	if !hadDeadline {
		t.Fatal("test-alert context had no deadline: a 429 backoff loop could stall setup")
	}
	if until := time.Until(gotDeadline); until <= 0 || until > 30*time.Second {
		t.Fatalf("test-alert deadline = %s out; want a bound in (0, 30s]", until)
	}
}

// TestApplyAlertsStepInterpolatesEnvTokenForTest pins #84 (resolve arm): when the
// --slack-webhook is an ${ENV} token AND the env var is SET, the immediate test
// alert must POST the INTERPOLATED URL (the value the daemon resolves at runtime),
// not the literal "${...}" token — which previously failed with "unsupported
// protocol scheme". Falsifiable: the pre-fix code passed `webhook` (the literal
// token) to the send seam.
func TestApplyAlertsStepInterpolatesEnvTokenForTest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	const resolved = "https://hooks.slack.com/services/RESOLVED"
	t.Setenv("RABBOT_TEST_HOOK_84", resolved)

	var got string
	var called bool
	prev := sendTestAlertFn
	sendTestAlertFn = func(_ context.Context, webhook string, _ *http.Client) error {
		called = true
		got = webhook
		return nil
	}
	t.Cleanup(func() { sendTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := persistCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := applyAlertsStep(cmd, cfgPath, "${RABBOT_TEST_HOOK_84}"); err != nil {
		t.Fatalf("applyAlertsStep: %v", err)
	}
	if !called {
		t.Fatal("test-alert seam was not invoked despite a resolvable env-token webhook")
	}
	if got != resolved {
		t.Fatalf("test-alert webhook = %q, want the INTERPOLATED %q (not the literal token)", got, resolved)
	}
	// SECRET-SAFETY: the resolved secret must not be echoed.
	if strings.Contains(out.String(), "RESOLVED") || strings.Contains(errOut.String(), "RESOLVED") {
		t.Fatalf("resolved webhook leaked:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
	// The config still carries the LITERAL token (the daemon interpolates at runtime).
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${RABBOT_TEST_HOOK_84}") {
		t.Fatalf("config must keep the literal env token for runtime interpolation:\n%s", raw)
	}
}

// TestApplyAlertsStepSkipsUnresolvedEnvToken pins #84 (skip arm): when the
// --slack-webhook is an ${ENV} token and the env var is UNSET (the secret is
// supplied to the service at runtime), the immediate test alert is SKIPPED with a
// clear note — it must NOT POST the literal "${...}" token. Falsifiable: the
// pre-fix code always called the send seam.
func TestApplyAlertsStepSkipsUnresolvedEnvToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	// Ensure the var is unset for this process.
	t.Setenv("RABBOT_UNSET_HOOK_84", "")
	_ = os.Unsetenv("RABBOT_UNSET_HOOK_84")

	var called bool
	prev := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error {
		called = true
		return nil
	}
	t.Cleanup(func() { sendTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := persistCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := applyAlertsStep(cmd, cfgPath, "${RABBOT_UNSET_HOOK_84}"); err != nil {
		t.Fatalf("applyAlertsStep: %v", err)
	}
	if called {
		t.Fatal("test-alert seam was invoked for an UNRESOLVED env-token webhook (would POST the literal token)")
	}
	// A clear, actionable note is printed to stderr (and echoes only the un-resolved
	// token form, never a secret).
	if !strings.Contains(errOut.String(), "skipping the immediate test alert") {
		t.Fatalf("expected a skip note on stderr, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "${RABBOT_UNSET_HOOK_84}") {
		t.Fatalf("skip note should name the env token, got: %s", errOut.String())
	}
	// The notifier is still written so the daemon can resolve it at runtime.
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("notifier not persisted: %+v", cfg.Notifiers)
	}
}

// TestInitHeadlessStartFlag pins step 10: --start invokes the startDaemonFn seam
// exactly once after a successful Apply.
func TestInitHeadlessStartFlag(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	var calls int
	prev := startDaemonFn
	startDaemonFn = func(*cobra.Command, BuildInfo) error { calls++; return nil }
	t.Cleanup(func() { startDaemonFn = prev })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
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
	if calls != 1 {
		t.Fatalf("startDaemonFn called %d times, want 1", calls)
	}
}

// TestInitHeadlessInstallServiceFlag pins step 10: --install-service invokes the
// installServiceFn seam exactly once.
func TestInitHeadlessInstallServiceFlag(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	var calls int
	prev := installServiceFn
	installServiceFn = func(*cobra.Command, BuildInfo) error { calls++; return nil }
	t.Cleanup(func() { installServiceFn = prev })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--install-service",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if calls != 1 {
		t.Fatalf("installServiceFn called %d times, want 1", calls)
	}
}

// TestInitInstallServiceStatesElevation guards the "never silently escalate"
// constraint: --install-service must print a clear elevation notice BEFORE the
// install seam runs.
func TestInitInstallServiceStatesElevation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	var noticeSeenBeforeInstall bool
	var out bytes.Buffer
	prev := installServiceFn
	installServiceFn = func(cmd *cobra.Command, _ BuildInfo) error {
		// The elevation notice must already be on stdout when install runs.
		noticeSeenBeforeInstall = strings.Contains(out.String(), "may require") &&
			(strings.Contains(out.String(), "privileg") ||
				strings.Contains(out.String(), "sudo") ||
				strings.Contains(out.String(), "Administrator"))
		return nil
	}
	t.Cleanup(func() { installServiceFn = prev })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--install-service",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !noticeSeenBeforeInstall {
		t.Fatalf("elevation notice not printed before install; stdout was:\n%s", out.String())
	}
}

// TestInitHeadlessStartFailureNonFatalVsFatal pins the spec error-handling rule:
// a --start failure surfaces the OS error but does NOT roll back the written
// config (the config remains on disk and complete).
func TestInitHeadlessStartFailureNonFatalVsFatal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := startDaemonFn
	startDaemonFn = func(*cobra.Command, BuildInfo) error { return errors.New("could not start daemon") }
	t.Cleanup(func() { startDaemonFn = prev })

	var errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--start",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a --start failure must not fail setup, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "could not start daemon") {
		t.Fatalf("start failure not surfaced on stderr: %s", errOut.String())
	}
	// Config still on disk and complete (not rolled back).
	cfgDir, _ := config.ResolveConfigDir()
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatalf("config rolled back / unloadable after --start failure: %v", err)
	}
	if cfg.Crawler.ContactEmail != "ops@example.com" || len(cfg.Sites) != 1 {
		t.Fatalf("config not intact after --start failure: %+v", cfg)
	}
}

// TestInitHeadlessPrintsSummary pins step 11 for the headless path: init prints
// the shared summary block — config path, data path, the site URL with a state
// word, and the next-commands block (delegating to setup.RenderSummary).
func TestInitHeadlessPrintsSummary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	var out bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)
	got := out.String()
	for _, want := range []string{
		cfgPath,
		"https://example.com",
		"throttled", // no verification block headless => throttled state word
		"rabbot status",
		"rabbot sites list",
		"rabbot history",
		"rabbot verify",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

// TestInitSummaryNoWebhook pins that with a configured webhook (send stubbed),
// the FULL stdout+stderr never contains the secret — the summary surfaces only
// "Slack alerts: configured".
func TestInitSummaryNoWebhook(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error { return nil }
	t.Cleanup(func() { sendTestAlertFn = prev })

	var out, errOut bytes.Buffer
	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--slack-webhook", "https://hooks.slack.com/SECRET",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "SECRET") {
		t.Fatalf("webhook leaked into output:\n%s", combined)
	}
	if !strings.Contains(out.String(), "Slack alerts: configured") {
		t.Fatalf("summary should report Slack configured, got:\n%s", out.String())
	}
}

// TestInitHeadlessReRunSlackSummaryReflectsConfig pins Finding 3: on a re-run
// that omits --slack-webhook (e.g. `rabbot init --add-site --site B ...`)
// the headless summary must still report "Slack alerts: configured" when Slack
// IS already written in the config from a prior run. This requires reading the
// resulting config state from disk rather than keying off the current
// invocation's --slack-webhook flag.
func TestInitHeadlessReRunSlackSummaryReflectsConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error { return nil }
	t.Cleanup(func() { sendTestAlertFn = prev })

	// First run: configure site A + Slack.
	run1 := newInitCmd(BuildInfo{Version: "9.9.9"})
	run1.SetOut(&bytes.Buffer{})
	run1.SetErr(&bytes.Buffer{})
	run1.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--slack-webhook", "https://hooks.slack.com/SECRET",
	})
	if err := run1.Execute(); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// Second run: add a new site WITHOUT --slack-webhook.
	var out2 bytes.Buffer
	run2 := newInitCmd(BuildInfo{Version: "9.9.9"})
	run2.SetOut(&out2)
	run2.SetErr(&bytes.Buffer{})
	run2.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://other.example",
		"--i-am-authorized",
		"--add-site",
	})
	if err := run2.Execute(); err != nil {
		t.Fatalf("second init (re-run without --slack-webhook): %v", err)
	}

	// The summary must still report Slack configured (it IS in the config from run 1).
	if !strings.Contains(out2.String(), "Slack alerts: configured") {
		t.Fatalf("re-run summary reports Slack as not configured despite existing notifier;\noutput:\n%s", out2.String())
	}
}

// TestPersistWizardResultSummary pins step 11 for the wizard runner: after
// persistWizardResult, the same summary block is produced from the assembled
// config (reuses the shared renderer). The runner writes the summary to the
// wizard's Out writer, so we drive it through launchWizard's persistence by
// asserting the summary is rendered from the just-written config.
func TestPersistWizardResultSummary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites: []wizard.SiteDraft{{
			URL:        "https://example.com",
			Method:     verify.MethodWellKnown,
			Token:      "rab_TOKEN",
			Verified:   true,
			VerifiedAt: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		}},
	}
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}

	// The summary is rendered from the loaded config; re-derive it the same way the
	// runner does and assert the verified state surfaces (intent => verified).
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	s := setup.SummaryFromConfig(cfg, cfgPath, "/data", false, "")
	if err := setup.RenderSummary(&b, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	if !strings.Contains(b.String(), "verified") {
		t.Fatalf("wizard summary should mark the verified site:\n%s", b.String())
	}
}

// TestFullSpeedHint pins the Task-14 plain-language "go full speed" hint that the
// wizard runner prints after the summary: it is shown ONLY when the just-written
// config still holds an UNVERIFIED site (the common case, since the verify upgrade
// is optional), names that site, and points at `rabbot verify <site>` — never
// at a removed onboarding step. The verified state is read from CONFIG (VerifiedAt)
// so a menu-verify (which writes the config block) is honored and not nagged. A
// fully-verified or empty config yields no hint, as does a load failure.
func TestFullSpeedHint(t *testing.T) {
	writeCfg := func(t *testing.T, yaml string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return p
	}

	t.Run("unverified site gets a verify hint", func(t *testing.T) {
		p := writeCfg(t, "sites:\n  - url: https://yoursite.com\n")
		got := fullSpeedHint(p)
		if got == "" {
			t.Fatal("unverified site should produce a go-full-speed hint")
		}
		for _, want := range []string{"https://yoursite.com", "rabbot verify", "full speed"} {
			if !strings.Contains(got, want) {
				t.Errorf("hint %q missing %q", got, want)
			}
		}
	})

	t.Run("verified site gets no hint", func(t *testing.T) {
		p := writeCfg(t, "sites:\n  - url: https://yoursite.com\n    verification:\n      method: meta\n      token: rab_TOK\n      verified_at: 2026-06-05T12:00:00Z\n")
		if got := fullSpeedHint(p); got != "" {
			t.Fatalf("verified site should not be nudged to verify, got %q", got)
		}
	})

	t.Run("no sites yields no hint", func(t *testing.T) {
		p := writeCfg(t, "sites: []\n")
		if got := fullSpeedHint(p); got != "" {
			t.Fatalf("empty config should produce no hint, got %q", got)
		}
	})

	t.Run("unreadable config degrades to empty", func(t *testing.T) {
		if got := fullSpeedHint(filepath.Join(t.TempDir(), "does-not-exist.yaml")); got != "" {
			t.Fatalf("a load failure must degrade to no hint, got %q", got)
		}
	})
}

// TestInitReRunAddSiteIdempotent pins the headless append + idempotency: run init
// once (site A), then --add-site a second site B, then re-run the exact same B
// invocation a third time — the config must end with exactly A+B (no dup), relying
// on setup.Apply's existing dedup/SitesSkipped.
func TestInitReRunAddSiteIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	runInit := func(args ...string) {
		t.Helper()
		cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("init %v: %v", args, err)
		}
	}

	runInit("--contact-email", "ops@example.com", "--site", "https://a.example", "--i-am-authorized")
	runInit("--add-site", "--contact-email", "ops@example.com", "--site", "https://b.example", "--i-am-authorized")
	// Re-run the same B invocation: dedup must keep it at A+B.
	runInit("--add-site", "--contact-email", "ops@example.com", "--site", "https://b.example", "--i-am-authorized")

	cfgDir, _ := config.ResolveConfigDir()
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 2 {
		t.Fatalf("want exactly 2 sites (A+B, deduped), got %d: %+v", len(cfg.Sites), cfg.Sites)
	}
	urls := map[string]bool{}
	for _, s := range cfg.Sites {
		urls[s.URL] = true
	}
	if !urls["https://a.example"] || !urls["https://b.example"] {
		t.Fatalf("sites missing after re-run: %+v", cfg.Sites)
	}
}

// TestInitExistingNoForceErrors pins that with an existing config and NO
// --force/--add-site and NO TTY, the Phase-1 'config already exists' error still
// fires (backward compatibility).
func TestInitExistingNoForceErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prev })

	// Seed an existing config via a first scaffold.
	first := newInitCmd(BuildInfo{Version: "9.9.9"})
	first.SetOut(&bytes.Buffer{})
	first.SetArgs([]string{})
	if err := first.Execute(); err != nil {
		t.Fatalf("seed scaffold: %v", err)
	}

	second := newInitCmd(BuildInfo{Version: "9.9.9"})
	second.SetOut(&bytes.Buffer{})
	second.SetErr(&bytes.Buffer{})
	second.SetArgs([]string{})
	err := second.Execute()
	if err == nil {
		t.Fatal("expected 'config already exists' error on a re-run with no TTY and no --force/--add-site")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want a 'config already exists' message", err)
	}
}

// TestInitAddSiteAloneRoutesHeadless pins the contract that --add-site is meaningful
// purely by being one of setupFlags: a bare `init --add-site` against an EXISTING
// config (which would otherwise trip the no-TTY 'config already exists' scaffold
// guard) must route to the HEADLESS append path instead. We prove the routing by the
// error it surfaces — the setup validation error (contact URL required, since none
// was given) — NOT the scaffold 'config already exists' error. This is the only
// behavior unique to --add-site (with --site/--contact-email, those flags trigger
// headless on their own), and it locks in that --add-site stays in setupFlags even
// though it is registered without a bound variable (it is read only via Changed).
func TestInitAddSiteAloneRoutesHeadless(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prev })

	// Seed an existing config via a bare scaffold — the exact state in which
	// TestInitExistingNoForceErrors expects the 'config already exists' guard to fire.
	seed := newInitCmd(BuildInfo{Version: "9.9.9"})
	seed.SetOut(&bytes.Buffer{})
	seed.SetArgs([]string{})
	if err := seed.Execute(); err != nil {
		t.Fatalf("seed scaffold: %v", err)
	}

	// `init --add-site` alone must NOT hit the scaffold guard; it must route headless
	// and fail validation (no contact URL / no site), proving the flag flips the mode.
	add := newInitCmd(BuildInfo{Version: "9.9.9"})
	add.SetOut(&bytes.Buffer{})
	add.SetErr(&bytes.Buffer{})
	add.SetArgs([]string{"--add-site"})
	err := add.Execute()
	if err == nil {
		t.Fatal("expected a setup validation error from the headless path, got nil")
	}
	if strings.Contains(err.Error(), "already exists") {
		t.Fatalf("--add-site fell through to the scaffold 'config already exists' guard; want headless routing: %v", err)
	}
	if !errors.Is(err, setup.ErrContactEmailRequired) {
		t.Fatalf("error = %v, want the headless setup validation error %v", err, setup.ErrContactEmailRequired)
	}
}

// TestInitExistingConfigOffersChoice pins the TTY sub-flow routing: with a TTY and
// an EXISTING config and a bare interactive `init`, the Add/Reconfigure/Cancel
// selection seam (existingActionFn) is invoked BEFORE the fresh wizard.
func TestInitExistingConfigOffersChoice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// Seed an existing config (scaffold).
	prevTTY := isTTY
	isTTY = func() bool { return false }
	seed := newInitCmd(BuildInfo{Version: "9.9.9"})
	seed.SetOut(&bytes.Buffer{})
	seed.SetArgs([]string{})
	if err := seed.Execute(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now flip to a TTY and a bare interactive init.
	isTTY = func() bool { return true }
	t.Cleanup(func() { isTTY = prevTTY })

	var existingCalled bool
	prevExisting := existingActionFn
	existingActionFn = func(*cobra.Command) (wizard.ExistingAction, error) {
		existingCalled = true
		return wizard.ActionCancel, nil // quiet exit
	}
	t.Cleanup(func() { existingActionFn = prevExisting })

	var wizardCalled bool
	prevWiz := launchWizardFn
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		wizardCalled = true
		return nil
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !existingCalled {
		t.Fatal("existing-config selection seam was not invoked despite an existing config on a TTY")
	}
	if wizardCalled {
		t.Fatal("fresh wizard ran on Cancel; expected a quiet exit instead")
	}
}

// TestInitExistingConfigReconfigureRoutesToWizard pins that the Reconfigure choice
// routes to the fresh wizard (launchWizardFn).
func TestInitExistingConfigReconfigureRoutesToWizard(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prevTTY := isTTY
	isTTY = func() bool { return false }
	seed := newInitCmd(BuildInfo{Version: "9.9.9"})
	seed.SetOut(&bytes.Buffer{})
	seed.SetArgs([]string{})
	if err := seed.Execute(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	isTTY = func() bool { return true }
	t.Cleanup(func() { isTTY = prevTTY })

	prevExisting := existingActionFn
	existingActionFn = func(*cobra.Command) (wizard.ExistingAction, error) {
		return wizard.ActionReconfigure, nil
	}
	t.Cleanup(func() { existingActionFn = prevExisting })

	var wizardCalled bool
	prevWiz := launchWizardFn
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		wizardCalled = true
		return nil
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !wizardCalled {
		t.Fatal("Reconfigure should route to the fresh wizard")
	}
}

// TestPersistWizardResultAlertsAndRun pins the wizard-runner side of steps 8+10:
// when in.SlackWebhook is set, persistWizardResult writes the notifier and fires
// the sendTestAlertFn seam (advisory); when StartDaemon/InstallService are set,
// the startDaemonFn/installServiceFn seams fire — reusing the same seams the
// headless path uses.
func TestPersistWizardResultAlertsAndRun(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	var sentWebhook string
	var sendCalled, startCalled, installCalled bool

	// #84: the wizard path runs the same applyAlertsStep as headless. Set the env
	// var so the ${ENV} token RESOLVES and the immediate test alert exercises the
	// interpolated URL (the value the daemon will use) — not the literal token, which
	// previously POSTed a "${...}" string and failed. (The unset-token skip arm is
	// pinned by TestApplyAlertsStepSkipsUnresolvedEnvToken / TestInitHeadlessWritesSlackNotifier.)
	const resolved = "https://hooks.slack.com/services/WIZARD"
	t.Setenv("RABBOT_SLACK_WEBHOOK", resolved)

	prevSend := sendTestAlertFn
	sendTestAlertFn = func(_ context.Context, webhook string, _ *http.Client) error {
		sendCalled = true
		sentWebhook = webhook
		return nil
	}
	t.Cleanup(func() { sendTestAlertFn = prevSend })

	prevStart := startDaemonFn
	startDaemonFn = func(*cobra.Command, BuildInfo) error { startCalled = true; return nil }
	t.Cleanup(func() { startDaemonFn = prevStart })

	prevInstall := installServiceFn
	installServiceFn = func(*cobra.Command, BuildInfo) error { installCalled = true; return nil }
	t.Cleanup(func() { installServiceFn = prevInstall })

	in := wizard.Inputs{
		ContactEmail:   "ops@example.com",
		Authorized:     true,
		AttestedAt:     time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites:          []wizard.SiteDraft{{URL: "https://example.com"}},
		SlackWebhook:   "${RABBOT_SLACK_WEBHOOK}",
		StartDaemon:    true,
		InstallService: true,
	}
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}

	if !sendCalled {
		t.Fatal("sendTestAlertFn not invoked despite a configured (resolvable) webhook")
	}
	if sentWebhook != resolved {
		t.Fatalf("test-alert webhook = %q, want the INTERPOLATED %q (not the literal token)", sentWebhook, resolved)
	}
	if !startCalled {
		t.Fatal("startDaemonFn not invoked despite StartDaemon=true")
	}
	if !installCalled {
		t.Fatal("installServiceFn not invoked despite InstallService=true")
	}

	// The notifier was persisted (literal ${ENV} on disk).
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${RABBOT_SLACK_WEBHOOK}") {
		t.Fatalf("notifier not persisted with literal env token:\n%s", raw)
	}
}

// TestPersistWizardResultNoAlertsNoRun pins that with none of the alerts/run
// fields set, none of the step-8/10 seams fire.
func TestPersistWizardResultNoAlertsNoRun(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)

	var anyCalled bool
	prevSend := sendTestAlertFn
	sendTestAlertFn = func(context.Context, string, *http.Client) error { anyCalled = true; return nil }
	t.Cleanup(func() { sendTestAlertFn = prevSend })
	prevStart := startDaemonFn
	startDaemonFn = func(*cobra.Command, BuildInfo) error { anyCalled = true; return nil }
	t.Cleanup(func() { startDaemonFn = prevStart })
	prevInstall := installServiceFn
	installServiceFn = func(*cobra.Command, BuildInfo) error { anyCalled = true; return nil }
	t.Cleanup(func() { installServiceFn = prevInstall })

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites:        []wizard.SiteDraft{{URL: "https://example.com"}},
	}
	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}
	if anyCalled {
		t.Fatal("a step-8/10 seam fired despite no alerts/run intent")
	}
}

func TestInitNoFlagsScaffolds(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("scaffold init: %v", err)
	}
	cfgDir, _ := config.ResolveConfigDir()
	cfgPath := config.ConfigFilePath(cfgDir)
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The scaffold ships an EMPTY contact_email the operator must fill — empty so
	// config-validate fails loudly (matching the old contact_url behavior), NOT the
	// old contact_url key (renamed in this phase).
	if cfg.Crawler.ContactEmail != "" {
		t.Errorf("scaffold contact_email = %q, want empty so config-validate fails loudly", cfg.Crawler.ContactEmail)
	}
	raw, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(raw), "contact_email:") {
		t.Errorf("scaffold missing contact_email key:\n%s", raw)
	}
	if strings.Contains(string(raw), "contact_url") {
		t.Errorf("scaffold still names the old contact_url key:\n%s", raw)
	}
}

// TestInitNoTTYNoFlagsScaffolds pins the routing decision: with no setup flags
// and NO TTY, init must fall back to the scaffold (NOT the wizard). It overrides
// the isTTY seam to false and asserts the scaffold's empty contact_email.
func TestInitNoTTYNoFlagsScaffolds(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return false }
	t.Cleanup(func() { isTTY = prev })

	prevWiz := launchWizardFn
	wizardRan := false
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		wizardRan = true
		return nil
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("scaffold init: %v", err)
	}
	if wizardRan {
		t.Fatal("wizard ran without a TTY; expected the scaffold path")
	}
	cfgDir, _ := config.ResolveConfigDir()
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Crawler.ContactEmail != "" {
		t.Errorf("scaffold contact_email = %q, want empty so config-validate fails loudly", cfg.Crawler.ContactEmail)
	}
}

// TestInitTTYNoFlagsRoutesToWizard pins the routing decision: with no setup
// flags and a TTY, init must launch the wizard (NOT scaffold). It stubs both
// seams and asserts the wizard ran and no scaffold config was written.
func TestInitTTYNoFlagsRoutesToWizard(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return true }
	t.Cleanup(func() { isTTY = prev })

	prevWiz := launchWizardFn
	wizardRan := false
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		wizardRan = true
		return nil
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !wizardRan {
		t.Fatal("expected the wizard to be launched on a TTY, but it was not")
	}
	// The stub wizard wrote nothing; the scaffold must NOT have run either.
	cfgDir, _ := config.ResolveConfigDir()
	if _, statErr := os.Stat(config.ConfigFilePath(cfgDir)); statErr == nil {
		t.Fatal("scaffold config was written; expected the wizard path to own config writing")
	}
}

// TestInitFlagsAlwaysHeadlessEvenOnTTY pins flag precedence: even on a TTY, any
// setup flag forces the headless path (flags win over TTY); the wizard must NOT
// launch and the config must be written from the flags.
func TestInitFlagsAlwaysHeadlessEvenOnTTY(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	prev := isTTY
	isTTY = func() bool { return true }
	t.Cleanup(func() { isTTY = prev })

	prevWiz := launchWizardFn
	wizardRan := false
	launchWizardFn = func(*cobra.Command, BuildInfo, string) error {
		wizardRan = true
		return nil
	}
	t.Cleanup(func() { launchWizardFn = prevWiz })

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("headless init: %v", err)
	}
	if wizardRan {
		t.Fatal("wizard launched despite setup flags; flags must force the headless path")
	}
	cfgDir, _ := config.ResolveConfigDir()
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Crawler.ContactEmail != "ops@example.com" {
		t.Errorf("headless contact_email = %q", cfg.Crawler.ContactEmail)
	}
}

// TestPersistWizardResultWritesVerificationBlock pins the happy path through the
// post-wizard persistence: a verified site must get its proof-of-control block
// actually written to config (so the daemon's verification-aware throttle, spec
// §E, sees the intent), with no error. This exercises the new found-must-be-true
// guard on its success side.
func TestPersistWizardResultWritesVerificationBlock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites: []wizard.SiteDraft{{
			URL:        "https://example.com",
			Name:       "Example",
			Method:     verify.MethodWellKnown,
			Token:      "rab_TOKEN",
			Verified:   true,
			VerifiedAt: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		}},
	}

	res, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard)
	if err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}
	if res.ConfigPath != cfgPath {
		t.Errorf("ConfigPath = %q, want %q", res.ConfigPath, cfgPath)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(cfg.Sites))
	}
	got := cfg.Sites[0].Verification
	if got.Method != "well_known" || got.Token != "rab_TOKEN" || got.VerifiedAt != "2026-06-05T12:00:00Z" {
		t.Fatalf("verification block not written as expected: %+v", got)
	}
}

// TestPersistWizardResultMissingTargetErrors is the regression test for the fix:
// when the verification writer reports found=false (the target site is absent),
// the just-earned proof intent must NOT be silently dropped — persistWizardResult
// must surface a not-found error rather than returning success and leaving the
// site throttled (spec §E). We drive the found=false path through the writer seam
// because setup.Apply otherwise always writes the sites we then look up.
func TestPersistWizardResultMissingTargetErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	// Force the silent-drop shape: writer reports found=false with a nil error.
	prev := setSiteVerificationFn
	setSiteVerificationFn = func(string, string, config.VerificationConfig) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { setSiteVerificationFn = prev })

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites: []wizard.SiteDraft{{
			URL:        "https://example.com",
			Method:     verify.MethodWellKnown,
			Token:      "rab_TOKEN",
			Verified:   true,
			VerifiedAt: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		}},
	}

	_, err = persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard)
	if err == nil {
		t.Fatal("persistWizardResult returned nil error when the verification target site was not found; the proof intent was silently dropped")
	}
	if !strings.Contains(err.Error(), "not found in config") {
		t.Fatalf("error = %q, want a 'not found in config' not-found error", err)
	}
}

// TestPersistWizardResultWritesThrottledIntent pins the common first-run case:
// the proof screen mints a token and runs verify immediately, before the operator
// can place it, so verify returns StateThrottled (Verified=false). The token+
// method the operator was just shown placement instructions for must STILL be
// recorded in config (Method+Token, no VerifiedAt), mirroring the CLI verify twin
// (internal/cli/verify.go writes the intent before the check runs). Otherwise a
// later `rabbot verify` would find no recorded token and mint a fresh one,
// orphaning the proof the operator already placed.
func TestPersistWizardResultWritesThrottledIntent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ConfigFilePath(cfgDir)

	in := wizard.Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		AttestedAt:   time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
		Sites: []wizard.SiteDraft{{
			URL:    "https://example.com",
			Name:   "Example",
			Method: verify.MethodWellKnown,
			Token:  "rab_THROTTLED",
			// Throttled: the proof was minted but not yet verified.
			Verified: false,
		}},
	}

	if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "9.9.9"}, io.Discard); err != nil {
		t.Fatalf("persistWizardResult: %v", err)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(cfg.Sites))
	}
	got := cfg.Sites[0].Verification
	if got.Method != "well_known" || got.Token != "rab_THROTTLED" {
		t.Fatalf("throttled verification intent not written: %+v", got)
	}
	if got.VerifiedAt != "" {
		t.Errorf("throttled site must NOT record VerifiedAt, got %q", got.VerifiedAt)
	}
}

func capPtr(n int) *int { return &n }

// TestPersistWizardResultWritesCap covers the Spec B cap write: a wizard SiteDraft with
// MaxPages set is written to the per-site discovery cap post-Apply (via
// SetSiteMaxPagesYAML) and round-trips through ResolveDiscovery (0 → unlimited; N → N).
// A nil MaxPages leaves the resolved default (2000) in place.
func TestPersistWizardResultWritesCap(t *testing.T) {
	cases := []struct {
		name    string
		maxPtr  *int
		wantCap int // resolved MaxPages after write (0 = unlimited)
	}{
		{"monitor all writes 0", capPtr(0), 0},
		{"set 500 writes 500", capPtr(500), 500},
		{"keep default writes nothing", nil, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			xdg.Reload()

			cfgDir, err := config.ResolveConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			cfgPath := config.ConfigFilePath(cfgDir)

			in := wizard.Inputs{
				ContactEmail: "ops@me.example",
				Authorized:   true,
				AttestedAt:   time.Now().UTC(),
				Sites:        []wizard.SiteDraft{{URL: "https://example.com", MaxPages: tc.maxPtr}},
			}
			if _, err := persistWizardResult(persistCmd(), in, cfgPath, BuildInfo{Version: "test"}, io.Discard); err != nil {
				t.Fatalf("persistWizardResult: %v", err)
			}
			cfg, err := config.Load(cfgPath, nil)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			var sc config.SiteConfig
			for _, s := range cfg.Sites {
				if s.URL == "https://example.com" {
					sc = s
				}
			}
			if got := cfg.ResolveDiscovery(sc).MaxPages; got != tc.wantCap {
				t.Errorf("resolved MaxPages = %d, want %d", got, tc.wantCap)
			}
		})
	}
}

// TestFirstSiteURL pins the tiny go-live helper: it returns the first collected
// site's URL, and is empty-guarded so a malformed/empty collection never panics
// (the go-live wiring then degrades to an empty host rather than crashing init).
func TestFirstSiteURL(t *testing.T) {
	if got := firstSiteURL(wizard.Inputs{
		Sites: []wizard.SiteDraft{{URL: "https://yoursite.com"}, {URL: "https://other.example"}},
	}); got != "https://yoursite.com" {
		t.Fatalf("firstSiteURL = %q, want the first site URL", got)
	}
	if got := firstSiteURL(wizard.Inputs{}); got != "" {
		t.Fatalf("firstSiteURL on an empty collection = %q, want empty (guarded)", got)
	}
}

func TestInitHeadlessMaxPagesUnlimited(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--max-pages", "0",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 1 {
		t.Fatalf("sites = %+v", cfg.Sites)
	}
	// 0 = unlimited: ResolveDiscovery returns 0 (no cap).
	if mp := cfg.ResolveDiscovery(cfg.Sites[0]).MaxPages; mp != 0 {
		t.Errorf("MaxPages = %d, want 0 (unlimited)", mp)
	}
}

func TestInitHeadlessMaxPagesCapsAtN(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--max-pages", "500",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mp := cfg.ResolveDiscovery(cfg.Sites[0]).MaxPages; mp != 500 {
		t.Errorf("MaxPages = %d, want 500", mp)
	}
}

func TestInitHeadlessMaxPagesUnsetLeavesDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	// No --max-pages: the resolved default (2000) stands; no discovery block written.
	if cfg.Sites[0].Discovery.MaxPagesPerSite != nil {
		t.Errorf("MaxPagesPerSite = %v, want nil (no write)", *cfg.Sites[0].Discovery.MaxPagesPerSite)
	}
	if mp := cfg.ResolveDiscovery(cfg.Sites[0]).MaxPages; mp != 2000 {
		t.Errorf("MaxPages = %d, want 2000 (default)", mp)
	}
}

func TestInitHeadlessMaxPagesNegativeRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--max-pages", "-5",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for --max-pages -5")
	}
}

// TestInitHeadlessMaxPagesCoverageLineEmitted pins that with a cap set, the
// per-site coverage line surfaces (the emit fires once per added site) and the
// on-disk cap is the value we asked for. The site has no reachable sitemap (no
// network), so the page count is unknown and the line prints the "unknown"
// guidance — what matters here is that the emit fires and the cap round-trips.
func TestInitHeadlessMaxPagesCoverageLineEmitted(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	cmd := newInitCmd(BuildInfo{Version: "9.9.9"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--max-pages", "0",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("coverage line missing site URL; output:\n%s", got)
	}
	if !strings.Contains(got, "Coverage:") {
		t.Errorf("expected a Coverage line; output:\n%s", got)
	}
	// And the on-disk cap is the unlimited 0 we asked for.
	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.ConfigFilePath(cfgDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mp := cfg.ResolveDiscovery(cfg.Sites[0]).MaxPages; mp != 0 {
		t.Errorf("MaxPages = %d, want 0", mp)
	}
}

// TestInitHeadlessMaxPagesSkippedSiteNotice pins finding #7: when --max-pages is
// given but a requested --site is deduped as already-present (so it lands in
// SitesSkipped, not SitesAdded), the cap is NOT applied to it — the headless path
// must emit a one-line stderr notice naming the skipped URL so the dropped cap
// intent is not silent. A first run adds the site (no notice); the second run
// re-adds the SAME site with a cap and must surface the notice.
func TestInitHeadlessMaxPagesSkippedSiteNotice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	// First run: add the site (it is freshly added, so no skip notice).
	first := newInitCmd(BuildInfo{Version: "9.9.9"})
	var firstErr bytes.Buffer
	first.SetOut(&bytes.Buffer{})
	first.SetErr(&firstErr)
	first.SetArgs([]string{
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
	})
	if err := first.Execute(); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if strings.Contains(firstErr.String(), "--max-pages not applied") {
		t.Fatalf("first (fresh-add) run should not warn about a skipped cap:\n%s", firstErr.String())
	}

	// Second run: re-add the SAME site WITH a cap. The site dedups (SitesSkipped),
	// so the cap is dropped — the notice must fire and name the URL.
	second := newInitCmd(BuildInfo{Version: "9.9.9"})
	var secondErr bytes.Buffer
	second.SetOut(&bytes.Buffer{})
	second.SetErr(&secondErr)
	second.SetArgs([]string{
		"--add-site",
		"--contact-email", "ops@example.com",
		"--site", "https://example.com",
		"--i-am-authorized",
		"--max-pages", "500",
	})
	if err := second.Execute(); err != nil {
		t.Fatalf("second init: %v", err)
	}
	got := secondErr.String()
	if !strings.Contains(got, "--max-pages not applied to already-present site(s)") {
		t.Errorf("expected a skipped-cap notice on stderr, got:\n%s", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("skipped-cap notice must name the skipped URL, got:\n%s", got)
	}
}
