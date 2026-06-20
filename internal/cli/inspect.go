package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/richresult"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// errWriter wraps an io.Writer and remembers the first write error.
// All subsequent writes are no-ops once an error is recorded, so the
// caller only needs to check ew.err at the end instead of after every line.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, a ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, a...)
}

func (ew *errWriter) println(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintln(ew.w, s)
}

// runInspect renders everything the daemon knows about one URL: its schedule
// state, the latest snapshot's SEO surface, open issues, and recent changes.
// Read-only — it queries the store directly.
func runInspect(ctx context.Context, db *store.DB, w io.Writer, rawURL string) error {
	u, err := db.GetURL(ctx, 0, rawURL)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("not monitored: %s", rawURL)
		}
		return err
	}

	ew := &errWriter{w: w}
	ew.printf("URL:          %s\n", u.URL)
	ew.printf("Site ID:      %d\n", u.SiteID)
	// Verification tier (spec §11): the operator-facing verified/throttled state of
	// the owning site, read from the authoritative DB proof record. A never-verified
	// site reads back StateThrottled (the migration DEFAULT). A read error degrades
	// to "(unknown)" rather than failing the whole inspect.
	ew.printf("Verification: %s\n", verificationTier(ctx, db, u.SiteID))
	ew.printf("Last checked: %s\n", fmtTimePtr(u.LastChecked))
	ew.printf("Next check:   %s\n", fmtTime(u.NextCheckAt))
	ew.printf("Last result:  %s\n", u.LastFetchClass)
	ew.printf("Importance:   %.2f  depth=%d  in_sitemap=%t\n", u.Importance, u.Depth, u.InSitemap)

	snap, snapErr := db.LatestSnapshot(ctx, u.ID)
	switch {
	case errors.Is(snapErr, store.ErrNotFound):
		ew.println("\nSnapshot:     (none yet — not crawled)")
	case snapErr != nil:
		return snapErr
	default:
		ew.println("\nLatest snapshot:")
		ew.printf("  fetched_at:   %s\n", fmtTime(snap.FetchedAt))
		ew.printf("  http_status:  %d  (%d ms)\n", snap.HTTPStatus, snap.ResponseTimeMS)
		ew.printf("  title:        %q\n", snap.Title)
		ew.printf("  meta_desc:    %q\n", snap.MetaDescription)
		ew.printf("  canonical:    %s\n", snap.Canonical)
		ew.printf("  headings:     %s\n", snap.Headings)
		ew.printf("  indexable:    %t  (%s)\n", snap.Indexable, snap.IndexabilityReason)
		ew.printf("  schema_types: %s\n", snap.SchemaTypes)
		ew.printf("  links:        %d internal / %d external\n", snap.InternalLinkCount, snap.ExternalLinkCount)
		ew.printf("  images:       %d  (%d missing alt)\n", snap.ImageCount, snap.MissingAltCount)
		ew.printf("  word_count:   %d\n", snap.WordCount)
		// A8 (pull surface): how the page delivers its SEO content (server_rendered /
		// hydrated / head_only_shell / client_shell), plus the extraction provenance
		// (e.g. dom, dom+next_data). A pre-A8 snapshot stored an empty render_mode
		// (migration DEFAULT '') which reads back as "unknown" — renderModeLabel maps
		// the zero value so the surface is honest rather than blank.
		ew.printf("  render_mode:  %s  (source: %s)\n", renderModeLabel(snap.RenderMode), extractionSourceLabel(snap.ExtractionSource))
		// A4 (pull surface): validate the snapshot's JSON-LD against the in-binary
		// rich-result profile and render per-type eligibility. Presence-driven: a
		// profiled type gets an eligible/ineligible verdict (with the missing
		// properties named); implemented-but-unprofiled types get only a neutral
		// count, never a verdict or a recommendation to add markup.
		renderRichResults(ew, richresult.Validate(snap.JSONLD, richresult.GRR202606))
	}
	if ew.err != nil {
		return ew.err
	}

	open, err := db.ListIssues(ctx, store.IssueFilter{URLID: &u.ID, OpenOnly: true})
	if err != nil {
		return err
	}
	ew.printf("\nOpen issues:  %d\n", len(open))
	for _, iss := range open {
		// A3: append the issue's raw detail JSON (e.g. measured/budget px) when
		// present, so the pull surface carries the numbers verbatim. A detail-less
		// issue ("{}"/empty) gets no trailing suffix — issueDetailCell returns "".
		if d := issueDetailCell(iss.Detail); d != "" {
			ew.printf("  - %-24s %-8s %s  %s\n", iss.RuleID, iss.Severity, iss.Status, d)
		} else {
			ew.printf("  - %-24s %-8s %s\n", iss.RuleID, iss.Severity, iss.Status)
		}
	}

	changes, err := db.GetURLHistory(ctx, u.ID, time.Time{})
	if err != nil {
		return err
	}
	ew.printf("\nRecent changes: %d\n", len(changes))
	for i, ch := range changes {
		if i >= 10 {
			ew.printf("  … %d more\n", len(changes)-10)
			break
		}
		ew.printf("  %s  %s  %q -> %q  [%s]\n",
			fmtTime(ch.DetectedAt), ch.Field, ch.OldValue, ch.NewValue, ch.ChangeClass)
	}
	return ew.err
}

