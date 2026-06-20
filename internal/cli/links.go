package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// defaultLinksLimit caps the inbound-linker rows `rabbot links <url>` prints by
// default. The blast-radius summary always reports the EXACT totals regardless of
// this limit (store.WhatLinksTo returns the full count), so a small default keeps
// the table readable without hiding the true inlink mass.
const defaultLinksLimit = 20

// newLinksCmd builds `rabbot links`, the A9 link-graph pull surface:
//
//	rabbot links <url> [--limit N] [--json]   — inlinks + blast-radius summary
//	rabbot links --orphans <base-url>          — orphan inventory for a site
//
// Read-only, direct-store via withStore (the `rabbot report` idiom): it works with
// the daemon down. Node identity is EXACT-STRING (fragment-stripped only): /a, /a/,
// and /a?utm=x are three distinct nodes — a documented LITE limitation, not a bug.
func newLinksCmd() *cobra.Command {
	var limit int
	var asJSON bool
	var orphans bool
	cmd := &cobra.Command{
		Use:   "links <url>",
		Short: "Inbound links + blast radius for a URL (or --orphans for a site's orphan inventory)",
		Long: "Answer \"who links this page, and how bad is it going dark?\" from the link " +
			"graph. With --orphans, list a site's monitored pages that have zero internal " +
			"inlinks. Read-only; works with the daemon down. URL identity is exact-string " +
			"(fragment-stripped): /a, /a/, and /a?utm=x are distinct nodes.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if limit <= 0 {
				return fmt.Errorf("--limit must be >= 1")
			}
			target := args[0]
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				if orphans {
					return runLinksOrphans(ctx, c, db, target, limit, asJSON)
				}
				return runLinksFor(ctx, c, db, target, limit, asJSON)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", defaultLinksLimit, "max inbound linkers (or orphan rows) to list")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit as JSON (for piping to Claude or jq)")
	cmd.Flags().BoolVar(&orphans, "orphans", false, "list the site's orphan inventory; the positional arg is the site BASE URL")
	return cmd
}

// runLinksFor resolves the target URL's owning site (by base-URL prefix is NOT how
// we resolve — A9 takes the target URL and finds its site via the urls row), then
// builds the blast-radius card. A target that the graph has never seen is reported
// as zero inlinks (data, not an error) so the operator learns the page is currently
// an island.
func runLinksFor(ctx context.Context, c *cobra.Command, db *store.DB, target string, limit int, asJSON bool) error {
	siteID, err := siteIDForURL(ctx, db, target)
	if err != nil {
		return err
	}
	g := linkgraph.NewGrapher(db)
	card, err := g.BlastRadiusCard(ctx, siteID, target, limit)
	if err != nil {
		return err
	}
	if asJSON {
		return renderLinksJSON(c.OutOrStdout(), card)
	}
	return renderLinksTable(c.OutOrStdout(), card)
}

// runLinksOrphans resolves the SITE by base URL (the positional arg in --orphans
// mode) and lists its orphan inventory.
func runLinksOrphans(ctx context.Context, c *cobra.Command, db *store.DB, baseURL string, limit int, asJSON bool) error {
	site, err := db.GetSiteByBaseURL(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("site %q: %w", baseURL, err)
	}
	g := linkgraph.NewGrapher(db)
	pages, err := g.Orphans(ctx, site.ID, limit)
	if err != nil {
		return err
	}
	if asJSON {
		return renderOrphansJSON(c.OutOrStdout(), baseURL, pages)
	}
	return renderOrphansTable(c.OutOrStdout(), baseURL, pages)
}

// siteIDForURL finds the site that owns target by matching its admitted urls row.
// A target that was never admitted (linked but uncrawled) has no urls row, so we
// fall back to matching the target against each site's base URL prefix. When no
// site is found at all, that is a clear caller error (the URL is for a site Rabbot
// does not monitor).
func siteIDForURL(ctx context.Context, db *store.DB, target string) (int64, error) {
	sites, err := db.ListSites(ctx)
	if err != nil {
		return 0, err
	}
	for _, s := range sites {
		if u, gerr := db.GetURL(ctx, s.ID, target); gerr == nil {
			return u.SiteID, nil
		}
	}
	// Fall back to base-URL ownership so a never-admitted (but in-scope) target still
	// resolves to its site — its blast radius is then "0 inlinks" (an island), which
	// is honest data rather than a not-found error.
	for _, s := range sites {
		if urlBelongsToSite(target, s.BaseURL) {
			return s.ID, nil
		}
	}
	return 0, fmt.Errorf("no monitored site owns %q (run `rabbot sites` to see monitored sites)", target)
}

