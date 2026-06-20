package mcpsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConnectSnippet_Shape(t *testing.T) {
	t.Parallel()

	got := Snippet("/opt/rabbot")

	// Must parse and have exactly the canonical shape, with NO token/env present.
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("Snippet not valid JSON: %v\n%s", err, got)
	}
	servers, ok := m["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcpServers map: %s", got)
	}
	entry, ok := servers["rabbot"].(map[string]any)
	if !ok {
		t.Fatalf("missing rabbot entry: %s", got)
	}
	if entry["command"] != "/opt/rabbot" {
		t.Fatalf("command = %v, want /opt/rabbot", entry["command"])
	}
	args, ok := entry["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Fatalf("args = %v, want [\"mcp\"]", entry["args"])
	}
	// No env / token leakage.
	if _, hasEnv := entry["env"]; hasEnv {
		t.Fatalf("snippet must not carry an env block: %s", got)
	}
	if strings.Contains(strings.ToLower(got), "token") {
		t.Fatalf("snippet must never contain a token: %s", got)
	}
	// Stable indentation (two-space) for human copy.
	if !strings.Contains(got, "\n  \"mcpServers\"") {
		t.Fatalf("snippet not indented as expected:\n%s", got)
	}
}

func TestConnectWrite_NewFileCreatesDirsAndModes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "nested", "deeper", ".mcp.json")

	if err := WriteConfig(target, "/opt/rabbot"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// Parent dir 0700.
	di, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if runtime.GOOS != "windows" && di.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir mode = %o, want 0700", di.Mode().Perm())
	}
	// File 0600.
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Content has exactly our entry.
	raw, _ := os.ReadFile(target)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("written file not JSON: %v", err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, ok := servers["rabbot"]; !ok {
		t.Fatalf("rabbot entry missing: %s", raw)
	}
}

// TestConnectWrite_MergePreservesEverything is the load-bearing merge test: a naive
// overwrite (clobber) MUST fail it. The unrelated top-level key and a sibling
// server under mcpServers must survive; only rabbot is added/updated.
func TestConnectWrite_MergePreservesEverything(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "claude_desktop_config.json")

	seed, err := os.ReadFile(filepath.Join("testdata", "existing_claude_config.json"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if err := os.WriteFile(target, seed, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := WriteConfig(target, "/opt/rabbot"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	raw, _ := os.ReadFile(target)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("merged file not JSON: %v\n%s", err, raw)
	}

	// 1. Unrelated top-level key survives intact.
	un, ok := m["unrelatedTopLevelKey"].(map[string]any)
	if !ok {
		t.Fatalf("unrelatedTopLevelKey was clobbered: %s", raw)
	}
	if un["keepMe"] != true {
		t.Fatalf("unrelatedTopLevelKey.keepMe lost: %v", un)
	}

	servers, ok := m["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers clobbered: %s", raw)
	}
	// 2. Sibling server survives.
	other, ok := servers["some-other-server"].(map[string]any)
	if !ok {
		t.Fatalf("sibling server some-other-server was clobbered: %s", raw)
	}
	if other["command"] != "/usr/local/bin/other" {
		t.Fatalf("sibling server command changed: %v", other)
	}
	// 3. Only rabbot added/updated.
	kb, ok := servers["rabbot"].(map[string]any)
	if !ok {
		t.Fatalf("rabbot entry missing after merge: %s", raw)
	}
	if kb["command"] != "/opt/rabbot" {
		t.Fatalf("rabbot command = %v, want /opt/rabbot", kb["command"])
	}
}

func TestConnectWrite_UpdatesExistingRabbot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, ".mcp.json")
	// Seed with an OLD rabbot entry pointing at a stale binary + an env block.
	seed := `{"mcpServers":{"rabbot":{"command":"/old/path","args":["mcp"],"env":{"X":"1"}}}}`
	if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteConfig(target, "/new/rabbot"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, _ := os.ReadFile(target)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	kb := m["mcpServers"].(map[string]any)["rabbot"].(map[string]any)
	if kb["command"] != "/new/rabbot" {
		t.Fatalf("command not updated: %v", kb)
	}
	if _, hasEnv := kb["env"]; hasEnv {
		t.Fatalf("our entry replaced the rabbot value, stale env must be gone: %v", kb)
	}
}

// TestConnectWrite_JSONNullConfig guards the nil-map panic: a config file whose
// entire content is the JSON literal `null` unmarshals into a nil map, and the
// later `doc[mcpServersKey] = servers` write would panic without the nil guard.
// WriteConfig must instead start a fresh doc and end with mcpServers.rabbot set.
func TestConnectWrite_JSONNullConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, ".mcp.json")
	// Pre-write a file containing exactly the JSON null literal.
	if err := os.WriteFile(target, []byte("null"), 0o600); err != nil {
		t.Fatalf("seed null: %v", err)
	}

	if err := WriteConfig(target, "/opt/rabbot"); err != nil {
		t.Fatalf("WriteConfig over JSON-null config: %v", err)
	}

	raw, _ := os.ReadFile(target)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("written file not JSON: %v\n%s", err, raw)
	}
	servers, ok := m["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers map missing after merging JSON-null config: %s", raw)
	}
	kb, ok := servers["rabbot"].(map[string]any)
	if !ok {
		t.Fatalf("rabbot entry missing after merging JSON-null config: %s", raw)
	}
	if kb["command"] != "/opt/rabbot" {
		t.Fatalf("rabbot command = %v, want /opt/rabbot", kb["command"])
	}
}

