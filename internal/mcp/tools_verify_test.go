package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// connectToolClient wires the server to an in-memory client (mirrors
// server_test.go's round-trip), returning a connected client session.
func connectToolClient(t *testing.T, b Bridge) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(b, "9.9.9")
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("CallToolResult has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func TestVerifyTools_Annotations(t *testing.T) {
	cs := connectToolClient(t, &mockBridge{})
	list, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range list.Tools {
		byName[tl.Name] = tl
	}

	vb := byName["verify_begin"]
	if vb == nil {
		t.Fatal("verify_begin tool not registered")
	}
	if vb.Annotations == nil || !vb.Annotations.ReadOnlyHint {
		t.Errorf("verify_begin ReadOnlyHint = %v, want true", vb.Annotations)
	}

	vc := byName["verify_check"]
	if vc == nil {
		t.Fatal("verify_check tool not registered")
	}
	if vc.Annotations == nil {
		t.Fatal("verify_check has no annotations")
	}
	if vc.Annotations.ReadOnlyHint {
		t.Error("verify_check ReadOnlyHint = true, want false (it writes the proof record)")
	}
	if vc.Annotations.DestructiveHint == nil || *vc.Annotations.DestructiveHint {
		t.Error("verify_check DestructiveHint must be explicitly false (SDK default is true)")
	}
	if !vc.Annotations.IdempotentHint {
		t.Error("verify_check IdempotentHint = false, want true")
	}
	if vc.Annotations.OpenWorldHint == nil || !*vc.Annotations.OpenWorldHint {
		t.Error("verify_check OpenWorldHint = nil/false, want true (fetches an external surface)")
	}
}

func TestVerifyBeginTool_ReturnsTokenAndInstructions(t *testing.T) {
	b := &mockBridge{verifyBegin: VerifyView{
		SiteID: 5, Method: "well_known", Token: "rab_X",
		State: "throttled", Instructions: "Place this file: ...", Throttled: true,
	}}
	cs := connectToolClient(t, b)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "verify_begin",
		Arguments: map[string]any{"site_id": 5, "method": "well_known"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("verify_begin IsError, content=%+v", res.Content)
	}
	var got VerifyView
	if err := json.Unmarshal([]byte(textOf(t, res)), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got.Token != "rab_X" || got.Instructions == "" {
		t.Fatalf("verify_begin output = %+v, want token + instructions", got)
	}
	if b.lastVerifySiteID != 5 || b.lastVerifyMethod != "well_known" {
		t.Fatalf("bridge args = (%d,%q), want (5,well_known)", b.lastVerifySiteID, b.lastVerifyMethod)
	}
}

func TestVerifyCheckTool_DaemonDownFriendlyError(t *testing.T) {
	b := &mockBridge{verifyCheckErr: control.ErrDaemonNotRunning}
	cs := connectToolClient(t, b)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "verify_check",
		Arguments: map[string]any{"site_id": 1, "method": "meta"},
	})
	if err != nil {
		t.Fatalf("CallTool transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("verify_check on a down daemon should be a tool error (IsError)")
	}
	if !strings.Contains(textOf(t, res), "daemon not running") {
		t.Errorf("verify_check error = %q, want a friendly 'daemon not running' message", textOf(t, res))
	}
}