// urlBelongsToSite reports whether target is the site base URL or sits under it
// at a PATH boundary. This is only the fallback ownership check for a
// never-admitted target; identity elsewhere stays exact-string. The boundary
// check matters: a bare prefix test would let base "https://example.com" falsely
// own "https://example.com.attacker.com/", so target must either equal the base
// or extend it after a "/" separator (the trailing slash on the base, if any, is
// normalized away first so both base forms behave identically).
func urlBelongsToSite(target, baseURL string) bool {
	if target == baseURL {
		return true
	}
	rest, ok := strings.CutPrefix(target, strings.TrimSuffix(baseURL, "/"))
	return ok && strings.HasPrefix(rest, "/")
}

func renderLinksTable(w io.Writer, card linkgraph.Card) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "url\t%s\n", card.URL)
	_, _ = fmt.Fprintln(tw, "\nBLAST RADIUS")
	_, _ = fmt.Fprintf(tw, "  inlinks %d\thigh-importance %d\tweighted %.2f\n",
		card.Inlinks, card.HighImportance, card.WeightedInlinks)
	if len(card.Linkers) > 0 {
		_, _ = fmt.Fprintf(tw, "\nTOP LINKERS (showing %d of %d)\n", len(card.Linkers), card.Inlinks)
		for _, l := range card.Linkers {
			_, _ = fmt.Fprintf(tw, "  %.2f\t%s\n", l.Importance, l.URL)
		}
	} else {
		_, _ = fmt.Fprintln(tw, "\nno inbound internal links (this page is an island)")
	}
	return tw.Flush()
}

// linksJSON is the CLI-local JSON view of a blast-radius card, so
// `rabbot links <url> --json | claude` works without the daemon.
type linksJSON struct {
	URL             string        `json:"url"`
	Inlinks         int           `json:"inlinks"`
	HighImportance  int           `json:"high_importance"`
	WeightedInlinks float64       `json:"weighted_inlinks"`
	Linkers         []linkJSONRow `json:"linkers"`
}

type linkJSONRow struct {
	URLID      int64   `json:"url_id"`
	URL        string  `json:"url"`
	Importance float64 `json:"importance"`
}

func renderLinksJSON(w io.Writer, card linkgraph.Card) error {
	v := linksJSON{
		URL:             card.URL,
		Inlinks:         card.Inlinks,
		HighImportance:  card.HighImportance,
		WeightedInlinks: card.WeightedInlinks,
		Linkers:         make([]linkJSONRow, 0, len(card.Linkers)),
	}
	for _, l := range card.Linkers {
		v.Linkers = append(v.Linkers, linkJSONRow{URLID: l.URLID, URL: l.URL, Importance: l.Importance})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func renderOrphansTable(w io.Writer, baseURL string, pages []store.OrphanPage) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "site\t%s\n", baseURL)
	_, _ = fmt.Fprintf(tw, "\nORPHANS (%d)\n", len(pages))
	if len(pages) == 0 {
		_, _ = fmt.Fprintln(tw, "  none — every monitored page has at least one internal inlink")
		return tw.Flush()
	}
	for _, p := range pages {
		_, _ = fmt.Fprintf(tw, "  %.2f\t%s\n", p.Importance, p.URL)
	}
	return tw.Flush()
}

// orphansJSON is the CLI-local JSON view of a site's orphan inventory.
type orphansJSON struct {
	Site    string          `json:"site"`
	Count   int             `json:"count"`
	Orphans []orphanJSONRow `json:"orphans"`
}

type orphanJSONRow struct {
	URLID      int64   `json:"url_id"`
	URL        string  `json:"url"`
	Importance float64 `json:"importance"`
}

func renderOrphansJSON(w io.Writer, baseURL string, pages []store.OrphanPage) error {
	v := orphansJSON{Site: baseURL, Count: len(pages), Orphans: make([]orphanJSONRow, 0, len(pages))}
	for _, p := range pages {
		v.Orphans = append(v.Orphans, orphanJSONRow{URLID: p.URLID, URL: p.URL, Importance: p.Importance})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
