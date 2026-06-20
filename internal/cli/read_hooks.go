package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/richresult"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// resolveVerificationTier is the canonical resolver of a site's LIVE throttle tier
// from the proof record, shared by every control read surface so they all agree:
// a never-verified site (no proof row -> ErrNotFound) or an empty state reads back
// as "throttled" (the safe default); a genuine DB error degrades to "".
func resolveVerificationTier(ctx context.Context, db *store.DB, siteID int64) string {
	rec, err := db.GetVerification(ctx, siteID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return string(verify.StateThrottled)
		}
		return ""
	}
	if rec.State == "" {
		return string(verify.StateThrottled)
	}
	return string(rec.State)
}

// listSitesHook backs GET /v1/sites: list sites + resolve each site's live tier.
func listSitesHook(db *store.DB) func(ctx context.Context) ([]control.SiteSummary, error) {
	return func(ctx context.Context) ([]control.SiteSummary, error) {
		sites, err := db.ListSites(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]control.SiteSummary, 0, len(sites))
		for _, s := range sites {
			out = append(out, control.SiteSummary{
				ID:                s.ID,
				URL:               s.BaseURL,
				Name:              s.Name,
				Enabled:           s.Enabled,
				VerificationState: resolveVerificationTier(ctx, db, s.ID),
			})
		}
		return out, nil
	}
}

// capResolver returns the resolved per-site page cap (0 = unlimited) for a site
// id. It is injected (not closed over a fixed *config.Config) because the cap
// lives in config.discovery.max_pages_per_site, which is mutated across SIGHUP
// reloads under cfgMu — the production resolver snapshots live config under the
// lock (mirrors discoveryResolver in run.go). Tests pass a trivial closure.
type capResolver func(ctx context.Context, db *store.DB, siteID int64) int

// siteDetailHook backs GET /v1/sites/{id}/detail. An unknown id returns found=false
// (the handler renders structured not-found); a missing homepage snapshot leaves
// HasSnapshot=false and omits the snapshot fields.
func siteDetailHook(db *store.DB, resolveCap capResolver) func(ctx context.Context, id int64) (control.SiteDetailResponse, bool, error) {
	return func(ctx context.Context, id int64) (control.SiteDetailResponse, bool, error) {
		site, err := db.GetSite(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.SiteDetailResponse{}, false, nil
			}
			return control.SiteDetailResponse{}, false, err
		}

		resp := control.SiteDetailResponse{
			ID:                site.ID,
			URL:               site.BaseURL,
			Name:              site.Name,
			Enabled:           site.Enabled,
			VerificationState: resolveVerificationTier(ctx, db, site.ID),
			MinInterval:       site.MinInterval,
			MaxInterval:       site.MaxInterval,
		}

		// Verification method/timestamps from the proof record (best-effort).
		if rec, verr := db.GetVerification(ctx, site.ID); verr == nil {
			resp.VerificationMethod = string(rec.Method)
			resp.VerifiedAt = rfc3339OrEmptyCLI(rec.VerifiedAt)
			resp.LastReverifiedAt = rfc3339OrEmptyCLI(rec.LastReverifiedAt)
		}

		// Open-issue count (best-effort; a query error must not fail the whole detail).
		if issues, ierr := db.ListIssues(ctx, store.IssueFilter{SiteID: &site.ID, OpenOnly: true}); ierr == nil {
			resp.OpenIssueCount = len(issues)
		}

		// Homepage representative snapshot. Resolve the homepage URL row first; a
		// missing URL or snapshot is "no snapshot yet", not an error.
		if u, uerr := db.GetURL(ctx, site.ID, site.BaseURL); uerr == nil {
			if snap, serr := db.LatestSnapshot(ctx, u.ID); serr == nil {
				resp.HasSnapshot = true
				resp.Title = snap.Title
				resp.MetaDescription = snap.MetaDescription
				resp.Canonical = snap.Canonical
				resp.Indexable = snap.Indexable
				resp.IndexabilityReason = snap.IndexabilityReason
				resp.HTTPStatus = snap.HTTPStatus
				resp.FetchedAt = rfc3339OrEmptyCLI(snap.FetchedAt)
			}
		}

		// Page-cap visibility (Phase 3, computed at read time — no migration).
		// MonitoredPages is the inventory count; MaxPages is the resolved cap
		// (0 = unlimited). A CountSiteURLs error degrades to 0 (detail must not
		// fail just because the count query hiccuped).
		if n, cerr := db.CountSiteURLs(ctx, site.ID); cerr == nil {
			resp.MonitoredPages = n
		}
		resp.MaxPages = resolveCap(ctx, db, site.ID)
		resp.Capped = pageCapped(resp.MonitoredPages, resp.MaxPages)

		// Configured segments (name, match, live member count) so an agent can
		// discover the filterable names. Normalize to a non-nil slice so the JSON is
		// [] not null; a query error degrades to the empty list (detail must not fail
		// just because the segment query hiccuped).
		resp.Segments = []control.SegmentSummary{}
		if segs, segErr := db.ListSegments(ctx, &site.ID); segErr == nil {
			for _, s := range segs {
				resp.Segments = append(resp.Segments, control.SegmentSummary{
					Name:        s.Name,
					Match:       s.MatchRule,
					MemberCount: s.MemberCount,
				})
			}
		}

		return resp, true, nil
	}
}

