package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsrv "github.com/roberto-grasiano/rabbot-seo/internal/mcp"
)

func TestMCPCmd_Use(t *testing.T) {
	cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
	if cmd.Use != "mcp" {
		t.Fatalf("Use = %q, want \"mcp\"", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Fatal("RunE is nil")
	}
}

func TestMCPCmd_RegisteredInRoot(t *testing.T) {
	root := NewRootCmd(BuildInfo{})
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "mcp" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp command not registered in NewRootCmd")
	}
}

// TestMCPCmd_BuildsBridgeAndServesViaSeam exercises the RunE path WITHOUT entering
// the blocking stdio loop: serveFn is replaced with a seam that captures the
// bridge + version it was handed and returns immediately. It also asserts the
// command writes NOTHING to stdout (stdout is the MCP JSON-RPC channel).
func TestMCPCmd_BuildsBridgeAndServesViaSeam(t *testing.T) {
	// Isolate config/data dirs so loadConfig/newControlClient resolve under temp.
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	var (
		gotBridge  mcpsrv.Bridge
		gotVersion string
		called     bool
	)
	orig := serveFn
	serveFn = func(_ context.Context, b mcpsrv.Bridge, version string) error {
		called = true
		gotBridge = b
		gotVersion = version
		return nil
	}
	t.Cleanup(func() { serveFn = orig })

	cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("serveFn was not called")
	}
	if gotBridge == nil {
		t.Fatal("serveFn got a nil bridge")
	}
	if gotVersion != "9.9.9" {
		t.Fatalf("serveFn version = %q, want 9.9.9", gotVersion)
	}
	// stdout MUST stay clean: it is the MCP channel.
	if out.Len() != 0 {
		t.Fatalf("command wrote %d bytes to stdout, want 0: %q", out.Len(), out.String())
	}
}

// TestMCPCmd_AnySeamErrorSurfaces pins the command-layer contract after disconnect
// classification was made single-owner: the RunE no longer second-guesses the serve
// seam. mcpsrv.Serve is the SOLE owner of graceful-disconnect normalization (it maps
// io.EOF / context.Canceled / ErrConnectionClosed / SDK substrings to nil — see
// internal/mcp.TestIsGracefulDisconnect). The command therefore treats ANY non-nil
// return from the seam as a real error, including a raw wrapped io.EOF /
// context.Canceled that Serve would itself have swallowed — those never reach the
// command on the real path, so re-filtering them here is dead code we don't carry.
func TestMCPCmd_AnySeamErrorSurfaces(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	cases := map[string]error{
		"genuine failure":      fmt.Errorf("boom: real failure"),
		"raw wrapped eof":      fmt.Errorf("server is closing: %w", io.EOF),
		"raw context canceled": fmt.Errorf("run: %w", context.Canceled),
	}
	for name, retErr := range cases {
		t.Run(name, func(t *testing.T) {
			orig := serveFn
			serveFn = func(context.Context, mcpsrv.Bridge, string) error { return retErr }
			t.Cleanup(func() { serveFn = orig })

			cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{})
			if err := cmd.Execute(); err == nil {
				t.Fatal("any non-nil serve return must be surfaced, got nil")
			}
		})
	}
}

// TestMCPCmd_GracefulServeReturnIsCleanExit pins that when the serve seam returns
// nil (which is what mcpsrv.Serve does for a routine client disconnect / Ctrl-C),
// the command exits cleanly so main does not print an alarming "error:" line.
func TestMCPCmd_GracefulServeReturnIsCleanExit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))

	orig := serveFn
	serveFn = func(context.Context, mcpsrv.Bridge, string) error { return nil }
	t.Cleanup(func() { serveFn = orig })

	cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a nil serve return must exit cleanly, got: %v", err)
	}
}

func TestMCPCmd_HasDirFlags(t *testing.T) {
	cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
	if cmd.Flags().Lookup("data-dir") == nil {
		t.Error("mcp command missing --data-dir flag")
	}
	if cmd.Flags().Lookup("config") == nil {
		t.Error("mcp command missing --config flag")
	}
}

// TestMCPCmd_DataDirFlagResolves asserts a custom --data-dir reaches the bridge
// construction: with an isolated dir holding a control.token, the command builds
// a client/bridge without error via the serveFn seam.
func TestMCPCmd_DataDirFlagResolves(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, "cfg")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed a control.token so newControlClientFromConfigDir finds it.
	if err := os.WriteFile(filepath.Join(cfgDir, "control.token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBridge mcpsrv.Bridge
	orig := serveFn
	serveFn = func(_ context.Context, b mcpsrv.Bridge, _ string) error {
		gotBridge = b
		return nil
	}
	t.Cleanup(func() { serveFn = orig })

	cmd := newMCPCmd(BuildInfo{Version: "9.9.9"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--config", filepath.Join(cfgDir, "config.yaml"), "--data-dir", dataDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute with dir flags: %v", err)
	}
	if gotBridge == nil {
		t.Fatal("bridge not built under custom dirs")
	}
}

func TestConnectWrite_RemoteEmitsSSH(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".mcp.json")
	// connectWriteRemote writes an SSH-transport entry to an explicit path.
	if err := connectWriteRemote(target, "you@vps", "rabbot"); err != nil {
		t.Fatalf("connectWriteRemote: %v", err)
	}
	raw, _ := os.ReadFile(target)
	if !strings.Contains(string(raw), `"ssh"`) {
		t.Fatalf("remote connect-write must emit an ssh command:\n%s", raw)
	}
	if !strings.Contains(string(raw), "you@vps") {
		t.Fatalf("remote connect-write must include the destination:\n%s", raw)
	}
}

func TestConnectWriteDirs_BakesCustomDataDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".mcp.json")
	if err := connectWriteDirs(target, "/opt/rabbot", "/srv/data", ""); err != nil {
		t.Fatalf("connectWriteDirs: %v", err)
	}
	raw, _ := os.ReadFile(target)
	if !strings.Contains(string(raw), "--data-dir") || !strings.Contains(string(raw), "/srv/data") {
		t.Fatalf("custom data-dir must be baked into the written args:\n%s", raw)
	}
}