// renderRichResults prints the "Rich results:" section for one validated
// snapshot. Each profiled entity gets an eligible/ineligible line (the canonical
// type, its raw @type when it differs via an alias, and the missing properties
// named on an ineligible verdict). Implemented-but-unprofiled entities are
// summarised as a single neutral count — never an eligibility verdict and never a
// recommendation (presence-driven). A snapshot with neither profiled entities nor
// unprofiled types still prints the section header + the profile version so the
// surface is honest about having looked.
func renderRichResults(ew *errWriter, rep richresult.Report) {
	ew.printf("\nRich results:  (profile %s)\n", rep.Profile)
	for _, e := range rep.Entities {
		label := e.Type
		if e.RawType != "" && e.RawType != e.Type {
			label = fmt.Sprintf("%s (%s)", e.Type, e.RawType)
		}
		if e.Eligible {
			ew.printf("  - %-28s eligible\n", label)
			continue
		}
		ew.printf("  - %-28s ineligible%s\n", label, richResultMissingSuffix(e))
	}
	if rep.Unprofiled > 0 {
		ew.printf("  %d unprofiled type(s) — not validated in this profile\n", rep.Unprofiled)
	}
	if len(rep.Entities) == 0 && rep.Unprofiled == 0 {
		ew.println("  (no rich-result markup detected)")
	}
}

// richResultMissingSuffix builds the " — missing: a, b | one-of: x|y" tail for an
// ineligible entity, naming the absent Required properties and the unsatisfied
// any-of groups so the operator sees exactly what regressed.
func richResultMissingSuffix(e richresult.EntityResult) string {
	var parts []string
	if len(e.Missing) > 0 {
		parts = append(parts, "missing: "+strings.Join(e.Missing, ", "))
	}
	for _, group := range e.MissingAnyOf {
		parts = append(parts, "one-of: "+strings.Join(group, "|"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, "; ")
}

// verificationTier renders the owning site's living verification state for the
// inspect view: "verified", "throttled", or "attested". It reads the
// authoritative DB proof record and MIRRORS verificationState (the throttle
// decision): a never-verified site (no proof row -> store.ErrNotFound) and an
// empty state both read back as "throttled", the safe default that matches the
// migration DEFAULT — so the operator display never diverges from the effective
// throttle tier. "(unknown)" is reserved for a GENUINE (non-not-found) store
// error so a transient DB glitch never fails inspect yet stays distinguishable
// from a legitimately never-verified site. The bare State string is the
// operator-facing tier (spec §11): verified => full speed, anything else =>
// throttled crawl budget.
func verificationTier(ctx context.Context, db *store.DB, siteID int64) string {
	rec, err := db.GetVerification(ctx, siteID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "throttled" // never verified — mirrors verificationState
		}
		return "(unknown)" // a genuine DB error, not a missing proof row
	}
	if rec.State == "" {
		return "throttled"
	}
	return string(rec.State)
}

// renderModeLabel maps a snapshot's persisted render_mode to its operator-facing
// label (A8). The zero value (model.RenderMode("")) — written by pre-A8 rows via
// the migration DEFAULT ” and by snapshots taken with hydration extraction
// disabled — reads back as "unknown" rather than an empty string, so the inspect
// line is never blank. Every populated value is already a human-readable token
// (server_rendered / hydrated / head_only_shell / client_shell / unknown), so it
// passes through verbatim.
func renderModeLabel(rm model.RenderMode) string {
	if rm == "" {
		return string(model.RenderUnknown)
	}
	return string(rm)
}

// extractionSourceLabel renders the extraction provenance (e.g. "dom",
// "dom+next_data") for the inspect surface. An empty source — pre-A8 rows and
// hydration-disabled snapshots — shows "dom", the implicit baseline (extraction
// always reads the DOM), so the source column is never blank.
func extractionSourceLabel(src string) string {
	if src == "" {
		return "dom"
	}
	return src
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <url>",
		Short: "Show what the daemon knows about a URL (snapshot, issues, changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				return runInspect(ctx, db, c.OutOrStdout(), args[0])
			})
		},
	}
}
