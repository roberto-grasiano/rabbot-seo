package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// GSC W1 scheduler half: the side-timer puller. PLUMBING ONLY — it PULLS Google
// Search Console ground truth (searchAnalytics.query + urlInspection.index.inspect)
// and STORES it via the store repo. It deliberately builds NO signals/rules/alerts:
// index_status_discrepancy / google_canonical_mismatch / search_performance_shift
// are W2, which READS the rows this puller writes.
//
// The puller mirrors SideTimers: it owns only narrow interfaces (so it is fully
// unit-testable against mocks with no live daemon and no real DB), takes an
// injectable clock (Now) for the dataState=final date math, and is built with a
// nil-safe severability seam — a daemon with no GSC-configured site simply does not
// construct one, and registerGSCPull returns (false, nil).

// Tuning defaults. They are puller-internal (not config knobs in W1); a W2 surface
// may expose poll cadence / budget later.
const (
	// gscPartialDataLagDays is how many trailing days searchAnalytics treats as
	// partial/incomplete. dataState=final already excludes them server-side; we
	// also pin the window's end behind this lag so the requested range is the
	// finalized data the spec alerts on (the latest ~3 days are partial).
	gscPartialDataLagDays = 3
	// gscDefaultLookbackDays is the default width of the search-analytics window
	// (a week of finalized days) when LookbackDays is unset.
	gscDefaultLookbackDays = 7
	// gscDefaultDailyInspectBudget is the default per-site URL-inspection budget
	// per pass. GSC's URL-Inspection quota is ~2000/day/property; the daily job
	// spends at most this many inspections, prioritized by importance, and never
	// blocks the crawl.
	gscDefaultDailyInspectBudget = 200
)

// gscSearchDimensions is the (date, page, query) grouping the puller requests. The
// store grain is (site_id, url, query, date); requesting `date` as a dimension makes
// the API return one row PER calendar day (not a window aggregate), so each stored
// row carries its true day and a re-pull idempotently corrects a day's partial→final
// metrics on the (site,url,query,date) upsert. Keys are positional: [0]=date,
// [1]=page, [2]=query.
var gscSearchDimensions = []string{"date", "page", "query"}

// GSCClient is the subset of the internal/gsc client the puller uses. The
// production *gsc.Client satisfies it; tests substitute a mock or a real client
// pointed at an httptest server.
type GSCClient interface {
	SearchAnalyticsQuery(ctx context.Context, property string, req gsc.SearchAnalyticsRequest) (*gsc.SearchAnalyticsResponse, error)
	InspectURL(ctx context.Context, property, inspectionURL string) (*gsc.InspectResponse, error)
}

// SearchMetricsStore persists searchAnalytics rows. *store.DB satisfies it.
type SearchMetricsStore interface {
	SaveSearchMetrics(ctx context.Context, metrics []model.SearchMetric) error
}

// URLIndexStore persists a URL's latest index status. *store.DB satisfies it.
type URLIndexStore interface {
	UpsertURLIndexStatus(ctx context.Context, s model.URLIndexStatus) error
}

// InspectCandidate is one URL nominated for an index-inspection, with the
// importance used to prioritize the bounded daily budget.
type InspectCandidate struct {
	URL        string
	Importance float64
}

// URLCandidateSource yields up to `limit` URLs to inspect for a site, highest
// priority first (changed + high-importance). The production adapter lives in the
// cli wiring (it reads urls ordered by importance via the store's Read accessor);
// tests substitute a fixed list. W1 keeps the selection simple — the signal/rule
// logic that consumes the stored statuses is W2.
type URLCandidateSource interface {
	InspectionCandidates(ctx context.Context, siteID int64, limit int) ([]InspectCandidate, error)
}

