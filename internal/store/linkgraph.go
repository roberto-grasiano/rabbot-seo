package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// A9 link-graph LITE store half. link_edges is keyed by urls (not snapshots) so
// retention never erodes the graph; to_url is TEXT (not an FK) so a
// linked-but-never-admitted target stays a valid question subject. All timestamps
// are UTC.
//
// #5 (MARQUEE) identity: a node IS a canonical URL string. link_edges.to_url is a
// net/url ResolveReference result (extract.go) while urls.url is the value a caller
// supplied; left raw, the two never share a keyspace, so a homepage registered as
// "https://example.com" (no slash) never matched href-resolved
// "https://example.com/" and reported zero inlinks / blast_radius. Per the LOCKED
// DECISION ("shared canonicalizer at both write boundaries") canonicalURL is applied
// at BOTH boundaries — writing link_edges.to_url (syncOutEdges) AND writing/keying
// urls.url (UpsertURL/GetURL) — so the keyspaces match. sites.base_url is kept
// verbatim (it is also the user-facing/robots/SSRF value); the BFS anchor and the
// orphan root-exclusion canonicalize it at READ instead (siteRootURL). The column
// stays TEXT (decision #9 — no FK). For existing rows, normalize-on-write plus a
// natural re-crawl re-canonicalizes the graph (acceptable for launch).

// canonicalURL is the single owner of "what string identifies this URL in the link
// graph and the url inventory". It applies urlx.Normalize (RFC 3986 §6.2 safe,
// identity-preserving normalization: lowercase scheme+host, strip default port,
// uppercase %-escape hex + decode unreserved octets, remove dot-segments, drop the
// fragment) and then folds an EMPTY path to "/". The empty→"/" step is what closes
// the homepage case: net/url's ResolveReference always emits at least "/" for an
// absolute target, so a bare-host base ("https://example.com") and a resolved link
// ("https://example.com/") must collapse to one identity. A trailing slash on a
// NON-root path ("/a" vs "/a/") and the query string stay identity-significant —
// urlx.Normalize preserves both, so genuinely distinct resources are not merged.
//
// It is total: an input urlx.Normalize cannot parse (a relative path, a malformed
// URL, a "scheme:opaque" form) is returned UNCHANGED rather than dropped or errored,
// so a caller can never lose a row by handing canonicalURL a value Normalize
// rejects — the worst case degrades to the prior exact-string behavior for that one
// odd value. Canonical output is idempotent (urlx.Normalize is, and re-folding an
// already-"/" path is a no-op), so re-canonicalizing a stored value is stable.
func canonicalURL(raw string) string {
	n, err := urlx.Normalize(raw)
	if err != nil {
		return raw
	}
	u, perr := url.Parse(n)
	if perr != nil {
		// Normalize just produced n, so this cannot fail in practice; degrade to the
		// normalized string rather than panicking.
		return n
	}
	if u.Path == "" {
		u.Path = "/"
		return u.String()
	}
	return n
}

// MaxBFSDepth caps the click-depth BFS sweep. A cycle or a hostile fan-out can
// otherwise walk forever; depths beyond the cap are left NULL (treated as
// unreachable from the root for the click_depth_regression signal).
const MaxBFSDepth = 20

// EdgeDelta reports what one SyncOutEdges call changed for a single source page:
// the absolute target URLs whose edges were newly inserted and those removed.
// Edges that merely had last_seen advanced are in neither slice.
type EdgeDelta struct {
	Added   []string
	Removed []string
}