// validSeverity reports whether s is a known SEO severity (empty = no filter).
func validSeverity(s string) bool {
	switch model.Severity(s) {
	case model.SeverityCritical, model.SeverityWarning, model.SeverityInfo:
		return true
	}
	return false
}

// validIssueStatus reports whether s is a known issue lifecycle state (empty = no filter).
func validIssueStatus(s string) bool {
	switch model.IssueStatus(s) {
	case model.IssueOpen, model.IssueClosed, model.IssueIgnored:
		return true
	}
	return false
}

// issuesHook backs GET /v1/issues. Invalid severity/status enums are caller faults
// -> control.ErrBadRequest (HTTP 400). An empty status with no other filter lists all.
func issuesHook(db *store.DB) func(ctx context.Context, q control.IssueQuery) ([]control.IssueView, error) {
	return func(ctx context.Context, q control.IssueQuery) ([]control.IssueView, error) {
		filter := store.IssueFilter{SiteID: q.SiteID}
		if q.Severity != "" {
			if !validSeverity(q.Severity) {
				return nil, fmt.Errorf("invalid severity %q: %w", q.Severity, control.ErrBadRequest)
			}
			sev := model.Severity(q.Severity)
			filter.Severity = &sev
		}
		if q.Status != "" {
			if !validIssueStatus(q.Status) {
				return nil, fmt.Errorf("invalid status %q: %w", q.Status, control.ErrBadRequest)
			}
			st := model.IssueStatus(q.Status)
			filter.Status = &st
		}
		if q.Segment != "" {
			// An unknown segment name is NOT a caller fault: the join simply matches
			// no rows, so the result is empty data (the spec's "bad segment value
			// degrades to empty, never a transport error"). No validation here.
			seg := q.Segment
			filter.Segment = &seg
		}
		issues, err := db.ListIssues(ctx, filter)
		if err != nil {
			return nil, err
		}
		out := make([]control.IssueView, 0, len(issues))
		for _, iss := range issues {
			out = append(out, control.IssueView{
				ID:           iss.ID,
				URLID:        iss.URLID,
				RuleID:       iss.RuleID,
				Status:       string(iss.Status),
				Severity:     string(iss.Severity),
				ImpactPoints: iss.ImpactPoints,
				Detail:       iss.Detail,
				OpenedAt:     rfc3339OrEmptyCLI(iss.OpenedAt),
				LastSeenAt:   rfc3339OrEmptyCLI(iss.LastSeenAt),
			})
		}
		return out, nil
	}
}

