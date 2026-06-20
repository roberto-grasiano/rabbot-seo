package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestIsGracefulDisconnect(t *testing.T) {
	t.Parallel()

	graceful := []error{
		nil,
		context.Canceled,
		io.EOF,
		mcp.ErrConnectionClosed,
		// The SDK's current EOF-on-disconnect shape: %w internal err + %v io.EOF.
		fmt.Errorf("server is closing: %v", io.EOF), //nolint:errorlint // exercising the SDK's non-wrapping shape
		errors.New("jsonrpc2: connection closed"),
	}
	for _, e := range graceful {
		if !isGracefulDisconnect(e) {
			t.Errorf("isGracefulDisconnect(%v) = false, want true", e)
		}
	}

	notGraceful := []error{
		errors.New("boom: real failure"),
		errors.New("config: parse error"),
		context.DeadlineExceeded,
		io.ErrUnexpectedEOF, // genuine truncation must surface, not be swallowed
	}
	for _, e := range notGraceful {
		if isGracefulDisconnect(e) {
			t.Errorf("isGracefulDisconnect(%v) = true, want false", e)
		}
	}
}

func TestNewServer_ConstructsWithoutPanic(t *testing.T) {
	t.Parallel()

	// AddResource panics on a non-absolute URI; a successful return proves all
	// three registered URIs are absolute rabbot:// URIs.
	srv := NewServer(&mockBridge{}, "9.9.9")
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestResourceURIsAreAbsolute(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{uriHealth, uriStatus, uriSites} {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", uri, err)
		}
		if u.Scheme == "" {
			t.Fatalf("URI %q has empty scheme (AddResource would panic)", uri)
		}
	}
}

// TestServerRoundTrip wires the server to an in-process client over in-memory
// transports and reads all three resources end-to-end, proving the handlers are
// registered under the expected URIs and produce JSON the client can read.
func TestServerRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	bridge := &mockBridge{
		healthErr: nil,
		status:    control.StatusResponse{Version: "9.9.9", SiteCount: 1, Paused: false},
		sites:     []SiteView{{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"}},
	}
	srv := NewServer(bridge, "9.9.9")

	serverT, clientT := mcp.NewInMemoryTransports()
	// Server must be connected before the client (the client initializes the
	// session during connection).
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer func() { _ = ss.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	// List: all three resources are advertised.
	list, err := cs.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	gotURIs := map[string]bool{}
	for _, r := range list.Resources {
		gotURIs[r.URI] = true
		if r.MIMEType != "application/json" {
			t.Errorf("resource %q MIMEType = %q, want application/json", r.URI, r.MIMEType)
		}
	}
	for _, want := range []string{uriHealth, uriStatus, uriSites} {
		if !gotURIs[want] {
			t.Errorf("ListResources missing %q (got %v)", want, gotURIs)
		}
	}

	// Read health.
	hres, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uriHealth})
	if err != nil {
		t.Fatalf("ReadResource health: %v", err)
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal([]byte(hres.Contents[0].Text), &health); err != nil {
		t.Fatalf("health payload: %v", err)
	}
	if !health.Healthy {
		t.Errorf("health.healthy = false, want true")
	}

	// Read status.
	sres, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uriStatus})
	if err != nil {
		t.Fatalf("ReadResource status: %v", err)
	}
	var st control.StatusResponse
	if err := json.Unmarshal([]byte(sres.Contents[0].Text), &st); err != nil {
		t.Fatalf("status payload: %v", err)
	}
	if st.Version != "9.9.9" || st.SiteCount != 1 {
		t.Errorf("status payload = %+v, want version 9.9.9 / SiteCount 1", st)
	}

	// Read sites.
	stres, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uriSites})
	if err != nil {
		t.Fatalf("ReadResource sites: %v", err)
	}
	var views []SiteView
	if err := json.Unmarshal([]byte(stres.Contents[0].Text), &views); err != nil {
		t.Fatalf("sites payload: %v", err)
	}
	if len(views) != 1 || views[0].ID != 1 || views[0].URL != "https://a.test" {
		t.Errorf("sites payload = %+v, want one site id=1", views)
	}
}

// TestReadToolsRegistered wires the server to an in-process client and asserts all
// five read tools are advertised, each with ReadOnlyHint:true (and DH/OW unset,
// which is correct: those hints are meaningful only when ReadOnlyHint==false).
func TestReadToolsRegistered(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bridge := &mockBridge{
		status:  control.StatusResponse{Version: "9.9.9", SiteCount: 1},
		sites:   []SiteView{{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"}},
		site:    SiteDetail{ID: 1, URL: "https://a.test", Name: "A", Enabled: true},
		issues:  []IssueView{{ID: 9, RuleID: "r", Status: "open", Severity: "critical"}},
		history: HistoryView{URL: "https://a.test", Changes: []ChangeView{{Field: "title"}}},
	}
	srv := NewServer(bridge, "9.9.9")

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer func() { _ = ss.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	list, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range list.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"get_status", "list_sites", "get_site", "list_issues", "get_history"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("read tool %q not advertised (got %v)", name, keysOf(byName))
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %q: ReadOnlyHint = false/nil, want true (annotations=%+v)", name, tool.Annotations)
		}
		// Read tools must NOT carry a destructive/open-world claim (meaningful only
		// when ReadOnlyHint==false; we leave them nil).
		if tool.Annotations.DestructiveHint != nil {
			t.Fatalf("tool %q: DestructiveHint set on a read tool, want nil", name)
		}
		if tool.Annotations.OpenWorldHint != nil {
			t.Fatalf("tool %q: OpenWorldHint set on a read tool, want nil", name)
		}
	}
}

// TestReadToolsCallable proves a representative read tool returns structured output
// the client can decode end-to-end.
func TestReadToolsCallable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bridge := &mockBridge{status: control.StatusResponse{Version: "7.7.7", SiteCount: 4, Paused: true}}
	srv := NewServer(bridge, "7.7.7")

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_status"})
	if err != nil {
		t.Fatalf("CallTool get_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_status IsError=true, want false: %+v", res.Content)
	}
	// The SDK puts the structured Out under StructuredContent and a JSON text mirror
	// in Content. Decode the text mirror.
	if len(res.Content) == 0 {
		t.Fatal("get_status returned no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	var got control.StatusResponse
	if err := json.Unmarshal([]byte(tc.Text), &got); err != nil {
		t.Fatalf("status output not JSON: %v (%s)", err, tc.Text)
	}
	if got.Version != "7.7.7" || got.SiteCount != 4 || !got.Paused {
		t.Fatalf("status output = %+v, want version 7.7.7 / SiteCount 4 / Paused", got)
	}
}

// keysOf is a tiny helper for the failure message above.
func keysOf(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
