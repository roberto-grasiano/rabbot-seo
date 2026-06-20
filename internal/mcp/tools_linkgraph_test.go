package mcpsrv

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// ─── blast_radius / what_links_to (A9) ───────────────────────────────────────

// TestBlastRadiusHandler_OK asserts blast_radius passes the URL + a default linker
// limit through and round-trips the summary DTO.
func TestBlastRadiusHandler_OK(t *testing.T) {
	t.Parallel()
	m := &mockBridge{links: LinksView{
		URL:             "https://a.test/p",
		Inlinks:         3,
		InlinkTotal:     3,
		HighImportance:  1,
		WeightedInlinks: 2.4,
		Linkers:         []LinkerView{{URLID: 5, URL: "https://a.test/hub", Importance: 0.9}},
	}}
	h := blastRadiusHandler(m)
	_, out, err := h(context.Background(), nil, BlastRadiusInput{URL: "https://a.test/p"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastBlastURL != "https://a.test/p" {
		t.Fatalf("url = %q", m.lastBlastURL)
	}
	if m.lastBlastLimit != blastRadiusDefaultLinkers {
		t.Fatalf("default limit = %d, want %d", m.lastBlastLimit, blastRadiusDefaultLinkers)
	}
	if out.Error != "" || out.Links.Inlinks != 3 || out.Links.HighImportance != 1 || out.Links.WeightedInlinks != 2.4 {
		t.Fatalf("blast-radius out = %+v err=%q", out.Links, out.Error)
	}
}

// TestBlastRadiusHandler_NotFound asserts a never-linked URL is data, not an error.
func TestBlastRadiusHandler_NotFound(t *testing.T) {
	t.Parallel()
	m := &mockBridge{links: LinksView{URL: "https://a.test/orphan", NotFound: true}}
	h := blastRadiusHandler(m)
	_, out, err := h(context.Background(), nil, BlastRadiusInput{URL: "https://a.test/orphan"})
	if err != nil {
		t.Fatalf("not-found must be data, not error: %v", err)
	}
	if !out.Links.NotFound || out.Error != "" {
		t.Fatalf("want not-found-as-data, got %+v", out)
	}
}

// TestBlastRadiusHandler_DaemonDown asserts a down daemon is errors-as-data.
func TestBlastRadiusHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{linksErr: control.ErrDaemonNotRunning}
	h := blastRadiusHandler(m)
	_, out, err := h(context.Background(), nil, BlastRadiusInput{URL: "https://a/p"})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}

// TestWhatLinksToHandler_OK asserts what_links_to passes the URL + a default (larger)
// limit through and round-trips the ranked-linker DTO.
func TestWhatLinksToHandler_OK(t *testing.T) {
	t.Parallel()
	m := &mockBridge{links: LinksView{
		URL:         "https://a.test/p",
		Inlinks:     2,
		InlinkTotal: 7, // exact total exceeds the returned list (truncated by limit)
		Linkers: []LinkerView{
			{URLID: 1, URL: "https://a.test/hub", Importance: 0.9},
			{URLID: 2, URL: "https://a.test/blog", Importance: 0.3},
		},
	}}
	h := whatLinksToHandler(m)
	_, out, err := h(context.Background(), nil, WhatLinksToInput{URL: "https://a.test/p", Limit: 2})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastWhatLinksURL != "https://a.test/p" || m.lastWhatLinksLim != 2 {
		t.Fatalf("args = url %q limit %d", m.lastWhatLinksURL, m.lastWhatLinksLim)
	}
	if out.Links.InlinkTotal != 7 || len(out.Links.Linkers) != 2 || out.Links.Linkers[0].Importance != 0.9 {
		t.Fatalf("what-links-to out = %+v", out.Links)
	}
}

// TestWhatLinksToHandler_DefaultLimit asserts a zero/omitted limit falls back to the
// what_links_to default (a larger ranked list than blast_radius).
func TestWhatLinksToHandler_DefaultLimit(t *testing.T) {
	t.Parallel()
	m := &mockBridge{}
	h := whatLinksToHandler(m)
	if _, _, err := h(context.Background(), nil, WhatLinksToInput{URL: "https://a/p"}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastWhatLinksLim != whatLinksToDefaultLinkers {
		t.Fatalf("default limit = %d, want %d", m.lastWhatLinksLim, whatLinksToDefaultLinkers)
	}
}

// ─── get_link_graph (A9) ─────────────────────────────────────────────────────

// TestGetLinkGraphHandler_Focus asserts get_link_graph passes the focus query
// through and round-trips a focus-mode export DTO.
func TestGetLinkGraphHandler_Focus(t *testing.T) {
	t.Parallel()
	m := &mockBridge{graph: GraphView{
		Mode:       "focus",
		Focus:      "https://a.test/p",
		Hops:       2,
		Nodes:      []GraphNodeView{{URL: "https://a.test/p", Admitted: true, Importance: 0.7}},
		Edges:      []GraphEdgeView{{From: "https://a.test/p", To: "https://a.test/q"}},
		Truncated:  true,
		TotalNodes: 40,
		TotalEdges: 90,
	}}
	h := getLinkGraphHandler(m)
	_, out, err := h(context.Background(), nil, GetLinkGraphInput{SiteID: 7, Focus: "https://a.test/p", Hops: 2, Limit: 20})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastGraphQuery.SiteID != 7 || m.lastGraphQuery.Focus != "https://a.test/p" ||
		m.lastGraphQuery.Hops != 2 || m.lastGraphQuery.Limit != 20 {
		t.Fatalf("graph query = %+v", m.lastGraphQuery)
	}
	if out.Error != "" || out.Graph.Mode != "focus" || len(out.Graph.Nodes) != 1 ||
		!out.Graph.Truncated || out.Graph.TotalNodes != 40 {
		t.Fatalf("graph out = %+v err=%q", out.Graph, out.Error)
	}
}

// TestGetLinkGraphHandler_HopsTooBigIsToolError asserts hops > 2 is a TOOL ERROR the
// model can correct — NOT data, and the bridge is never called (criterion 8).
func TestGetLinkGraphHandler_HopsTooBigIsToolError(t *testing.T) {
	t.Parallel()
	m := &mockBridge{}
	h := getLinkGraphHandler(m)
	_, _, err := h(context.Background(), nil, GetLinkGraphInput{SiteID: 1, Focus: "https://a/p", Hops: 3})
	if err == nil {
		t.Fatalf("hops=3 must be a tool error")
	}
	if m.lastGraphQuery.SiteID != 0 {
		t.Fatalf("bridge must NOT be called when hops > 2 (got query %+v)", m.lastGraphQuery)
	}
}

// TestGetLinkGraphHandler_NotFound asserts an unknown site id is data, not an error.
func TestGetLinkGraphHandler_NotFound(t *testing.T) {
	t.Parallel()
	m := &mockBridge{graph: GraphView{NotFound: true}}
	h := getLinkGraphHandler(m)
	_, out, err := h(context.Background(), nil, GetLinkGraphInput{SiteID: 999})
	if err != nil {
		t.Fatalf("not-found must be data, not error: %v", err)
	}
	if !out.Graph.NotFound || out.Error != "" {
		t.Fatalf("want not-found-as-data, got %+v", out)
	}
}

// TestGetLinkGraphHandler_DaemonDown asserts a down daemon is errors-as-data.
func TestGetLinkGraphHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{graphErr: control.ErrDaemonNotRunning}
	h := getLinkGraphHandler(m)
	_, out, err := h(context.Background(), nil, GetLinkGraphInput{SiteID: 1})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}

// TestLinkGraphToolsRegistered is the criterion-11 assertion: the THREE snake_case
// A9 tools (blast_radius, what_links_to, get_link_graph) are advertised by the
// server, carry ReadOnlyHint, and there is NO fourth orphans tool.
func TestLinkGraphToolsRegistered(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := NewServer(&mockBridge{}, "9.9.9")
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

	for _, name := range []string{"blast_radius", "what_links_to", "get_link_graph"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("A9 tool %q not advertised", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %q: ReadOnlyHint = false/nil, want true", name)
		}
	}
	// No fourth orphans tool (decision): orphans surface via list_issues + the CLI.
	for _, banned := range []string{"orphans", "get_orphans", "list_orphans", "orphan_pages"} {
		if _, ok := byName[banned]; ok {
			t.Fatalf("unexpected orphans tool %q registered; decision is NO fourth tool", banned)
		}
	}
}