// SyncOutEdges replaces fromURLID's out-edge set to exactly `links` in ONE
// transaction (this is on the crawl hot path — never N round trips of work
// outside the single write-tx): read the current out-set, diff vs the desired
// set, INSERT added (first_seen == last_seen == now UTC), advance last_seen on
// kept, DELETE removed. It returns the exact added/removed sets.
//
// Out-degree is capped at maxOutlinks (the deterministic FIRST-N of `links` in
// extractor order — the extracted slice is already deduped and ordered) so a
// hostile 50k-anchor page cannot bloat the table. maxOutlinks <= 0 falls back to
// 500 (the graph.max_outlinks_per_page default) so a missing/zero config value
// never disables the bound. The caller passes the config-sourced value.
//
// `now` is stored as-is; the caller is responsible for passing UTC (the crawl
// path does). The desired set is itself deduped here so a caller that passes a
// non-deduped slice cannot insert the same to_url twice (the PK would reject the
// second insert and fail the whole tx).
func (db *DB) SyncOutEdges(ctx context.Context, siteID, fromURLID int64, now time.Time, links []string) (EdgeDelta, error) {
	return db.syncOutEdges(ctx, siteID, fromURLID, now, links, 500)
}

// SyncOutEdgesCapped is SyncOutEdges with an explicit out-degree cap, so the
// caller can thread graph.max_outlinks_per_page through. SyncOutEdges keeps the
// 500 default for callers that don't carry config.
func (db *DB) SyncOutEdgesCapped(ctx context.Context, siteID, fromURLID int64, now time.Time, links []string, maxOutlinks int) (EdgeDelta, error) {
	return db.syncOutEdges(ctx, siteID, fromURLID, now, links, maxOutlinks)
}

func (db *DB) syncOutEdges(ctx context.Context, siteID, fromURLID int64, now time.Time, links []string, maxOutlinks int) (EdgeDelta, error) {
	if maxOutlinks <= 0 {
		maxOutlinks = 500
	}

	// Desired set: canonicalize each link to the shared keyspace (#5) FIRST, then
	// dedup preserving extractor order, then cap to the first N. Canonicalizing
	// before dedup means two divergent-but-equivalent hrefs (e.g. ".../b" and
	// ".../B" by host case, or ":443/b") collapse to one to_url and one edge, so the
	// out-degree cap counts identities, not raw anchor strings.
	desired := make([]string, 0, len(links))
	desiredSet := make(map[string]struct{}, len(links))
	for _, raw := range links {
		l := canonicalURL(raw)
		if _, dup := desiredSet[l]; dup {
			continue
		}
		desiredSet[l] = struct{}{}
		desired = append(desired, l)
		if len(desired) >= maxOutlinks {
			break
		}
	}
	// The cap dropped the overflow; the dedup map must match the capped set so a
	// kept-vs-added classification below uses the same membership the table will.
	if len(desired) < len(desiredSet) {
		desiredSet = make(map[string]struct{}, len(desired))
		for _, d := range desired {
			desiredSet[d] = struct{}{}
		}
	}

	var delta EdgeDelta
	err := db.WriteTx(ctx, func(tx Tx) error {
		current, err := scanOutEdges(ctx, tx, fromURLID)
		if err != nil {
			return err
		}
		currentSet := make(map[string]struct{}, len(current))
		for _, c := range current {
			currentSet[c] = struct{}{}
		}

		// INSERT new / UPDATE kept, in desired (deterministic) order.
		for _, to := range desired {
			if _, kept := currentSet[to]; kept {
				if _, uerr := tx.ExecContext(ctx,
					`UPDATE link_edges SET last_seen = ? WHERE from_url_id = ? AND to_url = ?`,
					now, fromURLID, to); uerr != nil {
					return fmt.Errorf("advance edge last_seen (from=%d to=%q): %w", fromURLID, to, uerr)
				}
				continue
			}
			if _, ierr := tx.ExecContext(ctx,
				`INSERT INTO link_edges (site_id, from_url_id, to_url, first_seen, last_seen)
				 VALUES (?, ?, ?, ?, ?)`,
				siteID, fromURLID, to, now, now); ierr != nil {
				return fmt.Errorf("insert edge (from=%d to=%q): %w", fromURLID, to, ierr)
			}
			delta.Added = append(delta.Added, to)
		}

		// DELETE edges no longer in the desired set, in stored (deterministic) order.
		for _, to := range current {
			if _, stillWanted := desiredSet[to]; stillWanted {
				continue
			}
			if _, derr := tx.ExecContext(ctx,
				`DELETE FROM link_edges WHERE from_url_id = ? AND to_url = ?`,
				fromURLID, to); derr != nil {
				return fmt.Errorf("delete edge (from=%d to=%q): %w", fromURLID, to, derr)
			}
			delta.Removed = append(delta.Removed, to)
		}
		return nil
	})
	if err != nil {
		return EdgeDelta{}, err
	}
	return delta, nil
}

