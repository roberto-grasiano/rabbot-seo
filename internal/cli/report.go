package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/spf13/cobra"
)

const defaultReportWindow = 168 * time.Hour // 7 days

func newReportCmd() *cobra.Command {
	var since string
	var siteFilter string
	var segmentFilter string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Windowed cross-site activity digest (read-only)",
		Long: "Summarise change and issue activity across all monitored sites (or one " +
			"site) over a time window. Emits structured facts; pipe --json into Claude or jq.",
		RunE: func(c *cobra.Command, _ []string) error {
			d := defaultReportWindow
			if since != "" {
				parsed, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("invalid --since: %w", err)
				}
				d = parsed
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be >= 1")
			}
			until := time.Now().UTC()
			sinceT := until.Add(-d)

			return withStore(c, func(ctx context.Context, db *store.DB) error {
				var siteID *int64
				if siteFilter != "" {
					site, serr := db.GetSiteByBaseURL(ctx, siteFilter)
					if serr != nil {
						return fmt.Errorf("filter --site %q: %w", siteFilter, serr)
					}
					siteID = &site.ID
				}
				params := store.ReportParams{Since: sinceT, SiteID: siteID, TopN: limit}
				if segmentFilter != "" {
					// Unknown segment name → empty digest + a hint listing known
					// names (mirrors `rabbot issues`), never a hard error.
					ok, cerr := segmentExists(ctx, db, siteID, segmentFilter)
					if cerr != nil {
						return cerr
					}
					if !ok {
						return unknownSegment(ctx, c, db, siteID, segmentFilter)
					}
					params.Segment = &segmentFilter
				}
				res, err := db.BuildReport(ctx, params)
				if err != nil {
					return err
				}
				if asJSON {
					return renderReportJSON(c.OutOrStdout(), res, sinceT, until, siteID)
				}
				return renderReportTable(c.OutOrStdout(), res, sinceT, until, siteID)
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "window as a Go duration (e.g. 24h, 168h); default 168h (7 days)")
	cmd.Flags().StringVar(&siteFilter, "site", "", "scope to one site by base URL (default: all sites)")
	cmd.Flags().StringVar(&segmentFilter, "segment", "", "scope to one segment by name (see `rabbot segments`)")
	cmd.Flags().IntVar(&limit, "limit", 10, "max number of top changed URLs")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the digest as JSON (for piping to Claude or jq)")
	return cmd
}

func scopeLabel(siteID *int64) string {
	if siteID == nil {
		return "all sites"
	}
	return fmt.Sprintf("site %d", *siteID)
}

// scoreLabel renders a ScopeHealth's score as a one-decimal number, or the em-dash
// "—" when the scope is undefined (below the coverage floor or no page mass). It
// NEVER prints a fake 100/0 for an undefined scope — the em-dash is the honest
// "not enough crawl coverage to score" signal (A6).
func scoreLabel(h store.ScopeHealth) string {
	if !h.Defined {
		return "—"
	}
	return fmt.Sprintf("%.1f", h.Score)
}

// deltaLabel renders an optional signed delta, or "—" when it is unavailable (no
// window-start point or an undefined current score).
func deltaLabel(d *float64) string {
	if d == nil {
		return "—"
	}
	return fmt.Sprintf("%+.1f", *d)
}

func renderReportTable(w io.Writer, res store.ReportResult, since, until time.Time, siteID *int64) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "window\t%s → %s\t(%s)\n",
		since.Format(time.RFC3339), until.Format(time.RFC3339), scopeLabel(siteID))
	_, _ = fmt.Fprintln(tw, "\nCHANGES")
	_, _ = fmt.Fprintf(tw, "  total %d\tsubstantive %d\tcosmetic %d\n", res.Changes.Total, res.Changes.Substantive, res.Changes.Cosmetic)
	_, _ = fmt.Fprintln(tw, "\nISSUES")
	_, _ = fmt.Fprintf(tw, "  open now %d\t(critical %d, warning %d, info %d)\n", res.Issues.OpenTotal, res.Issues.OpenCritical, res.Issues.OpenWarning, res.Issues.OpenInfo)
	_, _ = fmt.Fprintf(tw, "  opened %d\tclosed %d\n", res.Issues.OpenedInWindow, res.Issues.ClosedInWindow)
	if res.Health != nil {
		renderHealthSection(tw, res.Health)
	}
	if len(res.TopURLs) > 0 {
		_, _ = fmt.Fprintln(tw, "\nTOP CHANGED URLS")
		for _, u := range res.TopURLs {
			_, _ = fmt.Fprintf(tw, "  %d\t%s\tlast %s\n", u.Count, u.URL, u.LastChanged.UTC().Format(time.RFC3339))
		}
	}
	if len(res.Sites) > 0 {
		_, _ = fmt.Fprintln(tw, "\nPER-SITE")
		for _, s := range res.Sites {
			_, _ = fmt.Fprintf(tw, "  %s\tchanges %d\topen-issues %d\thealth %s\n", s.BaseURL, s.Changes, s.OpenIssues, scoreLabel(s.Health))
		}
	}
	return tw.Flush()
}

