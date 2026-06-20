package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/spf13/cobra"
)

// issueDetailCell renders an issue's Detail JSON for the operator-facing pull
// surfaces (`rabbot issues`, `rabbot inspect`). The detail is shown VERBATIM —
// raw JSON such as {"measured_px":906,"budget_px":580,"chars":48} — consistent
// with Finding.Detail's documented dual machine/operator purpose; humanizing it
// is a push-surface-only (Slack) open question, out of scope for pull. An empty
// detail, or the "{}" placeholder UpsertIssue writes for detail-less rules,
// renders as an empty string so the column/suffix stays blank rather than
// printing a noise "{}".
func issueDetailCell(detail string) string {
	if detail == "" || detail == "{}" {
		return ""
	}
	return detail
}

// renderIssues writes the issues table (with the trailing DETAIL column) to w.
// Extracted from newIssuesCmd so the rendering is unit-testable against a real
// store, mirroring renderReportTable.
func renderIssues(w io.Writer, issues []model.Issue) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tURL_ID\tRULE\tSTATUS\tSEVERITY\tIMPACT\tDETAIL")
	for _, iss := range issues {
		_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%d\t%s\n",
			iss.ID, iss.URLID, iss.RuleID, iss.Status, iss.Severity, iss.ImpactPoints,
			issueDetailCell(iss.Detail))
	}
	return tw.Flush()
}

// parseIssueStatus maps a CLI --status string onto a model.IssueStatus,
// rejecting anything outside the open|closed|ignored lifecycle so a typo can
// never silently widen (or empty) the result set.
func parseIssueStatus(s string) (model.IssueStatus, error) {
	switch model.IssueStatus(s) {
	case model.IssueOpen, model.IssueClosed, model.IssueIgnored:
		return model.IssueStatus(s), nil
	default:
		return "", fmt.Errorf("invalid --status %q (want one of: open, closed, ignored)", s)
	}
}

func newIssuesCmd() *cobra.Command {
	var openOnly bool
	var siteFilter string
	var statusFilter string
	var segmentFilter string
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List open/resolved/ignored issues (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			f := store.IssueFilter{OpenOnly: openOnly}
			// Validate --status up front so an unknown value fails clearly
			// before any config/DB access.
			if statusFilter != "" {
				status, perr := parseIssueStatus(statusFilter)
				if perr != nil {
					return perr
				}
				f.Status = &status
			}
			return withStore(cmd, func(ctx context.Context, st *store.DB) error {
				if siteFilter != "" {
					site, serr := st.GetSiteByBaseURL(ctx, siteFilter)
					if serr != nil {
						return fmt.Errorf("filter --site %q: %w", siteFilter, serr)
					}
					f.SiteID = &site.ID
				}
				if segmentFilter != "" {
					// An unknown segment name yields an empty result plus a hint
					// listing the known names — never a hard error (a typo should
					// guide, not abort), and never a silently-empty table that
					// looks like a clean run.
					ok, cerr := segmentExists(ctx, st, f.SiteID, segmentFilter)
					if cerr != nil {
						return cerr
					}
					if !ok {
						return unknownSegment(ctx, cmd, st, f.SiteID, segmentFilter)
					}
					f.Segment = &segmentFilter
				}
				issues, err := st.ListIssues(ctx, f)
				if err != nil {
					return err
				}
				return renderIssues(cmd.OutOrStdout(), issues)
			})
		},
	}
	cmd.Flags().BoolVar(&openOnly, "open", false, "show only open issues")
	cmd.Flags().StringVar(&siteFilter, "site", "", "filter by site base URL")
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (open|closed|ignored)")
	cmd.Flags().StringVar(&segmentFilter, "segment", "", "filter by segment name (see `rabbot segments`)")
	return cmd
}

func newIssueCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "issue", Short: "Issue operations"}
	ignore := &cobra.Command{
		Use:   "ignore <id>",
		Short: "Mark an issue ignored",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// ParseInt rejects trailing garbage ("12abc"), which fmt.Sscanf("%d")
			// would silently truncate to 12.
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid issue id %q", args[0])
			}
			return withControlClient(cmd, func(ctx context.Context, client *control.Client) error {
				if err := client.IgnoreIssue(ctx, id); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "issue %d ignored\n", id)
				return err
			})
		},
	}
	cmd.AddCommand(ignore)
	return cmd
}