// scanOutEdges returns fromURLID's current out-edge to_url set in a stable order
// (to_url), scanned inside the caller's write-tx. Extracted so the rows close is
// visible to sqlclosecheck.
func scanOutEdges(ctx context.Context, tx Tx, fromURLID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT to_url FROM link_edges WHERE from_url_id = ? ORDER BY to_url`, fromURLID)
	if err != nil {
		return nil, fmt.Errorf("read out-edges (from=%d): %w", fromURLID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var to string
		if scanErr := rows.Scan(&to); scanErr != nil {
			return nil, fmt.Errorf("scan out-edge: %w", scanErr)
		}
		out = append(out, to)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate out-edges: %w", rowsErr)
	}
	return out, nil
}

// Linker is one inbound source page for a target URL: the source's admitted URL
// row plus its importance, for ranking inlinks.
type Linker struct {
	URLID      int64
	URL        string
	Importance float64
}

// WhatLinksTo returns the admitted source pages that link TO `url` within
// siteID, ranked by source importance DESC (ties broken by url for determinism),
// limited to `limit` rows, plus the EXACT total inbound count (ignoring limit —
// criterion 4: "reports exact totals even when limited"). limit <= 0 returns no
// rows but still reports the exact total.
//
// Only edges whose source is an admitted urls row appear (the join on
// from_url_id is intrinsic — from_url_id is always an admitted source). The
// target `to_url` need not be admitted (TEXT, not an FK).
func (db *DB) WhatLinksTo(ctx context.Context, siteID int64, url string, limit int) (linkers []Linker, total int, err error) {
	// Canonicalize the queried target to the same keyspace to_url is written in (#5),
	// so a caller asking about "https://example.com" matches edges stored as the
	// resolved "https://example.com/".
	to := canonicalURL(url)
	// Exact total first, independent of limit.
	if terr := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM link_edges WHERE site_id = ? AND to_url = ?`,
		siteID, to).Scan(&total); terr != nil {
		return nil, 0, fmt.Errorf("count inlinks (to=%q): %w", url, terr)
	}
	if limit <= 0 {
		return nil, total, nil
	}

	rows, err := db.Read().QueryContext(ctx,
		`SELECT u.id, u.url, u.importance
		   FROM link_edges e
		   JOIN urls u ON u.id = e.from_url_id
		  WHERE e.site_id = ? AND e.to_url = ?
		  ORDER BY u.importance DESC, u.url ASC
		  LIMIT ?`,
		siteID, to, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list linkers (to=%q): %w", url, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var l Linker
		if scanErr := rows.Scan(&l.URLID, &l.URL, &l.Importance); scanErr != nil {
			return nil, 0, fmt.Errorf("scan linker: %w", scanErr)
		}
		linkers = append(linkers, l)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, 0, fmt.Errorf("iterate linkers: %w", rowsErr)
	}
	return linkers, total, nil
}

// HighImportanceThreshold is the inclusive cutoff for a "high-importance"
// inlink source in blast-radius: importance >= 0.70 counts, 0.69 does not.
const HighImportanceThreshold = 0.70

// BlastRadiusView answers "how bad is this URL going dark?": the inbound edge
// count, how many sources are high-importance (>= 0.70), and the weighted
// inlink mass Σ(0.5 + 0.5·importance(src)) over admitted sources.
type BlastRadiusView struct {
	URL             string
	Inlinks         int
	HighImportance  int
	WeightedInlinks float64
}

