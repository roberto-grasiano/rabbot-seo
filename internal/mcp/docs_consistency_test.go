package mcpsrv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpDocPath walks up from this test file to the module root and returns the path to the
// MCP reference doc (docs/claude-mcp.md), where the full tool catalog is documented. The
// README links to it but no longer enumerates the tools (it stays a slim front page).
func mcpDocPath(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "docs", "claude-mcp.md")
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate module root (go.mod) above the test file")
	return ""
}

// registeredToolNames lists every tool the server actually registers, over an
// in-memory client session — the same end-to-end path the host uses.
func registeredToolNames(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(&mockBridge{}, "test")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "doc-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestMCPDocDocumentsEveryTool is the doc-consistency guard: every MCP tool the server
// registers must be named in docs/claude-mcp.md. Adding a tool without documenting it (as
// the summarize_changes digest tool would otherwise be) fails here.
func TestMCPDocDocumentsEveryTool(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(mcpDocPath(t))
	if err != nil {
		t.Fatalf("read MCP doc: %v", err)
	}
	doc := string(raw)

	for _, name := range registeredToolNames(t) {
		if !strings.Contains(doc, name) {
			t.Errorf("docs/claude-mcp.md does not document the registered MCP tool %q", name)
		}
	}
}

// TestMCPDocDocumentsSummarizeChanges pins the load-bearing framing of the activity
// digest in docs/claude-mcp.md: the tool name, the default 7-day window, the ad-hoc
// `since`, and that the binary emits structured facts for Claude to summarise.
func TestMCPDocDocumentsSummarizeChanges(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(mcpDocPath(t))
	if err != nil {
		t.Fatalf("read MCP doc: %v", err)
	}
	low := strings.ToLower(string(raw))

	for _, want := range []string{
		"summarize_changes", // the tool
		"7 days",            // default window framing
		"since",             // ad-hoc window
		"structured facts",  // facts → Claude writes the prose
	} {
		if !strings.Contains(low, strings.ToLower(want)) {
			t.Errorf("docs/claude-mcp.md missing summarize_changes doc anchor %q", want)
		}
	}
}

// TestRemoteSnippet_SSHShape pins the VPS-over-SSH snippet shape docs/claude-mcp.md (and
// docs/vps.md) document: command "ssh", args [host, remoteBin, "mcp"], and NO token/env.
// If a future change to the remote-arg builder diverges from the documented recipe, this
// fails — keeping the documented VPS recipe honest.
func TestRemoteSnippet_SSHShape(t *testing.T) {
	t.Parallel()

	got := RemoteSnippet("you@vps", "rabbot")

	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("RemoteSnippet not valid JSON: %v\n%s", err, got)
	}
	entry := m["mcpServers"].(map[string]any)["rabbot"].(map[string]any)
	if entry["command"] != "ssh" {
		t.Fatalf("remote command = %v, want ssh", entry["command"])
	}
	args, ok := entry["args"].([]any)
	if !ok || len(args) != 3 ||
		args[0] != "you@vps" || args[1] != "rabbot" || args[2] != "mcp" {
		t.Fatalf(`remote args = %v, want ["you@vps","rabbot","mcp"]`, entry["args"])
	}
	// The SSH snippet must never carry a token/env (the token stays on the VPS).
	if _, hasEnv := entry["env"]; hasEnv {
		t.Fatalf("remote snippet must not carry an env block: %s", got)
	}
	if strings.Contains(strings.ToLower(got), "token") {
		t.Fatalf("remote snippet must never contain a token: %s", got)
	}
}
