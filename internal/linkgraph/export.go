package linkgraph

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Bounded get_link_graph export. Two modes:
//   - focus: the ≤2-hop in+out neighborhood of a focus URL, node/edge caps
//     enforced server-side (config default, HARD ceiling regardless of config),
//     response carries `Truncated` + node/edge totals bounded by that ceiling
//     (a floor — "at least this many" — when truncated);
//   - overview: aggregate by segment (A7) with inter-segment edge weights, or —
//     when a site has no segments — a first-path-segment FOLDER fallback (≤ 50
//     groups + an "(other)" rollup).
//
// Either mode serializes to tens of KB, never megabytes (asserted in tests): the
// node/edge caps and the ≤50-group fold bound the payload size hard. Rabbot emits
// JSON only — the agent draws (no image/SVG/DOT emitters here).

// FolderOther is the rollup label for folders beyond the ≤50-group cap in the
// overview folder fallback.
const FolderOther = "(other)"

// MaxOverviewGroups caps the overview's distinct groups (segments or folders).
// Folders beyond the cap fold into FolderOther; with segments the count is already
// operator-bounded but we apply the same ceiling for a uniform payload bound.
const MaxOverviewGroups = 50

// HardOverviewScanEdges hard-caps the edge scan that feeds the no-segments folder
// fallback. The fallback's RESPONSE is already bounded to ≤51 groups, but the
// intermediate edge slice it folds is not — without a cap, a large legitimate
// no-segments site streams up to the store's 100k ceiling of GraphEdge structs
// (tens of MB of strings) across the boundary, violating this package's
// documented "tens of KB, never megabytes" envelope and the bounding discipline
// every other export path follows (focus nodes ≤250, edges ≤750). 50_000 edges is
// a generous ceiling for folder aggregation (the busiest folders dominate the
// weights long before this) while keeping the intermediate allocation bounded; the
// fold is order-insensitive so a clipped scan still yields a representative — and
// now explicitly Truncated — folder graph.
const HardOverviewScanEdges = 50000

// hardCeil clamps a configured cap to its hard ceiling (and floors a non-positive
// cap to the ceiling so a zero never requests an unbounded result).
func hardCeil(configured, hard int) int {
	if configured <= 0 || configured > hard {
		return hard
	}
	return configured
}

// ExportMode selects the export shape.
type ExportMode string

const (
	ModeFocus    ExportMode = "focus"
	ModeOverview ExportMode = "overview"
)

// Query is one export request. SiteID is required. Mode defaults to overview when
// Focus is empty and to focus when Focus is set (an explicit Mode wins). Hops is
// the focus-mode neighborhood radius, HARD-capped at 2 (a Hops > 2 is rejected,
// criterion 8). Limit overrides the node cap downward only (it can never raise it
// above the hard ceiling).
type Query struct {
	SiteID int64
	Mode   ExportMode
	Focus  string // focus-mode anchor URL
	Hops   int    // focus-mode radius; > 2 is an error
	Limit  int    // optional node-cap override (downward only)
}

// MaxFocusHops is the hard hop ceiling for focus-mode export. A request for more
// is rejected (criterion 8: "hops=3 rejected clearly").
const MaxFocusHops = 2

// ExportNode is one node in a focus-mode export. URL is the exact-string node
// identity. The remaining fields are populated for ADMITTED nodes; a
// linked-but-never-admitted target carries URL + Admitted=false (so the agent can
// still draw "this 404's blast radius" before the target is crawled).
type ExportNode struct {
	URL            string  `json:"url"`
	Admitted       bool    `json:"admitted"`
	Importance     float64 `json:"importance"`
	GraphDepth     *int    `json:"graph_depth,omitempty"`
	InSitemap      bool    `json:"in_sitemap"`
	LastFetchClass string  `json:"last_fetch_class,omitempty"`
}

// ExportEdge is one directed internal-link edge in a focus-mode export.
type ExportEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GroupNode is one aggregated group (segment or folder) in an overview export.
type GroupNode struct {
	Name string `json:"name"`
}