// BlastRadius computes the one-hop inbound blast radius for `url` within siteID.
// Counts/weights are over edges whose source is an admitted urls row (the join
// is intrinsic). WeightedInlinks uses the documented formula
// Σ(0.5 + 0.5·importance(src)); importance ∈ [0,1] (ColdStartImportance), so
// each source contributes between 0.5 (importance 0) and 1.0 (importance 1).
// This is answers-only — never written back to urls.importance (decision).
func (db *DB) BlastRadius(ctx context.Context, siteID int64, url string) (BlastRadiusView, error) {
	v := BlastRadiusView{URL: url}
	// Canonicalize the queried target to to_url's keyspace (#5) so a no-slash
	// homepage (or any divergent-but-equivalent form) matches its inbound edges.
	to := canonicalURL(url)
	// One aggregate query over the joined sources. COALESCE guards the no-row
	// case (a never-linked target → all zeros, not NULL).
	err := db.Read().QueryRowContext(ctx,
		`SELECT
		     COUNT(*) AS inlinks,
		     COALESCE(SUM(CASE WHEN u.importance >= ? THEN 1 ELSE 0 END), 0) AS high,
		     COALESCE(SUM(0.5 + 0.5 * u.importance), 0) AS weighted
		   FROM link_edges e
		   JOIN urls u ON u.id = e.from_url_id
		  WHERE e.site_id = ? AND e.to_url = ?`,
		HighImportanceThreshold, siteID, to).
		Scan(&v.Inlinks, &v.HighImportance, &v.WeightedInlinks)
	if err != nil {
		return BlastRadiusView{}, fmt.Errorf("blast radius (to=%q): %w", url, err)
	}
	return v, nil
}