// historyHook backs GET /v1/history. It resolves the URL string to a row id via
// GetURL(ctx, 0, url) (match by URL only, mirroring inspect.go), then reads the
// change log. An unknown URL is structured not-found (NotFound=true), not an error.
func historyHook(db *store.DB) func(ctx context.Context, pageURL string, since time.Time) (control.HistoryResponse, error) {
	return func(ctx context.Context, pageURL string, since time.Time) (control.HistoryResponse, error) {
		u, err := db.GetURL(ctx, 0, pageURL)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.HistoryResponse{URL: pageURL, NotFound: true}, nil
			}
			return control.HistoryResponse{}, err
		}
		changes, err := db.GetURLHistory(ctx, u.ID, since)
		if err != nil {
			return control.HistoryResponse{}, err
		}
		out := control.HistoryResponse{URL: pageURL, Changes: make([]control.ChangeView, 0, len(changes))}
		for _, ch := range changes {
			out.Changes = append(out.Changes, control.ChangeView{
				Field:       ch.Field,
				OldValue:    ch.OldValue,
				NewValue:    ch.NewValue,
				ChangeClass: string(ch.ChangeClass),
				DetectedAt:  rfc3339OrEmptyCLI(ch.DetectedAt),
			})
		}
		return out, nil
	}
}

// richResultsHook backs GET /v1/rich-results. It resolves the URL string to a row
// id via GetURL(ctx, 0, url) (match by URL only, mirroring historyHook/inspect),
// reads the latest snapshot, and validates its JSON-LD against the in-binary
// rich-result profile (richresult.GRR202606). An unknown URL is structured
// not-found (NotFound=true, HTTP 200), mirroring historyHook — NOT a 404. A
// monitored-but-uncrawled URL returns HasSnapshot=false with the profile version
// still set, so the surface is honest about which profile would have applied.
func richResultsHook(db *store.DB) func(ctx context.Context, pageURL string) (control.RichResultsResponse, error) {
	return func(ctx context.Context, pageURL string) (control.RichResultsResponse, error) {
		u, err := db.GetURL(ctx, 0, pageURL)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.RichResultsResponse{URL: pageURL, NotFound: true}, nil
			}
			return control.RichResultsResponse{}, err
		}

		// Entities is normalized to a non-nil slice so the JSON is [] not null.
		resp := control.RichResultsResponse{
			URL:      pageURL,
			Profile:  richresult.GRR202606.Version,
			Entities: []control.RichResultEntity{},
		}

		snap, serr := db.LatestSnapshot(ctx, u.ID)
		if errors.Is(serr, store.ErrNotFound) {
			// Monitored but never crawled: no snapshot, but the URL is real, so this
			// is NOT not-found — HasSnapshot stays false, profile stays set.
			return resp, nil
		}
		if serr != nil {
			return control.RichResultsResponse{}, serr
		}

		resp.HasSnapshot = true
		rep := richresult.Validate(snap.JSONLD, richresult.GRR202606)
		resp.Unprofiled = rep.Unprofiled
		for _, e := range rep.Entities {
			resp.Entities = append(resp.Entities, control.RichResultEntity{
				Type:         e.Type,
				RawType:      e.RawType,
				Eligible:     e.Eligible,
				Missing:      e.Missing,
				MissingAnyOf: e.MissingAnyOf,
			})
		}
		return resp, nil
	}
}