// GSCPuller runs the per-site GSC pulls on a schedule. It is constructed in the
// daemon beside SideTimers and registered as a gocron job (registerGSCPull). A nil
// *GSCPuller is a no-op the registration treats as "GSC disabled".
type GSCPuller struct {
	// ResolveGSC maps a site's BaseURL to its per-site GSCConfig and whether that
	// site has an ACTIVE GSC block (model.Site carries no GSC field, so this is the
	// bridge). It is a FUNCTION, not a raw *config.Config, so the production wiring
	// can guard the read against live config reload with the daemon's cfg mutex
	// (the perHostUserAgentFunc precedent); tests pass a closure over a fixed config.
	// A nil resolver means "no site is GSC-configured" — every Pull is a no-op.
	ResolveGSC func(baseURL string) (config.GSCConfig, bool)
	// Metrics / Index are the store write sinks.
	Metrics SearchMetricsStore
	Index   URLIndexStore
	// Candidates selects the URLs to inspect under the daily budget.
	Candidates URLCandidateSource
	// API builds a GSC client from a token provider. Injected so tests substitute a
	// mock/httptest client; production wraps gsc.NewClient.
	API func(gsc.TokenProvider) (GSCClient, error)
	// ProviderForSite builds the token provider for a site's GSC config — reading
	// the 0600 credential file and constructing the SA-JWT or OAuth provider.
	// Injected so tests bypass the on-disk credential.
	ProviderForSite func(ctx context.Context, gc config.GSCConfig) (gsc.TokenProvider, error)
	// Now is the injectable UTC clock for the dataState=final date window and the
	// inspected-at stamp. Nil defaults to time.Now().UTC().
	Now func() time.Time
	// LookbackDays is the search-analytics window width (finalized days). <=0 uses
	// gscDefaultLookbackDays.
	LookbackDays int
	// DailyInspectBudget caps URL-inspections per pass. <=0 uses
	// gscDefaultDailyInspectBudget.
	DailyInspectBudget int
	// Logger, when set, records skipped per-URL inspections (best-effort). Nil is a
	// silent no-op (tests, library use). It never logs secrets.
	Logger *slog.Logger
}

func (p *GSCPuller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *GSCPuller) lookbackDays() int {
	if p.LookbackDays > 0 {
		return p.LookbackDays
	}
	return gscDefaultLookbackDays
}

func (p *GSCPuller) inspectBudget() int {
	if p.DailyInspectBudget > 0 {
		return p.DailyInspectBudget
	}
	return gscDefaultDailyInspectBudget
}

// siteContext resolves the per-site GSC config, builds the token provider, and the
// API client. It returns ok=false (clean, no error) when the site is not GSC-
// configured so callers skip it. A provider/client build failure IS an error
// (the credential is misconfigured and the operator must know).
func (p *GSCPuller) siteContext(ctx context.Context, site model.Site) (gc config.GSCConfig, client GSCClient, ok bool, err error) {
	if p.ResolveGSC == nil {
		return config.GSCConfig{}, nil, false, nil
	}
	gc, configured := p.ResolveGSC(site.BaseURL)
	if !configured {
		return config.GSCConfig{}, nil, false, nil
	}
	prov, perr := p.ProviderForSite(ctx, gc)
	if perr != nil {
		return gc, nil, true, fmt.Errorf("gsc: build credential for site %q: %w", site.BaseURL, perr)
	}
	c, cerr := p.API(prov)
	if cerr != nil {
		return gc, nil, true, fmt.Errorf("gsc: build client for site %q: %w", site.BaseURL, cerr)
	}
	return gc, c, true, nil
}

// Pull runs both GSC pulls for one site: the daily search-analytics pull and the
// bounded URL-inspection pass. It resolves the credential/client ONCE and shares it
// across both halves. The two halves are independent — a search-analytics failure
// does NOT skip the inspection pass (and vice versa); their errors are joined so a
// partial failure is surfaced without losing the other half's work. A site without
// a GSC block is a clean no-op.
func (p *GSCPuller) Pull(ctx context.Context, site model.Site) error {
	gc, client, ok, err := p.siteContext(ctx, site)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	var errs error
	if saErr := p.pullSearchAnalytics(ctx, site, gc, client); saErr != nil {
		errs = errors.Join(errs, saErr)
	}
	if _, insErr := p.runInspectionPass(ctx, site, gc, client, p.inspectBudget()); insErr != nil {
		errs = errors.Join(errs, insErr)
	}
	return errs
}

