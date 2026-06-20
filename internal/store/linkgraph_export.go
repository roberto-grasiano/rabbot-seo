package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// A9 bounded-export store half. These reads back linkgraph.Export: a focus-URL
// neighborhood (≤ 2 hops, undirected over link_edges) and the segment / folder
// aggregates for overview mode. Identity is the #5 canonical URL string (urlx.Normalize
// + empty-path→"/", see canonicalURL) — a node IS a urls.url / link_edges.to_url, both
// stored in that one keyspace, so the to_url↔urls.url joins below match exactly.

// GraphEdge is one directed internal-link edge for export: a source page URL and
// the target URL it links to. Both are exact-string node identities.
type GraphEdge struct {
	From string
	To   string
}

// GraphNode is one node's export payload. URL is always set (it is the node
// identity); the remaining fields are populated only for ADMITTED nodes (a
// urls row exists). A linked-but-never-admitted target (to_url with no urls
// row) carries URL only and Admitted=false — it is still a valid node so the
// agent can draw "this 404's blast radius" even before the target is crawled.
type GraphNode struct {
	URL            string
	Admitted       bool
	Importance     float64
	GraphDepth     *int // nil = NULL (not yet swept / unreachable from root)
	InSitemap      bool
	LastFetchClass string
}

// NeighborhoodURLs returns the set of node URLs within `hops` UNDIRECTED hops of
// `focus` over link_edges (following edges both as source→target and
// target→source), capped at `maxNodes` rows (a hard server-side bound the caller
// enforces). The focus URL itself is always included (hop 0) even when it has no
// edges, so a focus on an isolated page still yields a one-node graph.
//
// hops is clamped to [0, 2] here as a defense-in-depth backstop; the caller
// (linkgraph.Export) rejects hops > 2 before reaching the store, but the CTE
// must never be handed an unbounded depth. maxNodes <= 0 falls back to a safe
// ceiling so a missing cap can never request an unbounded result.
//
// The CTE walks an undirected adjacency: at each frontier node, follow every
// link_edge whose from-url == the node (out) and whose to_url == the node (in),
// expanding to the other endpoint. Endpoints match on the #5 canonical keyspace
// (an out-edge's target is its to_url; an in-edge's source is the admitted urls.url
// for that from_url_id, both stored canonical), so the focus anchor is canonicalized
// to that same keyspace. A trailing slash on a non-root path and the query string are
// preserved by canonicalURL, so genuinely distinct resources stay distinct nodes.
func (db *DB) NeighborhoodURLs(ctx context.Context, siteID int64, focus string, hops, maxNodes int) ([]string, error) {
	if hops < 0 {
		hops = 0
	}
	if hops > 2 {
		hops = 2
	}
	if maxNodes <= 0 {
		maxNodes = 250
	}
	// Anchor on the canonical identity so a focus on "https://example.com" matches the
	// stored homepage node "https://example.com/" (and its inbound/outbound edges).
	focus = canonicalURL(focus)

	// adj(url, nbr) is the UNDIRECTED adjacency: every edge contributes both
	// (source→target) and (target→source) so the walk reaches in- and out-neighbors.
	// walk(url, depth) starts at focus@0 and expands to adjacent nodes, depth+1,
	// capped at `hops`. We take MIN(depth) so a node reachable by multiple paths
	// counts at its shortest hop. The outer LIMIT is the hard node cap; ORDER BY
	// depth then url keeps the truncation deterministic (closest-first), so a capped
	// result is the nearest neighborhood. Endpoints match by exact-string URL
	// (the source's admitted urls.url and the edge's to_url string), so /a and /a/
	// stay distinct nodes.
	const query = `
		WITH RECURSIVE adj(url, nbr) AS (
		    SELECT su.url, e.to_url
		      FROM link_edges e
		      JOIN urls su ON su.id = e.from_url_id AND su.site_id = ?
		     WHERE e.site_id = ?
		    UNION ALL
		    SELECT e.to_url, su.url
		      FROM link_edges e
		      JOIN urls su ON su.id = e.from_url_id AND su.site_id = ?
		     WHERE e.site_id = ?
		),
		walk(url, depth) AS (
		    SELECT ?, 0
		    UNION
		    SELECT adj.nbr, w.depth + 1
		      FROM walk w
		      JOIN adj ON adj.url = w.url
		     WHERE w.depth < ?
		)
		SELECT url, MIN(depth) AS depth
		  FROM walk
		 GROUP BY url
		 ORDER BY MIN(depth) ASC, url ASC
		 LIMIT ?`

	rows, err := db.Read().QueryContext(ctx, query,
		siteID, siteID, // adj out arm
		siteID, siteID, // adj in arm
		focus, // walk anchor
		hops,  // depth cap
		maxNodes)
	if err != nil {
		return nil, fmt.Errorf("neighborhood urls (focus=%q): %w", focus, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		var d int
		if scanErr := rows.Scan(&u, &d); scanErr != nil {
			return nil, fmt.Errorf("scan neighborhood url: %w", scanErr)
		}
		out = append(out, u)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate neighborhood urls: %w", rowsErr)
	}
	return out, nil
}

// EdgesAmong returns every link_edge of siteID whose BOTH endpoints are in the
// given node-URL set (the source's admitted url AND the to_url both members), up
// to maxEdges rows (a hard server-side bound). It is the edge half of focus-mode
// export: the caller first collects the bounded node set via NeighborhoodURLs,
// then asks for the induced subgraph's edges here so the export carries no edge
// that dangles outside the node set.
//
// An empty node set returns no edges (and runs no query). maxEdges <= 0 falls
// back to a safe ceiling. Ordered (from, to) for deterministic truncation.
func (db *DB) EdgesAmong(ctx context.Context, siteID int64, nodes []string, maxEdges int) ([]GraphEdge, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	if maxEdges <= 0 {
		maxEdges = 750
	}

	// Parameterized IN-lists for both endpoints (gosec: no string-built values).
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(nodes)), ",")
	// args: siteID, then the source-url IN-list, then the to_url IN-list, then maxEdges.
	args := make([]any, 0, 2*len(nodes)+2)
	args = append(args, siteID)
	for _, n := range nodes {
		args = append(args, n)
	}
	for _, n := range nodes {
		args = append(args, n)
	}
	args = append(args, maxEdges)

	q := `
		SELECT su.url AS from_url, e.to_url AS to_url
		  FROM link_edges e
		  JOIN urls su ON su.id = e.from_url_id AND su.site_id = e.site_id
		 WHERE e.site_id = ?
		   AND su.url   IN (` + placeholders + `)
		   AND e.to_url IN (` + placeholders + `)
		 ORDER BY su.url ASC, e.to_url ASC
		 LIMIT ?`

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("edges among nodes (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []GraphEdge
	for rows.Next() {
		var e GraphEdge
		if scanErr := rows.Scan(&e.From, &e.To); scanErr != nil {
			return nil, fmt.Errorf("scan graph edge: %w", scanErr)
		}
		out = append(out, e)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate graph edges: %w", rowsErr)
	}
	return out, nil
}

