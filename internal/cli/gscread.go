package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// GSC W2 read verbs under `rabbot gsc`: `status <url>` and `performance --url`.
// They are READ-ONLY and read the store DIRECTLY via withStore (the `rabbot report`
// / `rabbot links` idiom) — so they work with the daemon DOWN, unlike the MCP path
// which is daemon-coherent. URL canonicalization happens at the store read boundary
// (store/gsc.go), so the verb passes the raw URL straight through.
//
// The defining behavior of BOTH verbs: ABSENT GSC data is reported honestly, never
// as an error. The URL-inspection quota is bounded (~2000/day/property), so a
// monitored URL may simply have no inspection / no metrics on record yet — Rabbot
// prints a clear "no GSC data on record" line (or has_status=false / has_data=false
// in --json), NEVER a guess and never a non-zero exit.

const defaultGSCPerfWindow = 168 * time.Hour // 7 days

// newGSCStatusCmd builds `rabbot gsc status <url>`: the latest stored GSC index
// status for one URL (verdict / coverage / indexing / canonical / last crawl).
func newGSCStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status <url>",
		Short: "Latest Google index status for a URL (read-only)",
		Long: "Show the latest Google Search Console URL-inspection result for one monitored " +
			"URL: Google's verdict, coverage/indexing state, robots & fetch state, the canonical " +
			"Google chose vs the one you declared, and the last crawl time. Read-only; works with " +
			"the daemon down. A URL with no inspection on record (the inspection quota is bounded) " +
			"prints an honest \"no inspection on record\" line, never an error. Pipe --json into " +
			"Claude or jq.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			target := args[0]
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				view, err := loadGSCStatus(ctx, db, target)
				if err != nil {
					return err
				}
				if asJSON {
					return renderGSCStatusJSON(c.OutOrStdout(), view)
				}
				return renderGSCStatusTable(c.OutOrStdout(), view)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit as JSON (for piping to Claude or jq)")
	return cmd
}

// newGSCPerformanceCmd builds `rabbot gsc performance --url <url>`: the stored GSC
// search-performance rows (clicks/impressions/CTR/position per query+day) over a
// window. Only finalized (dataState=final) days are stored.
func newGSCPerformanceCmd() *cobra.Command {
	var target string
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "performance --url <url>",
		Short: "Google search performance for a URL (read-only)",
		Long: "Show the stored Google Search Console search-performance rows for one monitored " +
			"URL: clicks, impressions, CTR, and average position per (query, day), newest first, " +
			"bounded by --since (a Go duration, default 168h = 7 days). Only FINALIZED data is " +
			"stored (dataState=final), so the most recent ~3 days are excluded by design. This is " +
			"raw read-only data, NOT an alert — Rabbot deliberately does not fire standalone " +
			"traffic/ranking-drop alerts. A URL with no metrics on record prints an honest \"no " +
			"search data\" line, never an error. Read-only; works with the daemon down. Pipe " +
			"--json into Claude or jq.",
		RunE: func(c *cobra.Command, _ []string) error {
			if target == "" {
				return fmt.Errorf("--url is required (the monitored URL to report search performance for)")
			}
			d := defaultGSCPerfWindow
			if since != "" {
				parsed, perr := time.ParseDuration(since)
				if perr != nil {
					return fmt.Errorf("invalid --since: %w", perr)
				}
				d = parsed
			}
			sinceT := time.Now().UTC().Add(-d)
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				view, err := loadGSCPerformance(ctx, db, target, sinceT)
				if err != nil {
					return err
				}
				if asJSON {
					return renderGSCPerformanceJSON(c.OutOrStdout(), view)
				}
				return renderGSCPerformanceTable(c.OutOrStdout(), view)
			})
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "the monitored URL to report search performance for (required)")
	cmd.Flags().StringVar(&since, "since", "", "window as a Go duration (e.g. 168h, 720h); default 168h (7 days)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit as JSON (for piping to Claude or jq)")
	return cmd
}

// gscStatusView is the CLI-local view of a URL's index status, so
// `rabbot gsc status <url> --json | claude` works without the daemon. HasStatus
// has NO omitempty so an un-inspected URL serializes has_status=false (honest
// absent data, distinguishable from "field absent").
type gscStatusView struct {
	URL             string `json:"url"`
	HasStatus       bool   `json:"has_status"`
	Verdict         string `json:"verdict,omitempty"`
	CoverageState   string `json:"coverage_state,omitempty"`
	IndexingState   string `json:"indexing_state,omitempty"`
	RobotsTxtState  string `json:"robots_txt_state,omitempty"`
	PageFetchState  string `json:"page_fetch_state,omitempty"`
	GoogleCanonical string `json:"google_canonical,omitempty"`
	UserCanonical   string `json:"user_canonical,omitempty"`
	CrawledAs       string `json:"crawled_as,omitempty"`
	InspectedAt     string `json:"inspected_at,omitempty"`
	LastCrawlTime   string `json:"last_crawl_time,omitempty"`
}

