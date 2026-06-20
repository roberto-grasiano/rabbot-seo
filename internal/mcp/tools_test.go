package mcpsrv

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// toolByName lists the server's tools over an in-memory client session and returns
// the named tool (or fails). It exercises the real AddTool registration end-to-end.
func toolByName(t *testing.T, b Bridge, name string) *mcp.Tool {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(b, "test")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range res.Tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered (have %d tools)", name, len(res.Tools))
	return nil
}

func TestActionToolsRegistered(t *testing.T) {
	t.Parallel()
	b := &mockBridge{}
	for _, name := range []string{
		"add_site", "recheck_site", "pause_monitoring",
		"resume_monitoring", "ignore_issue", "send_test_alert",
	} {
		if tl := toolByName(t, b, name); tl == nil {
			t.Fatalf("missing tool %q", name)
		}
	}
}

// callTool invokes a tool by name with raw JSON args and returns the result.
func callTool(t *testing.T, b Bridge, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(b, "test")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) protocol error: %v", name, err)
	}
	return res
}

// resultText concatenates the text content blocks of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb += tc.Text
		}
	}
	return sb
}

func TestTool_AddSite_Success(t *testing.T) {
	t.Parallel()
	b := &mockBridge{addSiteResp: control.AddSiteResponse{SiteID: 12}}
	res := callTool(t, b, "add_site", map[string]any{"url": "https://x.test", "name": "X"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if b.lastAddSite.URL != "https://x.test" || b.lastAddSite.Name != "X" {
		t.Fatalf("bridge got %+v, want url/name forwarded", b.lastAddSite)
	}
	// Structured output carries the new site id.
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if sc["site_id"].(float64) != 12 {
			t.Fatalf("structured site_id = %v, want 12", sc["site_id"])
		}
	}
}

func TestTool_AddSite_DaemonDown(t *testing.T) {
	t.Parallel()
	b := &mockBridge{addSiteErr: control.ErrDaemonNotRunning}
	res := callTool(t, b, "add_site", map[string]any{"url": "https://x.test"})
	if !res.IsError {
		t.Fatal("expected IsError=true when daemon down")
	}
	txt := resultText(t, res)
	if !strings.Contains(txt, "daemon not running") {
		t.Fatalf("error text = %q, want friendly daemon-down message", txt)
	}
}

func TestTool_Recheck_Success(t *testing.T) {
	t.Parallel()
	b := &mockBridge{crawlResp: control.CrawlResponse{Queued: 3}}
	res := callTool(t, b, "recheck_site", map[string]any{"target": "https://x.test"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	if b.lastRecheck != "https://x.test" {
		t.Fatalf("target = %q, want forwarded", b.lastRecheck)
	}
}

func TestTool_Pause_Resume(t *testing.T) {
	t.Parallel()
	b := &mockBridge{}
	if res := callTool(t, b, "pause_monitoring", map[string]any{}); res.IsError {
		t.Fatalf("pause error: %s", resultText(t, res))
	}
	if res := callTool(t, b, "resume_monitoring", map[string]any{}); res.IsError {
		t.Fatalf("resume error: %s", resultText(t, res))
	}
}

func TestTool_IgnoreIssue_Success(t *testing.T) {
	t.Parallel()
	b := &mockBridge{}
	res := callTool(t, b, "ignore_issue", map[string]any{"issue_id": 5})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	if b.lastIgnoreID != 5 {
		t.Fatalf("ignored id = %d, want 5", b.lastIgnoreID)
	}
}

func TestTool_SendTestAlert_DaemonDown(t *testing.T) {
	t.Parallel()
	b := &mockBridge{testAlertErr: control.ErrDaemonNotRunning}
	res := callTool(t, b, "send_test_alert", map[string]any{"notifier": "slack"})
	if !res.IsError {
		t.Fatal("expected IsError when daemon down")
	}
	if !strings.Contains(resultText(t, res), "daemon not running") {
		t.Fatalf("want friendly daemon-down text, got %q", resultText(t, res))
	}
}

func TestActionTools_NoSecretInOutput(t *testing.T) {
	t.Parallel()
	b := &mockBridge{
		addSiteResp: control.AddSiteResponse{SiteID: 1},
		crawlResp:   control.CrawlResponse{Queued: 1},
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"add_site", map[string]any{"url": "https://x.test"}},
		{"recheck_site", map[string]any{"target": "https://x.test"}},
		{"pause_monitoring", map[string]any{}},
		{"resume_monitoring", map[string]any{}},
		{"ignore_issue", map[string]any{"issue_id": 1}},
		{"send_test_alert", map[string]any{"notifier": "slack"}},
	} {
		res := callTool(t, b, tc.name, tc.args)
		txt := resultText(t, res)
		for _, secret := range []string{"Bearer", "Authorization", "control.token"} {
			if strings.Contains(txt, secret) {
				t.Errorf("%s output leaked %q: %s", tc.name, secret, txt)
			}
		}
	}
}

func TestActionToolAnnotations(t *testing.T) {
	t.Parallel()
	b := &mockBridge{}
	cases := []struct {
		name       string
		readOnly   bool
		destruct   bool // expected effective DestructiveHint
		idempotent bool
		openWorld  bool // expected effective OpenWorldHint
	}{
		{"add_site", false, false, false, true},
		{"recheck_site", false, false, true, true},
		{"pause_monitoring", false, false, true, false},
		{"resume_monitoring", false, false, true, false},
		{"ignore_issue", false, false, true, false},
		{"send_test_alert", false, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := toolByName(t, b, tc.name)
			a := tl.Annotations
			if a == nil {
				t.Fatalf("%s has nil Annotations", tc.name)
			}
			if a.ReadOnlyHint != tc.readOnly {
				t.Errorf("%s ReadOnlyHint = %v, want %v", tc.name, a.ReadOnlyHint, tc.readOnly)
			}
			// DestructiveHint MUST be set explicitly (non-nil) and false — a nil
			// pointer defaults to true (destructive), which we must never advertise.
			if a.DestructiveHint == nil {
				t.Errorf("%s DestructiveHint is nil; must be explicitly false", tc.name)
			} else if *a.DestructiveHint != tc.destruct {
				t.Errorf("%s DestructiveHint = %v, want %v", tc.name, *a.DestructiveHint, tc.destruct)
			}
			if a.IdempotentHint != tc.idempotent {
				t.Errorf("%s IdempotentHint = %v, want %v", tc.name, a.IdempotentHint, tc.idempotent)
			}
			// OpenWorldHint MUST be set explicitly (non-nil) — nil defaults to true.
			if a.OpenWorldHint == nil {
				t.Errorf("%s OpenWorldHint is nil; must be explicitly set (default is true)", tc.name)
			} else if *a.OpenWorldHint != tc.openWorld {
				t.Errorf("%s OpenWorldHint = %v, want %v", tc.name, *a.OpenWorldHint, tc.openWorld)
			}
		})
	}
}