// NodePayloads returns the export payload for each node URL in `nodes`. An
// admitted node (a urls row exists) carries its importance / graph_depth /
// in_sitemap / last_fetch_class; a node URL with no urls row is returned with
// Admitted=false and zero-value fields (it is still a valid node — a
// linked-but-never-admitted target). Every requested URL appears exactly once in
// the result map keyed by URL, so the caller never has to guess a missing entry.
//
// An empty node set returns an empty map. The query pulls only the admitted rows;
// the caller's loop fills the non-admitted nodes as Admitted=false.
func (db *DB) NodePayloads(ctx context.Context, siteID int64, nodes []string) (map[string]GraphNode, error) {
	out := make(map[string]GraphNode, len(nodes))
	for _, n := range nodes {
		out[n] = GraphNode{URL: n, Admitted: false}
	}
	if len(nodes) == 0 {
		return out, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(nodes)), ",")
	args := make([]any, 0, len(nodes)+1)
	args = append(args, siteID)
	for _, n := range nodes {
		args = append(args, n)
	}

	q := `
		SELECT url, importance, graph_depth, in_sitemap, last_fetch_class
		  FROM urls
		 WHERE site_id = ?
		   AND url IN (` + placeholders + `)`

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("node payloads (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			n         GraphNode
			depth     sql.NullInt64
			inSitemap int
		)
		if scanErr := rows.Scan(&n.URL, &n.Importance, &depth, &inSitemap, &n.LastFetchClass); scanErr != nil {
			return nil, fmt.Errorf("scan node payload: %w", scanErr)
		}
		n.Admitted = true
		n.InSitemap = inSitemap != 0
		if depth.Valid {
			d := int(depth.Int64)
			n.GraphDepth = &d
		}
		out[n.URL] = n
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate node payloads: %w", rowsErr)
	}
	return out, nil
}