// reportHook backs GET /v1/report. It runs store.BuildReport and maps the result
// onto the control wire DTO. The handler stamps Since/Until/SiteID; this hook fills
// only the data fields (so it stays clock-free and deterministic).
func reportHook(db *store.DB) func(ctx context.Context, since time.Time, siteID *int64, top int, segment *string) (control.ReportResponse, error) {
	return func(ctx context.Context, since time.Time, siteID *int64, top int, segment *string) (control.ReportResponse, error) {
		res, err := db.BuildReport(ctx, store.ReportParams{Since: since, SiteID: siteID, TopN: top, Segment: segment})
		if err != nil {
			return control.ReportResponse{}, err
		}
		resp := control.ReportResponse{
			Changes: control.ReportChangeSummary{Total: res.Changes.Total, Substantive: res.Changes.Substantive, Cosmetic: res.Changes.Cosmetic},
			Issues: control.ReportIssueSummary{
				OpenTotal: res.Issues.OpenTotal, OpenCritical: res.Issues.OpenCritical,
				OpenWarning: res.Issues.OpenWarning, OpenInfo: res.Issues.OpenInfo,
				OpenedInWindow: res.Issues.OpenedInWindow, ClosedInWindow: res.Issues.ClosedInWindow,
			},
		}
		for _, u := range res.TopURLs {
			resp.TopURLs = append(resp.TopURLs, control.ReportURLChange{
				URLID: u.URLID, URL: u.URL, Count: u.Count, LastChanged: rfc3339OrEmptyCLI(u.LastChanged),
			})
		}
		for _, s := range res.Sites {
			resp.Sites = append(resp.Sites, control.ReportSiteRollup{
				SiteID: s.SiteID, BaseURL: s.BaseURL, Changes: s.Changes, OpenIssues: s.OpenIssues,
			})
		}
		return resp, nil
	}
}

// coverageHook backs GET /v1/coverage. It confirms the site exists (unknown id
// -> found=false -> HTTP 404), then runs store.SitemapCoverage and maps the
// result onto the control wire DTO. A site that exists but has no watched sitemap
// snapshot returns found=true with HasSitemap=false (the zero coverage block) —
// "site known, sitemap not watched yet" is data, not a 404.
func coverageHook(db *store.DB) func(ctx context.Context, siteID int64) (control.CoverageResponse, bool, error) {
	return func(ctx context.Context, siteID int64) (control.CoverageResponse, bool, error) {
		if _, err := db.GetSite(ctx, siteID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.CoverageResponse{}, false, nil
			}
			return control.CoverageResponse{}, false, err
		}
		res, err := db.SitemapCoverage(ctx, siteID)
		if err != nil {
			return control.CoverageResponse{}, false, err
		}
		return control.CoverageResponse{
			HasSitemap:           res.HasSitemap,
			SeedStatus:           res.SeedStatus,
			SitemappedUncrawled:  res.SitemappedUncrawled,
			SitemappedUnadmitted: res.SitemappedUnadmitted,
			CrawledNotInSitemap:  res.CrawledNotInSitemap,
			SampleUncrawled:      res.SampleUncrawled,
			SampleNotInSitemap:   res.SampleNotInSitemap,
		}, true, nil
	}
}

// scoreHook backs GET /v1/score. It confirms the site exists (unknown id ->
// found=false -> NotFoundResponse, the errors-as-data SiteDetail pattern), resolves
// an optional segment NAME to its id (unknown name -> found=false too), computes the
// LIVE health score for that scope (store.ComputeHealthScore — so an ignore reflects
// immediately), and reads the persisted trend (store.HealthScoreSeries) bounded by
// `since`. computed_at is emitted RFC3339 (UTC). The current score is always live;
// the series is the persisted step function.
func scoreHook(db *store.DB) func(ctx context.Context, siteID int64, segment string, since time.Time) (control.ScoreResponse, bool, error) {
	return func(ctx context.Context, siteID int64, segment string, since time.Time) (control.ScoreResponse, bool, error) {
		if _, err := db.GetSite(ctx, siteID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.ScoreResponse{}, false, nil
			}
			return control.ScoreResponse{}, false, err
		}

		// Resolve an optional segment name to its id within this site. An unknown name
		// is errors-as-data (found=false), the same not-found shape as an unknown site.
		var segID *int64
		if segment != "" {
			id, ok, err := segmentIDByName(ctx, db, siteID, segment)
			if err != nil {
				return control.ScoreResponse{}, false, err
			}
			if !ok {
				return control.ScoreResponse{}, false, nil
			}
			segID = &id
		}

		hs, err := db.ComputeHealthScore(ctx, siteID, segID)
		if err != nil {
			return control.ScoreResponse{}, false, err
		}
		series, err := db.HealthScoreSeries(ctx, siteID, segID, since)
		if err != nil {
			return control.ScoreResponse{}, false, err
		}

		resp := control.ScoreResponse{
			SiteID:        siteID,
			Segment:       segment,
			SegmentID:     segID,
			Defined:       hs.Defined,
			Score:         hs.Score,
			ImpactMass:    hs.ImpactMass,
			MaxMass:       hs.MaxMass,
			KnownURLs:     hs.KnownURLs,
			ProcessedURLs: hs.ProcessedURLs,
			PageCount:     hs.ProcessedURLs,
			OpenCritical:  hs.OpenCritical,
			OpenWarning:   hs.OpenWarning,
			OpenInfo:      hs.OpenInfo,
			Breakdown:     hs.Breakdown,
			Series:        make([]control.ScorePoint, 0, len(series)),
		}
		for _, p := range series {
			resp.Series = append(resp.Series, control.ScorePoint{
				ComputedAt:   rfc3339OrEmptyCLI(p.ComputedAt),
				Score:        p.Score,
				ImpactMass:   p.ImpactMass,
				MaxMass:      p.MaxMass,
				PageCount:    p.PageCount,
				OpenCritical: p.OpenCritical,
				OpenWarning:  p.OpenWarning,
				OpenInfo:     p.OpenInfo,
			})
		}
		return resp, true, nil
	}
}