// GraphWarm reports whether the site's first full crawl has completed — i.e.
// every admitted url has been fetched at least once (no urls.last_checked IS
// NULL). Until then the inlink picture is partial (a target's linkers may not
// have been crawled yet), so the eager per-sync orphan signal is not
// trustworthy and must be suppressed (the #83 cold-start gate; the authoritative
// periodic sweep still reconciles orphans once warm). A site with zero admitted
// urls is vacuously warm — there are no targets to orphan. Scoped to siteID, so
// one partial site never drags a sibling's warm state down. Backed by
// idx_urls_site_id.
func (db *DB) GraphWarm(ctx context.Context, siteID int64) (bool, error) {
	var pending int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM urls WHERE site_id = ? AND last_checked IS NULL`,
		siteID).Scan(&pending); err != nil {
		return false, fmt.Errorf("graph warm (site=%d): %w", siteID, err)
	}
	return pending == 0, nil
}

// OrphanPage is a monitored page with zero inbound internal edges.
type OrphanPage struct {
	URLID      int64
	URL        string
	Importance float64
}

// OrphanPages returns monitored pages (status_type='page') in siteID with ZERO
// inbound link_edges, EXCLUDING the site root (the base_url, which is never an
// orphan even with no recorded inlinks). Ordered importance DESC (ties by url),
// limited to `limit` (<= 0 → no limit). A page that has never been a target is
// an orphan only once the graph has any edges at all is NOT a precondition here:
// a page with no inbound rows is reported regardless — the cold-start guard for
// the page_orphaned *signal* lives in internal/linkgraph (the 1+→0 transition),
// not in this inventory read.
func (db *DB) OrphanPages(ctx context.Context, siteID int64, limit int) ([]OrphanPage, error) {
	// Exclude the root by its CANONICAL identity (#5): both urls.url and
	// link_edges.to_url are stored canonical, but base_url is kept verbatim, so the
	// root must be canonicalized here to match the stored homepage row (a base of
	// "https://example.com" excludes the canonical "https://example.com/"). An
	// unknown site yields root == "" — no urls row equals "", so nothing is excluded
	// (a site with no base is degenerate; it simply has no root to spare).
	root, err := db.siteRootURL(ctx, siteID)
	if err != nil {
		return nil, err
	}
	// The LEFT JOIN matches edges on the now-shared canonical keyspace; orphans are
	// the rows with no matching edge.
	q := `
		SELECT u.id, u.url, u.importance
		  FROM urls u
		  LEFT JOIN link_edges e ON e.site_id = u.site_id AND e.to_url = u.url
		 WHERE u.site_id = ?
		   AND u.status_type = 'page'
		   AND u.url <> ?
		   AND e.to_url IS NULL
		 GROUP BY u.id, u.url, u.importance
		 ORDER BY u.importance DESC, u.url ASC`
	args := []any{siteID, root}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list orphan pages (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []OrphanPage
	for rows.Next() {
		var o OrphanPage
		if scanErr := rows.Scan(&o.URLID, &o.URL, &o.Importance); scanErr != nil {
			return nil, fmt.Errorf("scan orphan page: %w", scanErr)
		}
		out = append(out, o)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate orphan pages: %w", rowsErr)
	}
	return out, nil
}

// DepthChange reports one page's click-depth transition across a BFS sweep: its
// prior graph_depth (nil = NULL, never swept / was unreachable) and its new
// finite depth from the root.
type DepthChange struct {
	URLID    int64
	URL      string
	OldDepth *int // nil = was NULL (first sweep / previously unreachable)
	NewDepth int
}

// SweepGraphDepths runs a depth-capped (MaxBFSDepth) shortest-path BFS from the
// site root over link_edges and writes the resulting urls.graph_depth in CHUNKED
// transactions (the DeleteStaleSnapshots precedent — the single writer is never
// held long enough to stall the crawl). It returns one DepthChange per page that
// is now reachable within the cap, carrying its prior depth so the
// click_depth_regression signal layer (internal/linkgraph) can detect a
// worsening (NewDepth - OldDepth >= 2) — a NULL OldDepth never fires (first
// sweep / newly reachable). Pages NOT reachable from the root within the cap are
// left untouched (their graph_depth stays at its prior value, NULL on a first
// sweep) and are not returned.
//
// `chunk` is the write-back batch size (<= 0 → 500). The BFS itself is one
// recursive CTE (modernc.org/sqlite supports WITH RECURSIVE) computing MIN(depth)
// per reachable node from the root; only the write-back is chunked.
func (db *DB) SweepGraphDepths(ctx context.Context, siteID int64, now time.Time, chunk int) ([]DepthChange, error) {
	if chunk <= 0 {
		chunk = 500
	}

	root, err := db.siteRootURL(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, nil // no site / no base_url → nothing to sweep
	}

	// Shortest-path depths via a recursive CTE over link_edges. The root is the
	// urls row whose url == base_url; if it isn't admitted yet, there is nothing
	// to walk from. We anchor on the root's url string (edges' to_url join admitted
	// source ids), expanding to_url → that target's admitted url id → its out-edges.
	//
	// Identity is exact-string: the frontier matches an edge's to_url to an
	// admitted urls.url, so /a and /a/ are distinct nodes (matching the rest of A9).
	depths, err := db.bfsDepths(ctx, siteID, root)
	if err != nil {
		return nil, err
	}

	// Snapshot prior depths for the reachable set so the returned transitions are
	// computed against pre-sweep state, then write the new depths chunked.
	var changes []DepthChange
	batch := make([]depthRow, 0, chunk)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		return db.WriteTx(ctx, func(tx Tx) error {
			for _, p := range batch {
				if _, uerr := tx.ExecContext(ctx,
					`UPDATE urls SET graph_depth = ? WHERE id = ?`, p.depth, p.urlID); uerr != nil {
					return fmt.Errorf("write graph_depth (url=%d): %w", p.urlID, uerr)
				}
			}
			return nil
		})
	}

	for _, d := range depths {
		prior, perr := db.urlGraphDepth(ctx, d.urlID)
		if perr != nil {
			return nil, perr
		}
		changes = append(changes, DepthChange{
			URLID:    d.urlID,
			URL:      d.url,
			OldDepth: prior,
			NewDepth: d.depth,
		})
		batch = append(batch, d)
		if len(batch) >= chunk {
			if ferr := flush(); ferr != nil {
				return nil, ferr
			}
			batch = batch[:0]
		}
	}
	if ferr := flush(); ferr != nil {
		return nil, ferr
	}
	return changes, nil
}

// depthRow is one reachable node's shortest depth from the root.
type depthRow struct {
	urlID int64
	url   string
	depth int
}

// bfsDepths computes the shortest-path depth (root = 0) for every admitted url in
// siteID reachable from `root` within MaxBFSDepth, via a recursive CTE. Diamond
// graphs collapse to the MIN depth per node because the walk is breadth-bounded
// and we take MIN(depth) GROUP BY the node. Edges to non-admitted targets are
// dropped by the join to urls (a node must be admitted to have out-edges).
func (db *DB) bfsDepths(ctx context.Context, siteID int64, root string) ([]depthRow, error) {
	// walk(url, id, depth): start at the root url (depth 0), then for each frontier
	// url follow its admitted source's out-edges to the next url (depth+1), capped
	// at MaxBFSDepth. SQLite's recursive CTE does a breadth-style expansion; we
	// MIN(depth) per node afterwards to get shortest paths even on diamonds/cycles.
	//
	// The recursive term is UNION (NOT UNION ALL): SQLite dedups each (url, depth)
	// row, so the walk relation is bounded by nodes × MaxBFSDepth rather than by the
	// number of distinct PATHS. With UNION ALL a cyclic or densely cross-linked site
	// (e.g. a global nav/footer linking dozens of pages — a near-complete subgraph)
	// makes the path count explode per level and the query never terminates: a
	// self-inflicted, input-driven DoS of the monitoring daemon on ordinary real
	// sites. UNION keeps the frontier bounded; the outer MIN(depth) GROUP BY still
	// yields correct shortest-path depths because differing-length paths produce
	// distinct (url, depth) rows and MIN picks the shortest. (The depth-20 WHERE cap
	// bounds path LENGTH, not path COUNT, so it does NOT save UNION ALL on its own.)
	const q = `
		WITH RECURSIVE walk(url, depth) AS (
		    SELECT ?, 0
		    UNION
		    SELECT e.to_url, w.depth + 1
		      FROM walk w
		      JOIN urls u   ON u.site_id = ? AND u.url = w.url
		      JOIN link_edges e ON e.from_url_id = u.id
		     WHERE w.depth < ?
		)
		SELECT u.id, u.url, MIN(w.depth) AS depth
		  FROM walk w
		  JOIN urls u ON u.site_id = ? AND u.url = w.url
		 GROUP BY u.id, u.url`
	rows, err := db.Read().QueryContext(ctx, q, root, siteID, MaxBFSDepth, siteID)
	if err != nil {
		return nil, fmt.Errorf("bfs depths (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []depthRow
	for rows.Next() {
		var d depthRow
		if scanErr := rows.Scan(&d.urlID, &d.url, &d.depth); scanErr != nil {
			return nil, fmt.Errorf("scan depth row: %w", scanErr)
		}
		out = append(out, d)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate depth rows: %w", rowsErr)
	}
	return out, nil
}

// siteRootURL returns the site's base_url canonicalized to the link-graph keyspace
// (#5), or "" if the site is unknown. base_url is stored verbatim (the value the
// user typed, reused for robots/SSRF/display), but the BFS anchor must match the
// canonical urls.url / link_edges.to_url identities — so a homepage registered as
// "https://example.com" anchors on "https://example.com/", the form its admitted
// row and inbound edges carry.
func (db *DB) siteRootURL(ctx context.Context, siteID int64) (string, error) {
	var base string
	err := db.Read().QueryRowContext(ctx, `SELECT base_url FROM sites WHERE id = ?`, siteID).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read site root (site=%d): %w", siteID, err)
	}
	return canonicalURL(base), nil
}

// urlGraphDepth reads one url's current graph_depth (nil = NULL / not yet swept).
func (db *DB) urlGraphDepth(ctx context.Context, urlID int64) (*int, error) {
	var d sql.NullInt64
	err := db.Read().QueryRowContext(ctx, `SELECT graph_depth FROM urls WHERE id = ?`, urlID).Scan(&d)
	if err != nil {
		return nil, fmt.Errorf("read graph_depth (url=%d): %w", urlID, err)
	}
	if !d.Valid {
		return nil, nil
	}
	v := int(d.Int64)
	return &v, nil
}