func TestConnectTargetPath_PerOS(t *testing.T) {
	t.Parallel()

	// Project + Claude Code resolve to ./.mcp.json (cwd-relative).
	for _, tgt := range []Target{TargetProject, TargetClaudeCode} {
		p, err := TargetPath(tgt)
		if err != nil {
			t.Fatalf("TargetPath(%v): %v", tgt, err)
		}
		if filepath.Base(p) != ".mcp.json" {
			t.Fatalf("TargetPath(%v) base = %q, want .mcp.json", tgt, filepath.Base(p))
		}
	}

	// Claude Desktop resolves to a per-OS path ending in claude_desktop_config.json.
	p, err := TargetPath(TargetClaudeDesktop)
	if err != nil {
		t.Fatalf("TargetPath(desktop): %v", err)
	}
	if filepath.Base(p) != "claude_desktop_config.json" {
		t.Fatalf("desktop path base = %q, want claude_desktop_config.json", filepath.Base(p))
	}
	if !strings.Contains(p, "Claude") {
		t.Fatalf("desktop path %q does not contain the Claude app dir", p)
	}

	// Print target has no path.
	if _, err := TargetPath(TargetPrint); err == nil {
		t.Fatal("TargetPath(print) should error: print has no file path")
	}
}

func TestParseTarget(t *testing.T) {
	t.Parallel()

	cases := map[string]Target{
		"print":          TargetPrint,
		"project":        TargetProject,
		"claude-code":    TargetClaudeCode,
		"claude-desktop": TargetClaudeDesktop,
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseTarget(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseTarget("bogus"); err == nil {
		t.Fatal("ParseTarget(bogus) should error")
	}
}

func TestServerEntry_DefaultDirsBakeNoArgs(t *testing.T) {
	t.Parallel()
	entry := serverEntry("/opt/rabbot", "", "") // no dataDir, no configPath
	args, ok := entry["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Fatalf("default-dir entry args = %v, want [\"mcp\"]", entry["args"])
	}
}

func TestServerEntry_CustomDataDirBaked(t *testing.T) {
	t.Parallel()
	entry := serverEntry("/opt/rabbot", "/srv/kb/data", "")
	args := entry["args"].([]any)
	want := []any{"mcp", "--data-dir", "/srv/kb/data"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v (full: %v)", i, args[i], want[i], args)
		}
	}
}

func TestServerEntry_CustomConfigBaked(t *testing.T) {
	t.Parallel()
	entry := serverEntry("/opt/rabbot", "", "/etc/kb/config.yaml")
	args := entry["args"].([]any)
	want := []any{"mcp", "--config", "/etc/kb/config.yaml"}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

func TestServerEntry_BothBaked(t *testing.T) {
	t.Parallel()
	entry := serverEntry("/opt/rabbot", "/srv/kb/data", "/etc/kb/config.yaml")
	args := entry["args"].([]any)
	// Order pinned: mcp, --data-dir, <dir>, --config, <path>.
	want := []any{"mcp", "--data-dir", "/srv/kb/data", "--config", "/etc/kb/config.yaml"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

// SnippetWithDirs bakes the dirs into the copyable snippet; the default-dir
// snippet must equal the legacy Snippet output byte-for-byte.
func TestSnippetWithDirs_DefaultEqualsLegacy(t *testing.T) {
	t.Parallel()
	if SnippetWithDirs("/opt/rabbot", "", "") != Snippet("/opt/rabbot") {
		t.Fatal("default-dir SnippetWithDirs must equal legacy Snippet output")
	}
}

func TestRemoteServerEntry_Shape(t *testing.T) {
	t.Parallel()
	entry := remoteServerEntry("you@vps", "rabbot")
	if entry["command"] != "ssh" {
		t.Fatalf("command = %v, want ssh", entry["command"])
	}
	args := entry["args"].([]any)
	want := []any{"you@vps", "rabbot", "mcp"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

func TestRemoteServerEntry_DefaultBin(t *testing.T) {
	t.Parallel()
	entry := remoteServerEntry("you@vps", "") // empty remote bin defaults to "rabbot"
	args := entry["args"].([]any)
	if args[1] != "rabbot" {
		t.Fatalf("default remote bin = %v, want rabbot", args[1])
	}
}

func TestRemoteSnippet_NoTokenNoEnv(t *testing.T) {
	t.Parallel()
	got := RemoteSnippet("you@vps", "rabbot")
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("RemoteSnippet not JSON: %v\n%s", err, got)
	}
	entry := m["mcpServers"].(map[string]any)["rabbot"].(map[string]any)
	if entry["command"] != "ssh" {
		t.Fatalf("remote command = %v, want ssh:\n%s", entry["command"], got)
	}
	if _, hasEnv := entry["env"]; hasEnv {
		t.Fatalf("remote snippet must carry no env block:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "token") {
		t.Fatalf("remote snippet must never contain a token:\n%s", got)
	}
}

func TestWriteRemoteConfig_MergesSSHEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, ".mcp.json")
	if err := WriteRemoteConfig(target, "you@vps", "rabbot"); err != nil {
		t.Fatalf("WriteRemoteConfig: %v", err)
	}
	raw, _ := os.ReadFile(target)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("written file not JSON: %v", err)
	}
	entry := m["mcpServers"].(map[string]any)["rabbot"].(map[string]any)
	if entry["command"] != "ssh" {
		t.Fatalf("written remote command = %v, want ssh", entry["command"])
	}
}
