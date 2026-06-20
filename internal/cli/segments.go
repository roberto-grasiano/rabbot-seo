package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/spf13/cobra"
)

// newSegmentsCmd lists configured segments per site with their match pattern and
// live member count. Read-only and direct-store (the settled CLI idiom): it works
// daemon-down. Models on newReportCmd — a --site scope and a --json toggle that
// switch between the table and JSON renderers.
func newSegmentsCmd() *cobra.Command {
	var siteFilter string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "segments",
		Short: "List configured URL segments and their member counts (read-only)",
		Long: "List the named URL segments configured per site, each with its match " +
			"pattern and live member-URL count. Emits structured facts; pipe --json into Claude or jq.",
		RunE: func(c *cobra.Command, _ []string) error {
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				var siteID *int64
				if siteFilter != "" {
					site, serr := db.GetSiteByBaseURL(ctx, siteFilter)
					if serr != nil {
						return fmt.Errorf("filter --site %q: %w", siteFilter, serr)
					}
					siteID = &site.ID
				}
				return runSegments(ctx, db, c.OutOrStdout(), siteID, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&siteFilter, "site", "", "scope to one site by base URL (default: all sites)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the listing as JSON (for piping to Claude or jq)")
	return cmd
}

// runSegments fetches the segment listing for siteID (nil = all sites) and writes
// it to w in table or JSON form. Extracted from newSegmentsCmd so the rendering is
// unit-testable against a real store, mirroring runInspect.
func runSegments(ctx context.Context, db *store.DB, w io.Writer, siteID *int64, asJSON bool) error {
	segs, err := db.ListSegments(ctx, siteID)
	if err != nil {
		return err
	}
	if asJSON {
		return renderSegmentsJSON(w, segs)
	}
	return renderSegmentsTable(w, segs)
}

// segmentJSON is the CLI-local JSON view of one segment, snake_case and
// JSON-identical to the mcp.SiteDetail segments shape so a `rabbot segments --json`
// payload and a get_site detail agree on field names.
type segmentJSON struct {
	SiteID      int64  `json:"site_id"`
	Name        string `json:"name"`
	Match       string `json:"match"`
	MemberCount int    `json:"member_count"`
}

func renderSegmentsJSON(w io.Writer, segs []store.SegmentWithCount) error {
	out := make([]segmentJSON, 0, len(segs))
	for _, s := range segs {
		out = append(out, segmentJSON{SiteID: s.SiteID, Name: s.Name, Match: s.MatchRule, MemberCount: s.MemberCount})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderSegmentsTable(w io.Writer, segs []store.SegmentWithCount) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SITE_ID\tNAME\tPATTERN\tMEMBERS")
	for _, s := range segs {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%d\n", s.SiteID, s.Name, s.MatchRule, s.MemberCount)
	}
	return tw.Flush()
}

// segmentExists reports whether a segment named `name` exists (within siteID when
// non-nil, else across all sites). Used by the --segment filter on `rabbot issues`
// and `rabbot report` to distinguish "no members" from "no such segment" so an
// unknown name can surface a hint instead of a silently empty result.
func segmentExists(ctx context.Context, db *store.DB, siteID *int64, name string) (bool, error) {
	segs, err := db.ListSegments(ctx, siteID)
	if err != nil {
		return false, err
	}
	for _, s := range segs {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// unknownSegment handles an unknown --segment value on a pull command: it writes
// a hint listing the known names to stderr (so it never pollutes a --json stdout
// stream that gets piped) and returns nil — the caller produces an empty result,
// not an error. The empty result is intentional: a typo'd segment yields nothing,
// and the hint tells the operator what they could have typed instead.
func unknownSegment(ctx context.Context, cmd *cobra.Command, db *store.DB, siteID *int64, name string) error {
	hint, herr := segmentHint(ctx, db, siteID)
	if herr != nil {
		return herr
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "unknown segment %q; %s\n", name, hint)
	return nil
}

// segmentHint returns a one-line operator hint listing the known segment names
// (within siteID when non-nil, else across all sites), de-duplicated and sorted,
// so an unknown --segment value tells the operator what they could have typed.
func segmentHint(ctx context.Context, db *store.DB, siteID *int64) (string, error) {
	segs, err := db.ListSegments(ctx, siteID)
	if err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(segs))
	names := make([]string, 0, len(segs))
	for _, s := range segs {
		if _, dup := seen[s.Name]; dup {
			continue
		}
		seen[s.Name] = struct{}{}
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "no segments are configured", nil
	}
	return "known segments: " + strings.Join(names, ", "), nil
}