// GroupEdge is one weighted inter-group edge in an overview export.
type GroupEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Weight int    `json:"weight"`
}

// Export is the get_link_graph response. Exactly one of (Nodes/Edges) or
// (Groups/GroupEdges) is populated, per Mode. Truncated is true when the focus
// neighborhood hit a node or edge cap; TotalNodes/TotalEdges are bounded by the
// hard ceiling — EXACT when the neighborhood fits under it, a floor ("at least
// this many") when Truncated — so the agent knows the export is a bounded sample.
type Export struct {
	Mode  ExportMode `json:"mode"`
	Focus string     `json:"focus,omitempty"`
	Hops  int        `json:"hops,omitempty"`

	// focus mode
	Nodes []ExportNode `json:"nodes,omitempty"`
	Edges []ExportEdge `json:"edges,omitempty"`

	// overview mode
	Grouping   string      `json:"grouping,omitempty"` // "segment" | "folder"
	Groups     []GroupNode `json:"groups,omitempty"`
	GroupEdges []GroupEdge `json:"group_edges,omitempty"`

	Truncated  bool `json:"truncated"`
	TotalNodes int  `json:"total_nodes"`
	TotalEdges int  `json:"total_edges"`
}

// Export builds the bounded export for q. It rejects Hops > MaxFocusHops clearly
// (criterion 8) and otherwise dispatches to focus or overview mode.
func (g *Grapher) Export(ctx context.Context, q Query) (Export, error) {
	mode := q.Mode
	if mode == "" {
		if q.Focus != "" {
			mode = ModeFocus
		} else {
			mode = ModeOverview
		}
	}
	switch mode {
	case ModeFocus:
		if q.Hops > MaxFocusHops {
			return Export{}, fmt.Errorf("hops must be <= %d (got %d)", MaxFocusHops, q.Hops)
		}
		if q.Hops < 0 {
			return Export{}, fmt.Errorf("hops must be >= 0 (got %d)", q.Hops)
		}
		if q.Focus == "" {
			return Export{}, fmt.Errorf("focus mode requires a focus url")
		}
		return g.exportFocus(ctx, q)
	case ModeOverview:
		return g.exportOverview(ctx, q)
	default:
		return Export{}, fmt.Errorf("unknown export mode %q", mode)
	}
}