// renderHealthSection writes the site-scoped HEALTH block: the live score and its
// window delta, the per-segment scores, and the top contributing rules. An undefined
// scope renders "—" (never a fake number).
func renderHealthSection(tw io.Writer, h *store.HealthBlock) {
	_, _ = fmt.Fprintln(tw, "\nHEALTH")
	_, _ = fmt.Fprintf(tw, "  score %s\tΔ %s\t(coverage %d/%d processed)\n",
		scoreLabel(h.Current), deltaLabel(h.Delta), h.Current.ProcessedURLs, h.Current.KnownURLs)
	if len(h.Segments) > 0 {
		_, _ = fmt.Fprintln(tw, "  by segment")
		for _, s := range h.Segments {
			_, _ = fmt.Fprintf(tw, "    %s\t%s\n", s.Name, scoreLabel(s.ScopeHealth))
		}
	}
	if len(h.TopRules) > 0 {
		_, _ = fmt.Fprintln(tw, "  top contributors")
		for _, r := range h.TopRules {
			_, _ = fmt.Fprintf(tw, "    %s\t%d\n", r.RuleID, r.Mass)
		}
	}
}

// reportJSON is the CLI-local JSON view: store types with RFC3339 timestamps and an
// echoed window/scope, so `rabbot report --json | claude` works without the daemon.
type reportJSON struct {
	Since   string              `json:"since"`
	Until   string              `json:"until"`
	SiteID  *int64              `json:"site_id,omitempty"`
	Changes store.ChangeSummary `json:"changes"`
	Issues  store.IssueSummary  `json:"issues"`
	TopURLs []reportJSONTopURL  `json:"top_urls,omitempty"`
	Sites   []store.SiteRollup  `json:"sites,omitempty"`
	// Health is the site-scoped health block (A6); omitted for an all-sites report,
	// where each rollup row carries its own per-site score instead.
	Health *store.HealthBlock `json:"health,omitempty"`
}

type reportJSONTopURL struct {
	URLID       int64  `json:"url_id"`
	URL         string `json:"url"`
	Count       int    `json:"count"`
	LastChanged string `json:"last_changed"`
}

func renderReportJSON(w io.Writer, res store.ReportResult, since, until time.Time, siteID *int64) error {
	v := reportJSON{
		Since:   since.Format(time.RFC3339),
		Until:   until.Format(time.RFC3339),
		SiteID:  siteID,
		Changes: res.Changes,
		Issues:  res.Issues,
		Sites:   res.Sites,
		Health:  res.Health,
	}
	for _, u := range res.TopURLs {
		v.TopURLs = append(v.TopURLs, reportJSONTopURL{URLID: u.URLID, URL: u.URL, Count: u.Count, LastChanged: u.LastChanged.UTC().Format(time.RFC3339)})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