// SegmentEdgeWeight is one inter-segment (or intra-segment) edge weight for the
// overview-mode export: From and To are segment names, Weight the count of
// internal-link edges whose source belongs to From and whose target (an admitted
// urls row) belongs to To.
type SegmentEdgeWeight struct {
	From   string
	To     string
	Weight int
}

// SiteHasSegments reports whether siteID has any segment defined — overview mode
// uses it to choose segment aggregation vs the folder fallback.
func (db *DB) SiteHasSegments(ctx context.Context, siteID int64) (bool, error) {
	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE site_id = ?`, siteID).Scan(&n); err != nil {
		return false, fmt.Errorf("count segments (site=%d): %w", siteID, err)
	}
	return n > 0, nil
}

// SegmentEdgeWeights aggregates internal-link edges into inter-segment weights for
// overview mode: for each edge whose source admitted url is in segment A and whose
// target admitted url is in segment B, increment the (A,B) weight. Edges whose
// source or target is NOT a segment member (or whose target is not an admitted
// urls row) are dropped — overview groups only segmented pages. Ordered by weight
// DESC then names so a truncating caller keeps the heaviest flows.
//
// A URL in multiple segments contributes to one weight row per (sourceSegment,
// targetSegment) pair it participates in (the join fans out across memberships) —
// that is the intended semantics: a page in both "Blog" and "Money" linking a
// "Product" page counts for Blog→Product AND Money→Product.
func (db *DB) SegmentEdgeWeights(ctx context.Context, siteID int64) ([]SegmentEdgeWeight, error) {
	const q = `
		SELECT fs.name AS from_seg, ts.name AS to_seg, COUNT(*) AS weight
		  FROM link_edges e
		  JOIN urls su ON su.id = e.from_url_id AND su.site_id = ?
		  JOIN url_segments fus ON fus.url_id = su.id
		  JOIN segments fs ON fs.id = fus.segment_id
		  JOIN urls tu ON tu.site_id = e.site_id AND tu.url = e.to_url
		  JOIN url_segments tus ON tus.url_id = tu.id
		  JOIN segments ts ON ts.id = tus.segment_id
		 WHERE e.site_id = ?
		 GROUP BY fs.name, ts.name
		 ORDER BY weight DESC, from_seg ASC, to_seg ASC`
	rows, err := db.Read().QueryContext(ctx, q, siteID, siteID)
	if err != nil {
		return nil, fmt.Errorf("segment edge weights (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []SegmentEdgeWeight
	for rows.Next() {
		var w SegmentEdgeWeight
		if scanErr := rows.Scan(&w.From, &w.To, &w.Weight); scanErr != nil {
			return nil, fmt.Errorf("scan segment edge weight: %w", scanErr)
		}
		out = append(out, w)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate segment edge weights: %w", rowsErr)
	}
	return out, nil
}

// FolderEdge is one edge with both endpoints resolved to their first-path-segment
// folder (the overview folder fallback when a site has no segments). From and To
// are folder labels (e.g. "/blog", "/", "(other)" is applied by the caller after
// the ≤50-group cap).
type FolderEdge struct {
	From string
	To   string
}

// SiteEdgesResolved returns every internal-link edge of siteID as a (sourceURL,
// targetURL) pair where the source is admitted (the join is intrinsic). The
// overview folder-fallback caller (linkgraph) buckets each endpoint into its
// first-path-segment folder in Go (URL parsing belongs in the package that owns
// the export shape, not in SQL). Bounded by maxEdges so a hostile site cannot
// stream an unbounded scan into memory; ordered (from, to) for determinism.
func (db *DB) SiteEdgesResolved(ctx context.Context, siteID int64, maxEdges int) ([]GraphEdge, error) {
	if maxEdges <= 0 {
		maxEdges = 100000
	}
	const q = `
		SELECT su.url AS from_url, e.to_url AS to_url
		  FROM link_edges e
		  JOIN urls su ON su.id = e.from_url_id AND su.site_id = ?
		 WHERE e.site_id = ?
		 ORDER BY su.url ASC, e.to_url ASC
		 LIMIT ?`
	rows, err := db.Read().QueryContext(ctx, q, siteID, siteID, maxEdges)
	if err != nil {
		return nil, fmt.Errorf("site edges resolved (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []GraphEdge
	for rows.Next() {
		var e GraphEdge
		if scanErr := rows.Scan(&e.From, &e.To); scanErr != nil {
			return nil, fmt.Errorf("scan site edge: %w", scanErr)
		}
		out = append(out, e)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate site edges: %w", rowsErr)
	}
	return out, nil
}