// linksHook backs GET /v1/links?url=&limit= (A9). It resolves the URL's owning
// site, then builds the blast-radius card (inlink count + high-importance count +
// weighted inlink mass + ranked inbound linkers, exact totals). A URL that is not a
// monitored URL of any site is reported as data (NotFound=true, HTTP 200) — the
// History not-found pattern, NOT a 404. Node identity is exact-string. The Grapher
// is sink-less (read-only); `cfg` threads the export/out-degree caps for parity,
// though the links read does not itself cap.
func linksHook(db *store.DB, cfg config.GraphConfig) func(ctx context.Context, pageURL string, limit int) (control.LinksResponse, error) {
	return func(ctx context.Context, pageURL string, limit int) (control.LinksResponse, error) {
		// Resolve the URL across all sites to learn its site_id (edges are keyed by
		// (site_id, to_url)). An unknown URL is not-found-as-data.
		u, err := db.GetURL(ctx, 0, pageURL)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.LinksResponse{URL: pageURL, NotFound: true}, nil
			}
			return control.LinksResponse{}, err
		}
		g := linkgraph.NewGrapher(db,
			linkgraph.WithMaxOutlinks(cfg.MaxOutlinksPerPage),
			linkgraph.WithExportCaps(cfg.ExportMaxNodes, cfg.ExportMaxEdges),
		)
		card, err := g.BlastRadiusCard(ctx, u.SiteID, pageURL, limit)
		if err != nil {
			return control.LinksResponse{}, err
		}
		resp := control.LinksResponse{
			URL:             card.URL,
			Inlinks:         card.Inlinks,
			InlinkTotal:     card.Inlinks,
			HighImportance:  card.HighImportance,
			WeightedInlinks: card.WeightedInlinks,
			Linkers:         make([]control.LinkerView, 0, len(card.Linkers)),
		}
		for _, l := range card.Linkers {
			resp.Linkers = append(resp.Linkers, control.LinkerView{URLID: l.URLID, URL: l.URL, Importance: l.Importance})
		}
		return resp, nil
	}
}