// exportFocus builds the ≤2-hop in+out neighborhood of q.Focus, bounded by the
// node/edge caps (config default, hard ceiling regardless of config). The store
// computes the bounded node set (closest-first) and the induced subgraph's edges;
// here we enforce the caps, attach node payloads, and compute the ceiling-bounded
// totals for the Truncated flag.
func (g *Grapher) exportFocus(ctx context.Context, q Query) (Export, error) {
	hops := q.Hops
	if hops <= 0 {
		hops = MaxFocusHops // default to the full ≤2-hop neighborhood
	}

	nodeCap := hardCeil(g.exportMaxNodes, HardExportMaxNodes)
	if q.Limit > 0 && q.Limit < nodeCap {
		nodeCap = q.Limit // downward override only
	}
	edgeCap := hardCeil(g.exportMaxEdges, HardExportMaxEdges)

	// The bounded (closest-first) node set; the store applies the node cap.
	nodeURLs, err := g.db.NeighborhoodURLs(ctx, q.SiteID, q.Focus, hops, nodeCap)
	if err != nil {
		return Export{}, fmt.Errorf("export focus neighborhood: %w", err)
	}

	// Ceiling-bounded totals so the agent knows whether the export is a
	// sample. We re-run the neighborhood with the hard ceiling as the bound to get
	// the true node count and the full induced-edge count. Bounding the "total" by
	// the hard ceiling keeps even the COUNT query from streaming an unbounded set
	// into memory on a hostile fan-out — the totals are "at least this many", which
	// the Truncated flag already communicates.
	fullNodeURLs, err := g.db.NeighborhoodURLs(ctx, q.SiteID, q.Focus, hops, HardExportMaxNodes)
	if err != nil {
		return Export{}, fmt.Errorf("export focus total nodes: %w", err)
	}
	fullEdges, err := g.db.EdgesAmong(ctx, q.SiteID, fullNodeURLs, HardExportMaxEdges)
	if err != nil {
		return Export{}, fmt.Errorf("export focus total edges: %w", err)
	}

	// The induced subgraph's edges, over the CAPPED node set, bounded by the edge cap.
	edges, err := g.db.EdgesAmong(ctx, q.SiteID, nodeURLs, edgeCap)
	if err != nil {
		return Export{}, fmt.Errorf("export focus edges: %w", err)
	}

	payloads, err := g.db.NodePayloads(ctx, q.SiteID, nodeURLs)
	if err != nil {
		return Export{}, fmt.Errorf("export focus node payloads: %w", err)
	}

	out := Export{
		Mode:       ModeFocus,
		Focus:      q.Focus,
		Hops:       hops,
		TotalNodes: len(fullNodeURLs),
		TotalEdges: len(fullEdges),
	}
	for _, u := range nodeURLs {
		p := payloads[u] // always present (NodePayloads seeds every requested url)
		out.Nodes = append(out.Nodes, ExportNode{
			URL:            p.URL,
			Admitted:       p.Admitted,
			Importance:     p.Importance,
			GraphDepth:     p.GraphDepth,
			InSitemap:      p.InSitemap,
			LastFetchClass: p.LastFetchClass,
		})
	}
	for _, e := range edges {
		out.Edges = append(out.Edges, ExportEdge{From: e.From, To: e.To})
	}
	// Truncated when the rendered set is smaller than the true (hard-ceiling-bounded)
	// totals — i.e. a node or edge cap clipped the response. TotalNodes/TotalEdges
	// are themselves bounded by the hard ceilings (an "at least this many" floor on a
	// hostile fan-out), so this stays true whenever the export is a bounded sample.
	out.Truncated = len(out.Nodes) < out.TotalNodes || len(out.Edges) < out.TotalEdges
	return out, nil
}

// exportOverview aggregates the whole site into a small group graph. When the
// site has segments (A7), groups are segment names and edge weights are
// inter-segment link counts (SegmentEdgeWeights). With NO segments it falls back
// to first-path-segment FOLDER grouping over every edge, folding folders beyond
// the ≤50-group cap into "(other)".
func (g *Grapher) exportOverview(ctx context.Context, q Query) (Export, error) {
	hasSegments, err := g.db.SiteHasSegments(ctx, q.SiteID)
	if err != nil {
		return Export{}, fmt.Errorf("export overview: segments check: %w", err)
	}
	if hasSegments {
		return g.exportOverviewSegments(ctx, q)
	}
	return g.exportOverviewFolders(ctx, q)
}

func (g *Grapher) exportOverviewSegments(ctx context.Context, q Query) (Export, error) {
	weights, err := g.db.SegmentEdgeWeights(ctx, q.SiteID)
	if err != nil {
		return Export{}, fmt.Errorf("export overview segments: %w", err)
	}
	out := Export{Mode: ModeOverview, Grouping: "segment"}
	groupSet := map[string]struct{}{}
	for _, w := range weights {
		groupSet[w.From] = struct{}{}
		groupSet[w.To] = struct{}{}
		out.GroupEdges = append(out.GroupEdges, GroupEdge{From: w.From, To: w.To, Weight: w.Weight})
	}
	out.Groups = sortedGroups(groupSet)
	out.TotalNodes = len(out.Groups)
	out.TotalEdges = len(out.GroupEdges)
	return out, nil
}