// PullSearchAnalytics resolves the site's GSC client and runs only the search-
// analytics pull. Exposed for the doctor/tests and for callers that want the halves
// separately. A non-GSC site is a clean no-op.
func (p *GSCPuller) PullSearchAnalytics(ctx context.Context, site model.Site) error {
	gc, client, ok, err := p.siteContext(ctx, site)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return p.pullSearchAnalytics(ctx, site, gc, client)
}

// RunInspectionPass resolves the site's GSC client and runs only the bounded URL-
// inspection pass, returning the number of statuses stored. A non-GSC site is a
// clean no-op (0, nil).
func (p *GSCPuller) RunInspectionPass(ctx context.Context, site model.Site, budget int) (int, error) {
	gc, client, ok, err := p.siteContext(ctx, site)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return p.runInspectionPass(ctx, site, gc, client, budget)
}

// pullSearchAnalytics runs the dataState=final (page,query) search-analytics query
// for the finalized window and upserts the rows. The window ENDS gscPartialDataLag
// days behind today (the partial-data lag) and spans LookbackDays inclusively.
func (p *GSCPuller) pullSearchAnalytics(ctx context.Context, site model.Site, gc config.GSCConfig, client GSCClient) error {
	start, end := p.finalWindow()
	req := gsc.SearchAnalyticsRequest{
		StartDate:  start.Format("2006-01-02"),
		EndDate:    end.Format("2006-01-02"),
		Dimensions: gscSearchDimensions,
		DataState:  "final",
		RowLimit:   25000, // GSC per-request page cap; W1 pulls a single page
	}
	resp, err := client.SearchAnalyticsQuery(ctx, gc.Property, req)
	if err != nil {
		return fmt.Errorf("gsc: search-analytics pull for site %q: %w", site.BaseURL, err)
	}
	if resp == nil || len(resp.Rows) == 0 {
		return nil
	}

	// Keys are positional in the requested dimension order [date, page, query].
	metrics := make([]model.SearchMetric, 0, len(resp.Rows))
	for _, row := range resp.Rows {
		date := keyAt(row.Keys, 0)
		page := keyAt(row.Keys, 1)
		query := keyAt(row.Keys, 2)
		if page == "" || date == "" {
			// A row missing the page or date key cannot key the (site,url,query,date)
			// grain — skip it rather than store a malformed row.
			continue
		}
		metrics = append(metrics, model.SearchMetric{
			SiteID:      site.ID,
			URL:         page,
			Query:       query,
			Date:        date,
			Clicks:      int64(row.Clicks),
			Impressions: int64(row.Impressions),
			CTR:         row.CTR,
			Position:    row.Position,
		})
	}
	if len(metrics) == 0 {
		return nil
	}
	if err := p.Metrics.SaveSearchMetrics(ctx, metrics); err != nil {
		return fmt.Errorf("gsc: store search metrics for site %q: %w", site.BaseURL, err)
	}
	return nil
}

// keyAt returns the i-th positional dimension key, or "" if out of range.
func keyAt(keys []string, i int) string {
	if i < 0 || i >= len(keys) {
		return ""
	}
	return keys[i]
}

// finalWindow returns the [start, end] finalized search-analytics window: end is
// today (UTC) minus the partial-data lag, start is end minus (LookbackDays-1).
func (p *GSCPuller) finalWindow() (start, end time.Time) {
	today := p.now().Truncate(24 * time.Hour)
	end = today.AddDate(0, 0, -gscPartialDataLagDays)
	start = end.AddDate(0, 0, -(p.lookbackDays() - 1))
	return start, end
}

