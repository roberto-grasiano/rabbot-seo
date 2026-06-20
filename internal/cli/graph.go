package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// newGraphCmd builds `rabbot graph`, the A9 bounded link-graph export:
//
//	rabbot graph <base-url> [--focus <url>] [--hops N] [--json]
//
// With --focus it exports the ≤2-hop in+out neighborhood of one URL (node/edge
// caps enforced server-side, hard ceilings regardless of config). Without --focus
// it exports the segment- (or folder-) aggregated overview. Read-only, direct-store
// via withStore — works daemon-down. Rabbot emits JSON only; the agent draws (the
// "ask Claude to draw your site" recipe): pipe `--json` into Claude and ask for a
// Mermaid/HTML render.
func newGraphCmd() *cobra.Command {
	var focus string
	var hops int
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "graph <base-url>",
		Short: "Bounded link-graph export (focus neighborhood or segment/folder overview)",
		Long: "Export a bounded slice of a site's internal link graph as JSON — a ≤2-hop " +
			"focus neighborhood (--focus) or a segment/folder overview. Caps keep the " +
			"payload to tens of KB. Rabbot never renders; pipe --json into Claude and ask " +
			"it to draw the graph.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// Reject hops > 2 clearly at the CLI layer (criterion 8) so the operator
			// gets the message before any store work. The export layer enforces the
			// same ceiling; this is the friendly front-door check.
			if hops > linkgraph.MaxFocusHops {
				return fmt.Errorf("--hops must be <= %d (got %d)", linkgraph.MaxFocusHops, hops)
			}
			if hops < 0 {
				return fmt.Errorf("--hops must be >= 0 (got %d)", hops)
			}
			baseURL := args[0]
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				site, err := db.GetSiteByBaseURL(ctx, baseURL)
				if err != nil {
					return fmt.Errorf("site %q: %w", baseURL, err)
				}
				// Thread the configured export caps through the Grapher so the CLI
				// honors the same bounds as the daemon's MCP/control surfaces.
				cfg, cerr := loadConfig(c)
				if cerr != nil {
					return cerr
				}
				g := graphGrapher(db, cfg)
				exp, err := g.Export(ctx, linkgraph.Query{
					SiteID: site.ID,
					Focus:  focus,
					Hops:   hops,
					Limit:  limit,
				})
				if err != nil {
					return err
				}
				if asJSON {
					return renderGraphJSON(c.OutOrStdout(), exp)
				}
				return renderGraphTable(c.OutOrStdout(), exp)
			})
		},
	}
	cmd.Flags().StringVar(&focus, "focus", "", "focus-mode anchor URL (omit for the site overview)")
	cmd.Flags().IntVar(&hops, "hops", 0, "focus neighborhood radius (0 = full ≤2-hop; max 2)")
	cmd.Flags().IntVar(&limit, "limit", 0, "optional node-cap override (downward only; never above the hard ceiling)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the export as JSON (Rabbot never renders — pipe to Claude to draw)")
	return cmd
}

// graphGrapher builds a Grapher with the export caps sourced from config so the CLI
// and the daemon agree on the bounds (the hard ceilings still apply on top).
func graphGrapher(db *store.DB, cfg *config.Config) *linkgraph.Grapher {
	return linkgraph.NewGrapher(db,
		linkgraph.WithMaxOutlinks(cfg.Graph.MaxOutlinksPerPage),
		linkgraph.WithExportCaps(cfg.Graph.ExportMaxNodes, cfg.Graph.ExportMaxEdges),
	)
}

func renderGraphTable(w io.Writer, exp linkgraph.Export) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "mode\t%s\n", exp.Mode)
	switch exp.Mode {
	case linkgraph.ModeFocus:
		_, _ = fmt.Fprintf(tw, "focus\t%s\thops %d\n", exp.Focus, exp.Hops)
		_, _ = fmt.Fprintf(tw, "nodes\t%d of %d\tedges %d of %d\ttruncated %t\n",
			len(exp.Nodes), exp.TotalNodes, len(exp.Edges), exp.TotalEdges, exp.Truncated)
		if len(exp.Nodes) > 0 {
			_, _ = fmt.Fprintln(tw, "\nNODES")
			for _, n := range exp.Nodes {
				_, _ = fmt.Fprintf(tw, "  %.2f\t%s\t%s\n", n.Importance, n.URL, admittedLabel(n.Admitted))
			}
		}
		if len(exp.Edges) > 0 {
			_, _ = fmt.Fprintln(tw, "\nEDGES")
			for _, e := range exp.Edges {
				_, _ = fmt.Fprintf(tw, "  %s\t->\t%s\n", e.From, e.To)
			}
		}
	case linkgraph.ModeOverview:
		_, _ = fmt.Fprintf(tw, "grouping\t%s\n", exp.Grouping)
		_, _ = fmt.Fprintf(tw, "groups\t%d\tedges %d\ttruncated %t\n",
			exp.TotalNodes, exp.TotalEdges, exp.Truncated)
		if len(exp.Groups) > 0 {
			_, _ = fmt.Fprintln(tw, "\nGROUPS")
			for _, gr := range exp.Groups {
				_, _ = fmt.Fprintf(tw, "  %s\n", gr.Name)
			}
		}
		if len(exp.GroupEdges) > 0 {
			_, _ = fmt.Fprintln(tw, "\nGROUP EDGES")
			for _, ge := range exp.GroupEdges {
				_, _ = fmt.Fprintf(tw, "  %s\t->\t%s\tweight %d\n", ge.From, ge.To, ge.Weight)
			}
		}
	}
	return tw.Flush()
}

func admittedLabel(admitted bool) string {
	if admitted {
		return "(crawled)"
	}
	return "(linked, not yet crawled)"
}

// renderGraphJSON emits the export struct verbatim — the agent consumes this to
// draw the graph. The struct's json tags ARE the wire shape (shared with the
// control/MCP surfaces), so we encode it directly.
func renderGraphJSON(w io.Writer, exp linkgraph.Export) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(exp)
}