// graphHook backs GET /v1/graph?site_id=&focus=&hops=&mode= (A9). It confirms the
// site exists (unknown id -> found=false -> NotFoundResponse, HTTP 200, the
// errors-as-data SiteDetail pattern), then runs the bounded export. A caller fault
// from the export (focus-mode without a focus URL, an out-of-range hops the handler
// did not already reject, an unknown mode) is wrapped as control.ErrBadRequest so
// the handler returns HTTP 400 rather than 500. The Grapher is sink-less; `cfg`
// threads the same export caps the CLI and daemon use (the hard ceilings still
// apply on top).
func graphHook(db *store.DB, cfg config.GraphConfig) func(ctx context.Context, q control.GraphQuery) (control.GraphResponse, bool, error) {
	return func(ctx context.Context, q control.GraphQuery) (control.GraphResponse, bool, error) {
		if _, err := db.GetSite(ctx, q.SiteID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return control.GraphResponse{}, false, nil
			}
			return control.GraphResponse{}, false, err
		}
		g := linkgraph.NewGrapher(db,
			linkgraph.WithMaxOutlinks(cfg.MaxOutlinksPerPage),
			linkgraph.WithExportCaps(cfg.ExportMaxNodes, cfg.ExportMaxEdges),
		)
		exp, err := g.Export(ctx, linkgraph.Query{
			SiteID: q.SiteID,
			Mode:   linkgraph.ExportMode(q.Mode),
			Focus:  q.Focus,
			Hops:   q.Hops,
			Limit:  q.Limit,
		})
		if err != nil {
			// Export returns plain validation errors for caller faults (bad hops/mode,
			// focus without a url); surface them as 400 via the ErrBadRequest wrap. Both
			// verbs are %w (Go 1.20+ multi-wrap) so errors.Is(err, control.ErrBadRequest)
			// holds and the underlying message is preserved (errorlint-clean).
			return control.GraphResponse{}, false, fmt.Errorf("%w: %w", control.ErrBadRequest, err)
		}
		resp := control.GraphResponse{
			Mode:       string(exp.Mode),
			Focus:      exp.Focus,
			Hops:       exp.Hops,
			Grouping:   exp.Grouping,
			Truncated:  exp.Truncated,
			TotalNodes: exp.TotalNodes,
			TotalEdges: exp.TotalEdges,
		}
		for _, n := range exp.Nodes {
			resp.Nodes = append(resp.Nodes, control.GraphNodeView{
				URL: n.URL, Admitted: n.Admitted, Importance: n.Importance,
				GraphDepth: n.GraphDepth, InSitemap: n.InSitemap, LastFetchClass: n.LastFetchClass,
			})
		}
		for _, e := range exp.Edges {
			resp.Edges = append(resp.Edges, control.GraphEdgeView{From: e.From, To: e.To})
		}
		for _, gr := range exp.Groups {
			resp.Groups = append(resp.Groups, control.GraphGroupView{Name: gr.Name})
		}
		for _, ge := range exp.GroupEdges {
			resp.GroupEdges = append(resp.GroupEdges, control.GraphGroupEdgeView{From: ge.From, To: ge.To, Weight: ge.Weight})
		}
		return resp, true, nil
	}
}

