package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
)

func intPtrCLI(n int) *int { return &n }

// TestGraphRender_FocusTable golden-tests a focus-mode export table: the mode/focus
// header, the node and edge counts with the truncated flag, and one node + one edge
// row.
func TestGraphRender_FocusTable(t *testing.T) {
	t.Parallel()
	exp := linkgraph.Export{
		Mode:       linkgraph.ModeFocus,
		Focus:      "https://a.test/money",
		Hops:       2,
		Nodes:      []linkgraph.ExportNode{{URL: "https://a.test/money", Admitted: true, Importance: 0.9, GraphDepth: intPtrCLI(1)}, {URL: "https://a.test/new", Admitted: false}},
		Edges:      []linkgraph.ExportEdge{{From: "https://a.test/", To: "https://a.test/money"}},
		Truncated:  true,
		TotalNodes: 5,
		TotalEdges: 9,
	}
	var buf bytes.Buffer
	if err := renderGraphTable(&buf, exp); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"mode", "focus", "https://a.test/money",
		"nodes", "2 of 5", "edges", "1 of 9", "truncated true",
		"NODES", "(crawled)", "(linked, not yet crawled)",
		"EDGES", "->",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("focus table missing %q in:\n%s", want, out)
		}
	}
}

// TestGraphRender_FocusJSON golden-tests the focus-mode export JSON: the truncated
// flag + exact totals + the node/edge wire shape (including admitted + graph_depth)
// that the agent consumes to draw the graph.
func TestGraphRender_FocusJSON(t *testing.T) {
	t.Parallel()
	exp := linkgraph.Export{
		Mode:       linkgraph.ModeFocus,
		Focus:      "https://a.test/money",
		Hops:       2,
		Nodes:      []linkgraph.ExportNode{{URL: "https://a.test/money", Admitted: true, Importance: 0.9, GraphDepth: intPtrCLI(1)}},
		Edges:      []linkgraph.ExportEdge{{From: "https://a.test/", To: "https://a.test/money"}},
		Truncated:  true,
		TotalNodes: 5,
		TotalEdges: 9,
	}
	var buf bytes.Buffer
	if err := renderGraphJSON(&buf, exp); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["mode"] != "focus" || got["focus"] != "https://a.test/money" {
		t.Fatalf("json mode/focus = %v / %v", got["mode"], got["focus"])
	}
	if got["truncated"] != true {
		t.Fatalf("json truncated = %v", got["truncated"])
	}
	if got["total_nodes"].(float64) != 5 || got["total_edges"].(float64) != 9 {
		t.Fatalf("json totals = %v / %v", got["total_nodes"], got["total_edges"])
	}
	nodes := got["nodes"].([]any)
	n0 := nodes[0].(map[string]any)
	if n0["url"] != "https://a.test/money" || n0["admitted"] != true || n0["graph_depth"].(float64) != 1 {
		t.Fatalf("json node[0] = %v", n0)
	}
}

// TestGraphRender_OverviewTable golden-tests an overview-mode export table: the
// grouping label, group + group-edge sections, and the weighted inter-group edge.
func TestGraphRender_OverviewTable(t *testing.T) {
	t.Parallel()
	exp := linkgraph.Export{
		Mode:       linkgraph.ModeOverview,
		Grouping:   "folder",
		Groups:     []linkgraph.GroupNode{{Name: "/blog"}, {Name: "/"}},
		GroupEdges: []linkgraph.GroupEdge{{From: "/blog", To: "/", Weight: 12}},
		TotalNodes: 2,
		TotalEdges: 1,
	}
	var buf bytes.Buffer
	if err := renderGraphTable(&buf, exp); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"mode", "overview", "grouping", "folder", "GROUPS", "/blog", "GROUP EDGES", "weight 12"} {
		if !strings.Contains(out, want) {
			t.Fatalf("overview table missing %q in:\n%s", want, out)
		}
	}
}

// TestGraphRender_OverviewJSON golden-tests the overview-mode export JSON.
func TestGraphRender_OverviewJSON(t *testing.T) {
	t.Parallel()
	exp := linkgraph.Export{
		Mode:       linkgraph.ModeOverview,
		Grouping:   "segment",
		Groups:     []linkgraph.GroupNode{{Name: "blog"}},
		GroupEdges: []linkgraph.GroupEdge{{From: "blog", To: "shop", Weight: 4}},
		TotalNodes: 1,
		TotalEdges: 1,
	}
	var buf bytes.Buffer
	if err := renderGraphJSON(&buf, exp); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["mode"] != "overview" || got["grouping"] != "segment" {
		t.Fatalf("json mode/grouping = %v / %v", got["mode"], got["grouping"])
	}
	ge := got["group_edges"].([]any)[0].(map[string]any)
	if ge["from"] != "blog" || ge["to"] != "shop" || ge["weight"].(float64) != 4 {
		t.Fatalf("json group_edge = %v", ge)
	}
}

// TestGraphCmd_RejectsHopsAboveCeiling asserts the CLI rejects --hops 3 clearly
// (criterion 8) at the front door, before any store work. The export layer enforces
// the same ≤2 ceiling; this guards the friendly CLI-layer check.
func TestGraphCmd_RejectsHopsAboveCeiling(t *testing.T) {
	t.Parallel()
	cmd := NewRootCmd(BuildInfo{})
	cmd.SetArgs([]string{"graph", "https://a.test", "--focus", "https://a.test/x", "--hops", "3"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --hops 3")
	}
	if !strings.Contains(err.Error(), "--hops") || !strings.Contains(err.Error(), "<= 2") {
		t.Fatalf("error %q must mention --hops must be <= 2", err)
	}
}
