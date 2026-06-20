package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/spf13/cobra"
)

// newSitemapCmd builds the `rabbot sitemap` parent command. It has no run
// function of its own: a bare `rabbot sitemap` prints subcommand help (cobra's
// default for a parent with no Run). The `coverage` subcommand is the A2 pull
// surface — a read-only reconciliation of a site's declared sitemap against the
// crawled inventory.
func newSitemapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sitemap",
		Short: "Sitemap watching and coverage reconciliation (read-only)",
		Long: "Inspect how a site's declared sitemap reconciles against the crawled " +
			"inventory. Use the `coverage` subcommand to see drift between what the " +
			"site declares and what Rabbot has actually fetched.",
	}
	cmd.AddCommand(newSitemapCoverageCmd())
	return cmd
}

// newSitemapCoverageCmd builds `rabbot sitemap coverage [--site <id|url>] [--json]`.
// It reads the store directly (read-only, via withStore) — the same pattern as
// `rabbot report` — so it works without the daemon.
func newSitemapCoverageCmd() *cobra.Command {
	var siteRef string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Sitemap-vs-crawl coverage drift for a site (read-only)",
		Long: "Reconcile a site's declared sitemap set against the crawled inventory: " +
			"how many declared URLs are uncrawled, how many were never admitted (page-cap " +
			"exhaustion / rejects), and how many crawled URLs are absent from the sitemap. " +
			"Emits structured facts; pipe --json into Claude or jq.",
		RunE: func(c *cobra.Command, _ []string) error {
			if siteRef == "" {
				return fmt.Errorf("--site is required (a site id or base URL)")
			}
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				siteID, err := resolveSiteRef(ctx, db, siteRef)
				if err != nil {
					return err
				}
				res, err := db.SitemapCoverage(ctx, siteID)
				if err != nil {
					return err
				}
				if asJSON {
					return renderCoverageJSON(c.OutOrStdout(), res)
				}
				return renderCoverageTable(c.OutOrStdout(), res, siteID)
			})
		},
	}
	cmd.Flags().StringVar(&siteRef, "site", "", "the site to report on, by numeric id or base URL (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the coverage report as JSON (for piping to Claude or jq)")
	return cmd
}

// resolveSiteRef resolves a --site reference that may be either a numeric site id
// or a base URL into the site's id. A purely-numeric ref is treated as an id; any
// other ref is looked up by base URL. An unknown site is reported as an error.
func resolveSiteRef(ctx context.Context, db *store.DB, ref string) (int64, error) {
	if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		site, err := db.GetSite(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("site %d: %w", id, err)
		}
		return site.ID, nil
	}
	site, err := db.GetSiteByBaseURL(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("site %q: %w", ref, err)
	}
	return site.ID, nil
}

// renderCoverageTable renders the coverage result as an aligned text table. All
// four counts (the three drift buckets + the seed HTTP status) plus bounded
// sample URLs per bucket are shown. A site without a watched sitemap renders an
// explicit "no sitemap" notice rather than bare zeros.
func renderCoverageTable(w io.Writer, res store.SitemapCoverageResult, siteID int64) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "SITEMAP COVERAGE\tsite %d\n", siteID)
	if !res.HasSitemap {
		_, _ = fmt.Fprintln(tw, "  no sitemap watched yet for this site (nothing to reconcile)")
		return tw.Flush()
	}
	_, _ = fmt.Fprintf(tw, "  seed status\t%d\n", res.SeedStatus)
	_, _ = fmt.Fprintf(tw, "  sitemapped, uncrawled\t%d\n", res.SitemappedUncrawled)
	_, _ = fmt.Fprintf(tw, "  sitemapped, unadmitted\t%d\n", res.SitemappedUnadmitted)
	_, _ = fmt.Fprintf(tw, "  crawled, not in sitemap\t%d\n", res.CrawledNotInSitemap)
	if len(res.SampleUncrawled) > 0 {
		_, _ = fmt.Fprintln(tw, "\nSAMPLE: SITEMAPPED, UNCRAWLED")
		for _, u := range res.SampleUncrawled {
			_, _ = fmt.Fprintf(tw, "  %s\n", u)
		}
	}
	if len(res.SampleNotInSitemap) > 0 {
		_, _ = fmt.Fprintln(tw, "\nSAMPLE: CRAWLED, NOT IN SITEMAP")
		for _, u := range res.SampleNotInSitemap {
			_, _ = fmt.Fprintf(tw, "  %s\n", u)
		}
	}
	return tw.Flush()
}

// renderCoverageJSON emits the coverage result as indented JSON. The store result
// carries the canonical JSON tags (has_sitemap, seed_status, the three count
// fields, the two sample lists) that are JSON-identical to the control + mcp DTOs,
// so the CLI re-uses it directly rather than re-declaring a wire shape. Nil sample
// slices serialize as null; that is acceptable for the read surface and matches
// the store zero value.
func renderCoverageJSON(w io.Writer, res store.SitemapCoverageResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