// indexStatusHook backs GET /v1/index-status?url= (GSC W2). It resolves the URL's
// owning site (GetURL(ctx, 0, url), the historyHook pattern), reads the latest
// stored url_index_status, and maps it onto the control wire DTO. An un-inspected
// URL — either the URL is not a monitored row, OR it is but has no inspection on
// record (LatestURLIndexStatus ok=false) — is reported as DATA (NotFound=true /
// HasStatus=false, HTTP 200), the RichResults not-found pattern, NEVER a 404 and
// NEVER a discrepancy. That absent-data honesty is the quota-bounded-staleness
// guard: the inspection quota is bounded, so a missing row means "no GSC data on
// record", not "noindex". Timestamps are emitted RFC3339 (UTC); a nil last-crawl
// reads back as "" (omitted).
func indexStatusHook(db *store.DB) func(ctx context.Context, pageURL string) (control.IndexStatusResponse, error) {
	return func(ctx context.Context, pageURL string) (control.IndexStatusResponse, error) {
		// Resolve the URL's owning site by host ownership, NOT by an admitted urls row:
		// GSC inspects URLs Google knows about that Rabbot may never have crawled (the
		// url_index_status.url column is TEXT, not a urls FK), so requiring a urls row
		// would hide real GSC data. siteIDForURL tries the urls row first, then falls
		// back to base-URL ownership; a URL no monitored site owns is absent-data, not 404.
		siteID, err := siteIDForURL(ctx, db, pageURL)
		if err != nil {
			return control.IndexStatusResponse{URL: pageURL, NotFound: true}, nil
		}
		st, ok, err := db.LatestURLIndexStatus(ctx, siteID, pageURL)
		if err != nil {
			return control.IndexStatusResponse{}, err
		}
		if !ok {
			// The single most important correctness invariant: no inspection on record
			// (quota-bounded) is absent data, never a discrepancy.
			return control.IndexStatusResponse{URL: pageURL, NotFound: true}, nil
		}
		return control.IndexStatusResponse{
			URL:             pageURL,
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
}

// searchPerformanceHook backs GET /v1/search-performance?url=&since= (GSC W2). It
// resolves the URL's owning site (GetURL(ctx, 0, url)), then reads the stored
// search_metrics rows filtered to date >= since. since is an RFC3339 string ("" =
// all stored history); a malformed since is a caller fault (the handler validates
// it first, but the hook is defensive). The stored data is already dataState=final
// (the puller only persists finalized days). A URL with no metrics — unknown URL OR
// known-but-empty — is reported as DATA (HasData=false, empty Rows), never a 404 or
// error. Rows are newest-first (store.SearchMetricsForURL orders date DESC).
func searchPerformanceHook(db *store.DB) func(ctx context.Context, pageURL, since string) (control.SearchPerformanceResponse, error) {
	return func(ctx context.Context, pageURL, since string) (control.SearchPerformanceResponse, error) {
		var sinceT time.Time
		if since != "" {
			t, perr := time.Parse(time.RFC3339, since)
			if perr != nil {
				return control.SearchPerformanceResponse{}, fmt.Errorf("invalid since %q: want RFC3339: %w", since, control.ErrBadRequest)
			}
			sinceT = t
		}
		// Resolve the owning site by host ownership (not an admitted urls row): search
		// metrics, like index status, can reference a URL Google knows but Rabbot has
		// not crawled. A URL no monitored site owns is honest empty data, not a 404.
		siteID, err := siteIDForURL(ctx, db, pageURL)
		if err != nil {
			return control.SearchPerformanceResponse{URL: pageURL, Rows: []control.SearchMetricView{}}, nil
		}
		metrics, err := db.SearchMetricsForURL(ctx, siteID, pageURL, sinceT)
		if err != nil {
			return control.SearchPerformanceResponse{}, err
		}
		resp := control.SearchPerformanceResponse{
			URL:     pageURL,
			HasData: len(metrics) > 0,
			Rows:    make([]control.SearchMetricView, 0, len(metrics)),
		}
		for _, m := range metrics {
			resp.Rows = append(resp.Rows, control.SearchMetricView{
				Query:       m.Query,
				Date:        m.Date,
				Clicks:      m.Clicks,
				Impressions: m.Impressions,
				CTR:         m.CTR,
				Position:    m.Position,
			})
		}
		return resp, nil
	}
}

// segmentIDByName resolves a segment NAME to its id within siteID. ok is false when
// no segment with that name exists for the site (errors-as-data, not an error).
// Point lookup via the idx_segments_site_name unique index (PR #76 review nit).
func segmentIDByName(ctx context.Context, db *store.DB, siteID int64, name string) (int64, bool, error) {
	return db.SegmentIDByName(ctx, siteID, name)
}

// rfc3339PtrOrEmptyCLI formats an optional time as RFC3339 (UTC), returning "" for a
// nil pointer (Google reported no last-crawl instant) so omitempty drops the field.
func rfc3339PtrOrEmptyCLI(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339OrEmptyCLI(*t)
}

// rfc3339OrEmptyCLI formats a time as RFC3339 (UTC), returning "" for the zero
// value so omitempty drops the JSON field. (Mirrors the control DTO formatting but
// stays CLI-local so the closures don't reach into control internals.)
func rfc3339OrEmptyCLI(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