// loadGSCStatus resolves the URL's owning site (by host ownership — GSC may know a
// URL Rabbot never admitted, the linksHook precedent) and reads its latest index
// status. An un-inspected URL (no row, or no owning site) is honest absent data
// (HasStatus=false), NEVER an error.
func loadGSCStatus(ctx context.Context, db *store.DB, target string) (gscStatusView, error) {
	siteID, err := siteIDForURL(ctx, db, target)
	if err != nil {
		// No monitored site owns the URL: report it as un-inspected absent data, not a
		// hard error — the verb is a read that should never fail on "Rabbot doesn't have
		// GSC data for this URL".
		return gscStatusView{URL: target, HasStatus: false}, nil
	}
	st, ok, err := db.LatestURLIndexStatus(ctx, siteID, target)
	if err != nil {
		return gscStatusView{}, err
	}
	if !ok {
		return gscStatusView{URL: target, HasStatus: false}, nil
	}
	return gscStatusView{
		URL:             target,
		HasStatus:       true,
		Verdict:         st.Verdict,
		CoverageState:   st.CoverageState,
		IndexingState:   st.IndexingState,
		RobotsTxtState:  st.RobotsTxtState,
		PageFetchState:  st.PageFetchState,
		GoogleCanonical: st.GoogleCanonical,
		UserCanonical:   st.UserCanonical,
		CrawledAs:       st.CrawledAs,
		InspectedAt:     rfc3339OrEmptyCLI(st.InspectedAt),
		LastCrawlTime:   rfc3339PtrOrEmptyCLI(st.LastCrawlTime),
	}, nil
}

func renderGSCStatusTable(w io.Writer, v gscStatusView) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "url\t%s\n", v.URL)
	if !v.HasStatus {
		_, _ = fmt.Fprintln(tw, "\nno Google inspection on record (the URL-inspection quota is bounded — this URL has not been inspected yet, which is not a problem)")
		return tw.Flush()
	}
	_, _ = fmt.Fprintln(tw, "\nINDEX STATUS")
	_, _ = fmt.Fprintf(tw, "  verdict\t%s\n", gscDash(v.Verdict))
	_, _ = fmt.Fprintf(tw, "  coverage_state\t%s\n", gscDash(v.CoverageState))
	_, _ = fmt.Fprintf(tw, "  indexing_state\t%s\n", gscDash(v.IndexingState))
	_, _ = fmt.Fprintf(tw, "  robots_txt_state\t%s\n", gscDash(v.RobotsTxtState))
	_, _ = fmt.Fprintf(tw, "  page_fetch_state\t%s\n", gscDash(v.PageFetchState))
	_, _ = fmt.Fprintf(tw, "  google_canonical\t%s\n", gscDash(v.GoogleCanonical))
	_, _ = fmt.Fprintf(tw, "  user_canonical\t%s\n", gscDash(v.UserCanonical))
	_, _ = fmt.Fprintf(tw, "  crawled_as\t%s\n", gscDash(v.CrawledAs))
	_, _ = fmt.Fprintf(tw, "  inspected_at\t%s\n", gscDash(v.InspectedAt))
	_, _ = fmt.Fprintf(tw, "  last_crawl_time\t%s\n", gscDash(v.LastCrawlTime))
	return tw.Flush()
}

func renderGSCStatusJSON(w io.Writer, v gscStatusView) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// gscPerformanceView is the CLI-local view of a URL's search metrics over a window.
// HasData has NO omitempty so a URL with no metrics serializes has_data=false.
type gscPerformanceView struct {
	URL     string             `json:"url"`
	HasData bool               `json:"has_data"`
	Since   string             `json:"since"`
	Rows    []gscMetricJSONRow `json:"rows"`
}

type gscMetricJSONRow struct {
	Query       string  `json:"query"`
	Date        string  `json:"date"`
	Clicks      int64   `json:"clicks"`
	Impressions int64   `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// loadGSCPerformance resolves the URL's owning site and reads its stored search
// metrics filtered to date >= since. A URL with no metrics (or no owning site) is
// honest absent data (HasData=false), never an error.
func loadGSCPerformance(ctx context.Context, db *store.DB, target string, since time.Time) (gscPerformanceView, error) {
	view := gscPerformanceView{URL: target, Since: since.UTC().Format(time.RFC3339), Rows: []gscMetricJSONRow{}}
	siteID, err := siteIDForURL(ctx, db, target)
	if err != nil {
		return view, nil
	}
	metrics, err := db.SearchMetricsForURL(ctx, siteID, target, since)
	if err != nil {
		return gscPerformanceView{}, err
	}
	view.HasData = len(metrics) > 0
	for _, m := range metrics {
		view.Rows = append(view.Rows, gscMetricJSONRow{
			Query:       m.Query,
			Date:        m.Date,
			Clicks:      m.Clicks,
			Impressions: m.Impressions,
			CTR:         m.CTR,
			Position:    m.Position,
		})
	}
	return view, nil
}

func renderGSCPerformanceTable(w io.Writer, v gscPerformanceView) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "url\t%s\n", v.URL)
	_, _ = fmt.Fprintf(tw, "since\t%s\t(only finalized days are stored)\n", v.Since)
	if !v.HasData {
		_, _ = fmt.Fprintln(tw, "\nno Google search data on record for this URL/window")
		return tw.Flush()
	}
	_, _ = fmt.Fprintf(tw, "\nSEARCH PERFORMANCE (%d rows, newest first)\n", len(v.Rows))
	_, _ = fmt.Fprintln(tw, "  date\tquery\tclicks\timpressions\tctr\tposition")
	for _, r := range v.Rows {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%.3f\t%.1f\n",
			r.Date, r.Query, r.Clicks, r.Impressions, r.CTR, r.Position)
	}
	return tw.Flush()
}

func renderGSCPerformanceJSON(w io.Writer, v gscPerformanceView) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// gscDash renders an empty string as the em-dash "—" honesty marker (the report.go
// scoreLabel precedent), so a field GSC did not report reads as "—", never blank.
func gscDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