// runInspectionPass inspects up to `budget` prioritized URLs and upserts each
// result, returning the number stored. It respects the quota: the FIRST quota
// (429 / RESOURCE_EXHAUSTED) error halts the pass cleanly (no error — the budget is
// simply spent), while a permanent per-URL error (e.g. 404) is skipped and the pass
// continues. A non-positive budget inspects nothing.
func (p *GSCPuller) runInspectionPass(ctx context.Context, site model.Site, gc config.GSCConfig, client GSCClient, budget int) (int, error) {
	if budget <= 0 {
		return 0, nil
	}
	cands, err := p.Candidates.InspectionCandidates(ctx, site.ID, budget)
	if err != nil {
		return 0, fmt.Errorf("gsc: select inspection candidates for site %q: %w", site.BaseURL, err)
	}

	stored := 0
	var errs error
	for _, cand := range cands {
		// Cooperative cancellation: stop promptly on daemon shutdown.
		if ctx.Err() != nil {
			break
		}
		resp, ierr := client.InspectURL(ctx, gc.Property, cand.URL)
		if ierr != nil {
			if isQuotaError(ierr) {
				// Budget spent for the day: halt cleanly without surfacing it as a
				// pull failure.
				break
			}
			// A permanent per-URL inspect error (bad URL, removed page, not in the
			// property) must NOT fail the whole daily pass — one stale URL would
			// otherwise poison every other site URL's inspection. Skip it (logged for
			// observability) and continue, mirroring the robots/graph side-timers'
			// log-and-continue on a per-item failure.
			p.logInspectSkip(ctx, site, cand.URL, ierr)
			continue
		}
		status := indexStatusFrom(site.ID, cand.URL, resp, p.now())
		if uerr := p.Index.UpsertURLIndexStatus(ctx, status); uerr != nil {
			// A store-write failure is SYSTEMIC (our DB, not GSC data): surface it.
			errs = errors.Join(errs, fmt.Errorf("store index status %q: %w", cand.URL, uerr))
			continue
		}
		stored++
	}
	return stored, errs
}

// logInspectSkip records a skipped per-URL inspection at Warn when a logger is set
// (the daemon supplies one); it is a no-op otherwise and is silent during ctx
// cancellation (shutdown noise). The URL is non-secret; the error never carries the
// bearer (the gsc client scrubs it).
func (p *GSCPuller) logInspectSkip(ctx context.Context, site model.Site, url string, err error) {
	if p.Logger == nil || ctx.Err() != nil {
		return
	}
	p.Logger.Warn("gsc: url inspection skipped",
		"component", "gsc", "site", site.BaseURL, "url", url, "error", err.Error())
}

// indexStatusFrom maps a gsc InspectResponse into the store model. A zero
// LastCrawlTime (Google reported no last crawl) becomes a nil pointer (SQL NULL).
func indexStatusFrom(siteID int64, url string, resp *gsc.InspectResponse, inspectedAt time.Time) model.URLIndexStatus {
	var r gsc.IndexStatusResult
	if resp != nil {
		r = resp.InspectionResult.IndexStatusResult
	}
	st := model.URLIndexStatus{
		SiteID:          siteID,
		URL:             url,
		InspectedAt:     inspectedAt,
		Verdict:         r.Verdict,
		CoverageState:   r.CoverageState,
		IndexingState:   r.IndexingState,
		RobotsTxtState:  r.RobotsTxtState,
		PageFetchState:  r.PageFetchState,
		GoogleCanonical: r.GoogleCanonical,
		UserCanonical:   r.UserCanonical,
		CrawledAs:       r.CrawledAs,
	}
	if !r.LastCrawlTime.IsZero() {
		t := r.LastCrawlTime.UTC()
		st.LastCrawlTime = &t
	}
	return st
}

// isQuotaError reports whether err is a GSC quota/rate-limit error (429 /
// RESOURCE_EXHAUSTED) — the signal to stop the inspection pass for the day.
func isQuotaError(err error) bool {
	var apiErr *gsc.APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsQuotaExceeded()
	}
	return false
}