// exportOverviewFolders is the no-segments fallback: bucket every edge endpoint by
// its first path segment, sum the inter-folder weights, then fold all but the top
// MaxOverviewGroups folders into "(other)" so the group set is ≤ 51 (50 + other).
func (g *Grapher) exportOverviewFolders(ctx context.Context, q Query) (Export, error) {
	// Bound the scan so a large no-segments site cannot stream an unbounded edge
	// set into memory: pass an EXPLICIT hard cap (matching the bounding discipline
	// of every other export path), not 0. We request one extra row so a clip is
	// detectable — if the store returns the full ceiling+1, the scan was truncated
	// and we flag it on the response (the fold is order-insensitive, so a clipped
	// scan still yields a representative folder graph).
	scanCap := g.overviewScanCap
	if scanCap <= 0 {
		scanCap = HardOverviewScanEdges
	}
	edges, err := g.db.SiteEdgesResolved(ctx, q.SiteID, scanCap+1)
	if err != nil {
		return Export{}, fmt.Errorf("export overview folders: %w", err)
	}
	scanTruncated := len(edges) > scanCap
	if scanTruncated {
		edges = edges[:scanCap]
	}

	// Per-folder out-degree weight (the folder's total outbound edges) chooses which
	// folders survive the ≤50 cap (busiest first). Edge weights are summed per
	// (fromFolder, toFolder) pair.
	folderWeight := map[string]int{}
	type pair struct{ from, to string }
	pairWeight := map[pair]int{}
	for _, e := range edges {
		ff := folderOf(e.From)
		tf := folderOf(e.To)
		folderWeight[ff]++
		pairWeight[pair{ff, tf}]++
	}

	keep := topFolders(folderWeight, MaxOverviewGroups)

	// Re-bucket pairs into kept folders + "(other)".
	label := func(f string) string {
		if _, ok := keep[f]; ok {
			return f
		}
		return FolderOther
	}
	merged := map[pair]int{}
	for p, w := range pairWeight {
		merged[pair{label(p.from), label(p.to)}] += w
	}

	out := Export{Mode: ModeOverview, Grouping: "folder"}
	groupSet := map[string]struct{}{}
	for p, w := range merged {
		groupSet[p.from] = struct{}{}
		groupSet[p.to] = struct{}{}
		out.GroupEdges = append(out.GroupEdges, GroupEdge{From: p.from, To: p.to, Weight: w})
	}
	// Deterministic edge order: weight DESC, then names.
	sort.Slice(out.GroupEdges, func(i, j int) bool {
		a, b := out.GroupEdges[i], out.GroupEdges[j]
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	out.Groups = sortedGroups(groupSet)
	out.TotalNodes = len(out.Groups)
	out.TotalEdges = len(out.GroupEdges)
	// Truncated communicates that the underlying edge scan hit the hard ceiling, so
	// the folder weights are computed over a (bounded) sample of the site's edges.
	out.Truncated = scanTruncated
	return out, nil
}

// folderOf returns the first path segment of an absolute URL as its folder label,
// e.g. https://x/blog/post -> "/blog", https://x/ -> "/", a fragment-only or
// unparsable URL -> "/". Identity stays exact-string elsewhere; this bucketing is
// presentation-only and never feeds node identity.
func folderOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "/"
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return "/"
	}
	first := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		first = p[:i]
	}
	return "/" + first
}

// topFolders returns the set of the `n` highest-weight folders (busiest first; ties
// broken by name for determinism). Folders beyond `n` are dropped from the set so
// the caller buckets them into "(other)".
func topFolders(weights map[string]int, n int) map[string]struct{} {
	type fw struct {
		folder string
		weight int
	}
	all := make([]fw, 0, len(weights))
	for f, w := range weights {
		all = append(all, fw{f, w})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].weight != all[j].weight {
			return all[i].weight > all[j].weight
		}
		return all[i].folder < all[j].folder
	})
	keep := make(map[string]struct{}, n)
	for i := 0; i < len(all) && i < n; i++ {
		keep[all[i].folder] = struct{}{}
	}
	return keep
}

// sortedGroups returns the group names sorted ascending (deterministic output).
func sortedGroups(set map[string]struct{}) []GroupNode {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]GroupNode, 0, len(names))
	for _, n := range names {
		out = append(out, GroupNode{Name: n})
	}
	return out
}
