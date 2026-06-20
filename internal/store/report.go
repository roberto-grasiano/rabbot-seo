package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// defaultReportTopN bounds the top-changed-URL list when the caller passes a
// non-positive TopN. It must never be left unset before the SQL LIMIT: SQLite
// treats `LIMIT -1` as UNBOUNDED, so a 0/negative TopN would otherwise return
// every changed URL instead of the intended default.
const defaultReportTopN = 10

// maxTimestampLayout is how modernc.org/sqlite renders a TIMESTAMP returned by an
// aggregate (e.g. MAX(detected_at)). Aggregates strip column type affinity, so the
// driver hands back the column value's Go String() form rather than a time.Time —
// it must be parsed back with this layout (time.Time's default String() layout).
const maxTimestampLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// ReportParams scopes a BuildReport call. Since is the resolved UTC lower bound of
// the half-open window [Since, now); SiteID nil = all sites; TopN bounds the
// top-changed-URL list (<=0 → defaultReportTopN); Segment (when non-nil) scopes
// every sub-query to URLs that are members of a segment with that name (names are
// per-site, so an all-sites report matches that name in any site). Now is the UTC
// clock the search_performance_shift enrichment uses to derive the dataState=final
// cutoff; a zero Now defaults to time.Now().UTC() (production), and a test sets it
// explicitly for a deterministic finalized window.
type ReportParams struct {
	Since   time.Time
	SiteID  *int64
	TopN    int
	Segment *string
	Now     time.Time
}

// ChangeSummary is the windowed change rollup. Total == Substantive + Cosmetic.
type ChangeSummary struct {
	Total       int `json:"total"`
	Substantive int `json:"substantive"`
	Cosmetic    int `json:"cosmetic"`
}

// IssueSummary mixes current state (Open*) with windowed deltas (…InWindow).
// OpenTotal == OpenCritical + OpenWarning + OpenInfo.
type IssueSummary struct {
	OpenTotal      int `json:"open_total"`
	OpenCritical   int `json:"open_critical"`
	OpenWarning    int `json:"open_warning"`
	OpenInfo       int `json:"open_info"`
	OpenedInWindow int `json:"opened_in_window"`
	ClosedInWindow int `json:"closed_in_window"`
}

// URLChangeCount is one row of the top-changed-URL list. SearchShift is the ADDITIVE
// GSC W2 search_performance_shift enrichment (signal 3): nil unless this URL's
// primary query moved measurably across the change date AND enough FINALIZED
// post-change search data exists to be meaningful. It is purely additive — its
// absence is the common case and never implies "no shift", only "not enough
// finalized data to claim one" (the dataState=final discipline). It is NEVER a
// standalone row or alert; it annotates an existing changed-URL row.
type URLChangeCount struct {
	URLID       int64        `json:"url_id"`
	URL         string       `json:"url"`
	Count       int          `json:"count"`
	LastChanged time.Time    `json:"last_changed"`
	SearchShift *SearchShift `json:"search_shift,omitempty"`
}

// SiteRollup is one per-site summary row (all-sites scope only). Health is the
// LIVE whole-site health score for that site (A6): each site is scored
// independently, so an undefined (cold-start / below-coverage-floor) site reports
// Health.Defined=false and renderers show "—", never a fake 100/0. There is NO
// blended cross-site number (per-site is the product unit).
type SiteRollup struct {
	SiteID     int64       `json:"site_id"`
	BaseURL    string      `json:"base_url"`
	Changes    int         `json:"changes"`
	OpenIssues int         `json:"open_issues"`
	Health     ScopeHealth `json:"health"`
}

// ScopeHealth is the live health score of one scope (whole site or one segment) for
// the report surface (A6). It is the report-local projection of a HealthScore: the
// derived 0..100 Score plus the canonical masses and coverage counts, so an
// undefined score is self-explaining. Defined is false (and Score meaningless) when
// the scope has no importance-weighted page mass or sits below MinScoreCoverage;
// callers MUST render "—", never a fake number.
type ScopeHealth struct {
	Defined       bool    `json:"defined"`
	Score         float64 `json:"score"`
	ImpactMass    int     `json:"impact_mass"`
	MaxMass       int     `json:"max_mass"`
	KnownURLs     int     `json:"known_urls"`
	ProcessedURLs int     `json:"processed_urls"`
	OpenCritical  int     `json:"open_critical"`
	OpenWarning   int     `json:"open_warning"`
	OpenInfo      int     `json:"open_info"`
}

// SegmentHealth is one segment's live score in a site-scoped report's HealthBlock.
type SegmentHealth struct {
	SegmentID *int64 `json:"segment_id,omitempty"`
	Name      string `json:"name"`
	ScopeHealth
}

// ContributingRule is one rule's UNCAPPED open-issue mass over the site scope —
// the ranking attribution behind the score (most impactful rule first). The mass is
// the raw Σ impact_points, NOT the capped contribution the score math uses (the ADR
// records the distinction).
type ContributingRule struct {
	RuleID string `json:"rule_id"`
	Mass   int    `json:"mass"`
}

// HealthBlock is the site-scoped health rollup carried by a site-scoped BuildReport
// (A6). Current is the LIVE whole-site score; WindowStart is the earliest persisted
// trend point at/after the window lower bound (nil when the series has no point in
// window); Delta is Current.Score - *WindowStart (nil when either is unavailable).
// Segments is the live per-segment scores; TopRules ranks the contributing rules.
// It is present only when the report is scoped to one site — an all-sites report
// carries per-site scores on each SiteRollup instead.
type HealthBlock struct {
	Current     ScopeHealth        `json:"current"`
	WindowStart *float64           `json:"window_start_score,omitempty"`
	Delta       *float64           `json:"delta,omitempty"`
	Segments    []SegmentHealth    `json:"segments,omitempty"`
	TopRules    []ContributingRule `json:"top_rules,omitempty"`
}

// ReportResult is the canonical aggregation result both surfaces consume. TopURLs
// and Sites are nil (not empty) when there is nothing to report. Health is non-nil
// only for a site-scoped report (A6); an all-sites report carries per-site scores on
// each SiteRollup.
type ReportResult struct {
	Changes ChangeSummary
	Issues  IssueSummary
	TopURLs []URLChangeCount
	Sites   []SiteRollup
	Health  *HealthBlock
}

// reportTopRules bounds the contributing-rule ranking in a HealthBlock.
const reportTopRules = 5

// BuildReport runs the windowed aggregation queries and assembles a ReportResult.
// It is the single source of truth for the CLI `report` command and the daemon's
// GET /v1/report handler. All reads use db.Read(); it performs no writes.
func (db *DB) BuildReport(ctx context.Context, p ReportParams) (ReportResult, error) {
	if p.TopN <= 0 {
		p.TopN = defaultReportTopN
	}
	var res ReportResult
	var err error
	if res.Changes, err = db.changeSummary(ctx, p.Since, p.SiteID, p.Segment); err != nil {
		return ReportResult{}, err
	}
	if res.Issues, err = db.issueSummary(ctx, p.Since, p.SiteID, p.Segment); err != nil {
		return ReportResult{}, err
	}
	if res.TopURLs, err = db.topChangedURLs(ctx, p.Since, p.SiteID, p.Segment, p.TopN); err != nil {
		return ReportResult{}, err
	}
	// GSC W2 signal 3: annotate each top-changed URL with a search_performance_shift
	// enrichment when (and only when) enough FINALIZED post-change search data exists
	// to claim a measurable move on its primary query. This is purely additive — the
	// common case attaches nothing — and is computed at the read layer (here), because
	// the correlation needs finalized post-change data that does not yet exist at the
	// instant a change alert fires. Per-URL siteID comes from the report scope when the
	// report is site-scoped, else resolved from the row's site.
	if err = db.annotateSearchShifts(ctx, p.SiteID, p.Now, res.TopURLs); err != nil {
		return ReportResult{}, err
	}
	if p.SiteID == nil {
		if res.Sites, err = db.siteRollups(ctx, p.Since, p.Segment); err != nil {
			return ReportResult{}, err
		}
	} else {
		if res.Health, err = db.healthBlock(ctx, *p.SiteID, p.Since); err != nil {
			return ReportResult{}, err
		}
	}
	return res, nil
}

// scopeHealthFrom projects a computed HealthScore onto the report-local ScopeHealth.
func scopeHealthFrom(hs HealthScore) ScopeHealth {
	return ScopeHealth{
		Defined:       hs.Defined,
		Score:         hs.Score,
		ImpactMass:    hs.ImpactMass,
		MaxMass:       hs.MaxMass,
		KnownURLs:     hs.KnownURLs,
		ProcessedURLs: hs.ProcessedURLs,
		OpenCritical:  hs.OpenCritical,
		OpenWarning:   hs.OpenWarning,
		OpenInfo:      hs.OpenInfo,
	}
}

// healthBlock assembles the site-scoped HealthBlock (A6): the LIVE whole-site score,
// the window-start score + delta from the persisted trend (the first point at/after
// `since`), the live per-segment scores, and the top contributing rules ranked by
// uncapped open-issue mass. The current score is always computed live (so an ignore
// reflects immediately); the window-start point comes from the persisted series.
func (db *DB) healthBlock(ctx context.Context, siteID int64, since time.Time) (*HealthBlock, error) {
	cur, err := db.ComputeHealthScore(ctx, siteID, nil)
	if err != nil {
		return nil, err
	}
	hb := &HealthBlock{Current: scopeHealthFrom(cur)}

	// Window-start score + delta from the persisted series. The first persisted point
	// at/after `since` is the window's starting score; the delta is the move from it
	// to the live current. Only when both are defined (current is defined here means
	// the scope is above the floor; an empty series leaves WindowStart nil).
	series, err := db.HealthScoreSeries(ctx, siteID, nil, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 && cur.Defined {
		ws := series[0].Score // oldest-first; first point >= since
		hb.WindowStart = &ws
		d := cur.Score - ws
		hb.Delta = &d
	}

	// Top contributing rules from the live whole-site breakdown (uncapped per-rule
	// mass). Ranked mass DESC, then rule_id ASC for a deterministic order.
	hb.TopRules = topContributingRules(cur.Breakdown, reportTopRules)

	// Live per-segment scores: every segment of the site, scored independently. A
	// segment below its own coverage floor reports Defined=false (renders "—").
	segs, err := db.ListSegments(ctx, &siteID)
	if err != nil {
		return nil, err
	}
	for _, s := range segs {
		segID := s.ID
		shs, scErr := db.ComputeHealthScore(ctx, siteID, &segID)
		if scErr != nil {
			return nil, scErr
		}
		hb.Segments = append(hb.Segments, SegmentHealth{
			SegmentID:   &segID,
			Name:        s.Name,
			ScopeHealth: scopeHealthFrom(shs),
		})
	}
	return hb, nil
}

// topContributingRules ranks a HealthScore.Breakdown ({"rule_id": uncapped mass})
// into at most n ContributingRules, mass DESC then rule_id ASC (deterministic).
func topContributingRules(breakdown string, n int) []ContributingRule {
	if breakdown == "" {
		return nil
	}
	var raw map[string]int
	if err := json.Unmarshal([]byte(breakdown), &raw); err != nil || len(raw) == 0 {
		// A malformed/empty breakdown is not a report failure — the ranking is purely
		// attributive (the score itself is computed from the masses, not this string).
		return nil
	}
	rules := make([]ContributingRule, 0, len(raw))
	for id, m := range raw {
		rules = append(rules, ContributingRule{RuleID: id, Mass: m})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Mass != rules[j].Mass {
			return rules[i].Mass > rules[j].Mass
		}
		return rules[i].RuleID < rules[j].RuleID
	})
	if n > 0 && len(rules) > n {
		rules = rules[:n]
	}
	return rules
}

// segmentJoin returns the JOIN fragment that scopes a query to URLs that are
// members of a segment named *segment, plus its bind arg, given the column that
// holds the URL id (e.g. "c.url_id", "i.url_id", "u.id"). When segment is nil it
// returns an empty fragment and no arg. The placeholder sits in the JOIN clause,
// so the returned arg must be bound BEFORE any WHERE-clause args. Segment names
// are unique per site (idx_segments_site_name), so the join adds at most one row
// per (url, name) — counts never inflate. An all-sites query matches the name in
// any site; a site-scoped query is already constrained to one site via the urls
// join, so membership naturally limits to that site's segment.
func segmentJoin(urlIDCol string, segment *string) (clause string, args []any) {
	if segment == nil {
		return "", nil
	}
	return ` JOIN url_segments us ON us.url_id = ` + urlIDCol +
		` JOIN segments seg ON seg.id = us.segment_id AND seg.name = ?`, []any{*segment}
}

func (db *DB) changeSummary(ctx context.Context, since time.Time, siteID *int64, segment *string) (ChangeSummary, error) {
	q := `SELECT c.change_class, COUNT(*) FROM changes c`
	segClause, segArgs := segmentJoin("c.url_id", segment)
	q += segClause
	where := []string{"c.detected_at >= ?"}
	// JOIN-clause args bind before WHERE-clause args.
	args := append(segArgs, since)
	if siteID != nil {
		q += ` JOIN urls u ON u.id = c.url_id`
		where = append(where, "u.site_id = ?")
		args = append(args, *siteID)
	}
	q += ` WHERE ` + strings.Join(where, " AND ") + ` GROUP BY c.change_class`
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return ChangeSummary{}, err
	}
	defer func() { _ = rows.Close() }()
	var cs ChangeSummary
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return ChangeSummary{}, err
		}
		switch model.ChangeClass(class) {
		case model.ChangeSubstantive:
			cs.Substantive = n
			cs.Total += n
		case model.ChangeCosmetic:
			cs.Cosmetic = n
			cs.Total += n
		default:
			// Unknown change_class (schema-impossible today — the column is a closed
			// enum, NOT NULL DEFAULT 'substantive' — but defends against a future
			// migration value or a corrupted row). Excluded from Total so the
			// documented invariant Total == Substantive + Cosmetic always holds.
		}
	}
	return cs, rows.Err()
}

func (db *DB) issueSummary(ctx context.Context, since time.Time, siteID *int64, segment *string) (IssueSummary, error) {
	var is IssueSummary
	// open-now by severity (current state, NOT windowed)
	openQ := `SELECT i.severity, COUNT(*) FROM issues i`
	segClause, segArgs := segmentJoin("i.url_id", segment)
	openQ += segClause
	openWhere := []string{"i.status = 'open'"}
	// JOIN-clause args bind before WHERE-clause args.
	openArgs := append([]any{}, segArgs...)
	if siteID != nil {
		openQ += ` JOIN urls u ON u.id = i.url_id`
		openWhere = append(openWhere, "u.site_id = ?")
		openArgs = append(openArgs, *siteID)
	}
	openQ += ` WHERE ` + strings.Join(openWhere, " AND ") + ` GROUP BY i.severity`
	rows, err := db.Read().QueryContext(ctx, openQ, openArgs...)
	if err != nil {
		return IssueSummary{}, err
	}
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var sev string
			var n int
			if scanErr := rows.Scan(&sev, &n); scanErr != nil {
				err = scanErr
				return
			}
			switch model.Severity(sev) {
			case model.SeverityCritical:
				is.OpenCritical = n
				is.OpenTotal += n
			case model.SeverityWarning:
				is.OpenWarning = n
				is.OpenTotal += n
			case model.SeverityInfo:
				is.OpenInfo = n
				is.OpenTotal += n
			default:
				// Unknown severity (schema-impossible today — closed enum, NOT NULL
				// DEFAULT 'info' — but defends against a future value or corrupted
				// row). Excluded from OpenTotal so the invariant
				// OpenTotal == OpenCritical + OpenWarning + OpenInfo always holds.
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return IssueSummary{}, err
	}
	// opened-in-window / closed-in-window (scalar counts). closed_at >= since is
	// NULL-safe (NULLs never satisfy >=), so only genuinely resolved issues count.
	if is.OpenedInWindow, err = db.issueWindowCount(ctx, issueOpenedField, since, siteID, segment); err != nil {
		return IssueSummary{}, err
	}
	if is.ClosedInWindow, err = db.issueWindowCount(ctx, issueClosedField, since, siteID, segment); err != nil {
		return IssueSummary{}, err
	}
	return is, nil
}

// issueWindowField is a closed enum selecting which issue timestamp the windowed
// count filters on. It exists so issueWindowCount takes no caller-supplied SQL
// fragment — the column is resolved internally from literals, removing any
// injection surface (the value never originates from user input).
type issueWindowField int

const (
	issueOpenedField issueWindowField = iota // i.opened_at
	issueClosedField                         // i.closed_at
)

func (db *DB) issueWindowCount(ctx context.Context, field issueWindowField, since time.Time, siteID *int64, segment *string) (int, error) {
	col := "i.opened_at"
	if field == issueClosedField {
		col = "i.closed_at"
	}
	q := `SELECT COUNT(*) FROM issues i`
	segClause, segArgs := segmentJoin("i.url_id", segment)
	q += segClause
	where := []string{col + " >= ?"}
	// JOIN-clause args bind before WHERE-clause args.
	args := append(segArgs, since)
	if siteID != nil {
		q += ` JOIN urls u ON u.id = i.url_id`
		where = append(where, "u.site_id = ?")
		args = append(args, *siteID)
	}
	q += ` WHERE ` + strings.Join(where, " AND ")
	var n int
	if err := db.Read().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (db *DB) topChangedURLs(ctx context.Context, since time.Time, siteID *int64, segment *string, topN int) ([]URLChangeCount, error) {
	q := `SELECT c.url_id, u.url, COUNT(*) AS cnt, MAX(c.detected_at) AS last_changed
	      FROM changes c JOIN urls u ON u.id = c.url_id`
	segClause, segArgs := segmentJoin("c.url_id", segment)
	q += segClause
	where := []string{"c.detected_at >= ?"}
	// JOIN-clause args bind before WHERE-clause args.
	args := append(segArgs, since)
	if siteID != nil {
		where = append(where, "u.site_id = ?")
		args = append(args, *siteID)
	}
	q += ` WHERE ` + strings.Join(where, " AND ") +
		` GROUP BY c.url_id, u.url ORDER BY cnt DESC, last_changed DESC, c.url_id ASC LIMIT ?`
	args = append(args, topN)
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []URLChangeCount
	for rows.Next() {
		var u URLChangeCount
		var lastChanged string
		if err := rows.Scan(&u.URLID, &u.URL, &u.Count, &lastChanged); err != nil {
			return nil, err
		}
		// MAX(detected_at) loses TIMESTAMP affinity, so it scans as a string; parse
		// it back to a time.Time so callers get a real timestamp, not raw text.
		// Defense-in-depth: if any caller stored detected_at without stripping the
		// monotonic clock reading (e.g. a raw time.Now() instead of time.Now().UTC()),
		// time.Time.String() appends a trailing " m=+..." segment that maxTimestampLayout
		// cannot parse. Trim it so topChangedURLs never errors on that coupling.
		if i := strings.Index(lastChanged, " m="); i >= 0 {
			lastChanged = lastChanged[:i]
		}
		t, perr := time.Parse(maxTimestampLayout, strings.TrimSpace(lastChanged))
		if perr != nil {
			return nil, perr
		}
		u.LastChanged = t.UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// annotateSearchShifts attaches the GSC W2 search_performance_shift enrichment to
// each top-changed-URL row in place. For every row it anchors on the URL's most
// recent in-window change day (LastChanged, as a 'YYYY-MM-DD' GSC bucket), reads the
// URL's stored search_metrics, and runs the finalized-window correlation; a row is
// enriched ONLY when SearchPerformanceShift returns ok (enough finalized post-change
// data on a primary query with a baseline). Absent/partial data attaches nothing —
// never a fabricated annotation. now is the dataState=final clock (zero → real UTC
// now). siteID is the report scope: when site-scoped every URL belongs to that site;
// for an all-sites report each URL's site is resolved from its id in one batched
// query (no N+1, no per-row site lookup).
func (db *DB) annotateSearchShifts(ctx context.Context, siteID *int64, now time.Time, top []URLChangeCount) error {
	if len(top) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Resolve each row's site id. A site-scoped report already knows it; an all-sites
	// report batches a single url_id → site_id lookup so SearchMetricsForURL is keyed
	// on the right (site_id, url) pair.
	siteByURLID := map[int64]int64{}
	if siteID != nil {
		for _, u := range top {
			siteByURLID[u.URLID] = *siteID
		}
	} else {
		ids := make([]int64, 0, len(top))
		for _, u := range top {
			ids = append(ids, u.URLID)
		}
		resolved, err := db.siteIDsForURLs(ctx, ids)
		if err != nil {
			return err
		}
		siteByURLID = resolved
	}
	for i := range top {
		sid, ok := siteByURLID[top[i].URLID]
		if !ok {
			continue // url row vanished between queries — leave it un-enriched, never error
		}
		// The change-day anchor is the URL's most-recent in-window change (UTC),
		// rendered as the GSC 'YYYY-MM-DD' bucket the correlation expects.
		changeDate := top[i].LastChanged.UTC().Format("2006-01-02")
		// A zero since reads all stored history; SearchPerformanceShift windows the
		// before/after ranges around changeDate itself, so the read need not pre-window.
		metrics, err := db.SearchMetricsForURL(ctx, sid, top[i].URL, time.Time{})
		if err != nil {
			return err
		}
		if shift, ok := SearchPerformanceShift(metrics, changeDate, now); ok {
			s := shift // escape the loop variable
			top[i].SearchShift = &s
		}
	}
	return nil
}

// siteIDsForURLs resolves a batch of url ids to their owning site ids in one query
// (a single IN (…) lookup), so the all-sites search-shift annotation avoids an N+1
// per-URL round trip. Ids absent from urls are simply omitted from the result map.
func (db *DB) siteIDsForURLs(ctx context.Context, urlIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(urlIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(urlIDs))
	args := make([]any, len(urlIDs))
	for i, id := range urlIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, site_id FROM urls WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, sid int64
		if scanErr := rows.Scan(&id, &sid); scanErr != nil {
			return nil, scanErr
		}
		out[id] = sid
	}
	return out, rows.Err()
}

// siteRollups returns one row per site that has ANY changes in the window OR ANY
// open issue (active-or-problematic). Quiet, healthy sites are omitted to keep the
// digest readable. Ordered Changes DESC, OpenIssues DESC, SiteID ASC.
func (db *DB) siteRollups(ctx context.Context, since time.Time, segment *string) ([]SiteRollup, error) {
	// The per-site rollup is a sibling of the windowed sub-queries: when a segment
	// scope is active it must use the SAME member set, or the rollup would leak
	// non-member counts and contradict the (filtered) Changes/Issues totals. The
	// seg.name ? sits in the JOIN clause, so its arg binds before the WHERE args.
	chSeg, chSegArgs := segmentJoin("c.url_id", segment)
	changesBySite, err := db.countBySite(ctx,
		`SELECT u.site_id, COUNT(*) FROM changes c JOIN urls u ON u.id = c.url_id`+chSeg+`
		 WHERE c.detected_at >= ? GROUP BY u.site_id`, append(chSegArgs, since)...)
	if err != nil {
		return nil, err
	}
	isSeg, isSegArgs := segmentJoin("i.url_id", segment)
	openBySite, err := db.countBySite(ctx,
		`SELECT u.site_id, COUNT(*) FROM issues i JOIN urls u ON u.id = i.url_id`+isSeg+`
		 WHERE i.status = 'open' GROUP BY u.site_id`, isSegArgs...)
	if err != nil {
		return nil, err
	}

	sites, err := db.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	var out []SiteRollup
	for _, s := range sites {
		ch, op := changesBySite[s.ID], openBySite[s.ID]
		if ch == 0 && op == 0 {
			continue // quiet + healthy → omit
		}
		// LIVE whole-site score for this row (A6); each site is scored independently,
		// so an undefined site reports Defined=false and renders "—". The segment scope
		// does NOT apply to the score — the score is a site-level fact, not windowed.
		hs, scErr := db.ComputeHealthScore(ctx, s.ID, nil)
		if scErr != nil {
			return nil, scErr
		}
		out = append(out, SiteRollup{
			SiteID: s.ID, BaseURL: s.BaseURL, Changes: ch, OpenIssues: op,
			Health: scopeHealthFrom(hs),
		})
	}
	sortSiteRollups(out)
	return out, nil
}

// countBySite runs a `SELECT site_id, COUNT(*) … GROUP BY site_id` query and
// returns the per-site counts as a map.
func (db *DB) countBySite(ctx context.Context, q string, args ...any) (map[int64]int, error) {
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	bySite := map[int64]int{}
	for rows.Next() {
		var sid int64
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		bySite[sid] = n
	}
	return bySite, rows.Err()
}

func sortSiteRollups(rs []SiteRollup) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Changes != rs[j].Changes {
			return rs[i].Changes > rs[j].Changes
		}
		if rs[i].OpenIssues != rs[j].OpenIssues {
			return rs[i].OpenIssues > rs[j].OpenIssues
		}
		return rs[i].SiteID < rs[j].SiteID
	})
}

// ── GSC W2 signal 3: search_performance_shift enrichment (relocated here from the
// scheduler) ─────────────────────────────────────────────────────────────────────
//
// This is an ENRICHMENT on an existing change record — "title changed on the 3rd;
// the primary query lost N impressions / dropped M positions over the following
// window" — NOT a standalone alert and NOT an Ingest into the pipeline. It lives in
// internal/store (not internal/scheduler) so the read layer can call it without an
// import cycle: BuildReport (this file) and the MCP summarize_changes handler both
// reach it from the read path, where change history is reviewed after the fact —
// because the correlation needs enough FINALIZED post-change search data, which by
// the dataState=final lag does not exist at the instant a change alert fires. It
// reads model.SearchMetric rows (a store concern, the SearchMetricsForURL keyspace).
// A standalone raw-traffic / impression / ranking-drop threshold is a HARD non-goal
// (seasonality, SERP volatility, data lag = noise). The scheduler keeps a thin
// forwarding shim (gscenrichment.go) so its synthetic tests still drive this fn.

const (
	// gscShiftWindowDays is how many days on each side of a change the correlation
	// compares. A week mirrors the puller's default lookback.
	gscShiftWindowDays = 7
	// gscShiftMinAfterFinalDays is the minimum number of FINALIZED post-change days
	// the after-window must contain to be meaningful. One finalized day is too thin
	// to claim a shift; we require a few so day-to-day noise averages out.
	gscShiftMinAfterFinalDays = 3
	// gscShiftPartialDataLagDays mirrors the puller's gscPartialDataLagDays: searchAnalytics
	// treats the trailing ~3 days as partial (not dataState=final), so they are excluded
	// from the after-window correlation. Kept in lock-step with the puller constant.
	gscShiftPartialDataLagDays = 3
)

// SearchShift is the additive enrichment attached to a change record when a primary
// query's search performance moved measurably across the change date. Deltas are
// after − before averages over the finalized windows: a negative ImpressionsDelta is
// a loss; a POSITIVE PositionDelta is a rank that got WORSE (Google position is
// rank-from-top, so a larger number is lower). Query is the primary (highest-volume)
// query the page ranked for across the compared window.
type SearchShift struct {
	Query            string  `json:"query"`
	ImpressionsDelta int64   `json:"impressions_delta"`
	PositionDelta    float64 `json:"position_delta"`
	BeforeImpr       int64   `json:"before_impressions"`
	AfterImpr        int64   `json:"after_impressions"`
	BeforePosition   float64 `json:"before_position"`
	AfterPosition    float64 `json:"after_position"`
	AfterDays        int     `json:"after_days"`
}

// String renders the enrichment as a one-line human annotation for the report / MCP
// surface (and, later, an optional Slack follow-up). It describes only what moved,
// never prescribes — and it is purely additive to the host change row.
func (s SearchShift) String() string {
	dir := "gained"
	impr := s.ImpressionsDelta
	if impr < 0 {
		dir = "lost"
		impr = -impr
	}
	out := fmt.Sprintf("primary query %q %s %d impressions", s.Query, dir, impr)
	switch {
	case s.PositionDelta > 0:
		out += fmt.Sprintf(" and dropped %.1f positions", s.PositionDelta)
	case s.PositionDelta < 0:
		out += fmt.Sprintf(" and gained %.1f positions", -s.PositionDelta)
	}
	out += fmt.Sprintf(" over the following %d finalized days", s.AfterDays)
	return out
}

// SearchPerformanceShift correlates a single change (at changeDate, the GSC
// 'YYYY-MM-DD' day the change was detected) against the page's per-(query,date)
// search metrics, returning an enrichment ONLY when there is enough finalized
// post-change data to be meaningful. metrics are the stored rows for ONE URL (as
// SearchMetricsForURL returns them — any query, any date). now is the UTC clock used
// to derive the dataState=final cutoff.
//
// It returns ok=false (no enrichment) when any of these hold — never guessing on
// thin/partial data:
//   - the after-window has fewer than gscShiftMinAfterFinalDays FINALIZED days (a
//     day is finalized only when it is at/behind now − gscShiftPartialDataLagDays);
//     the latest ~3 partial days are excluded, so a change too recent for finalized
//     after-data yields nothing (it will enrich on a later read once the days finalize);
//   - the before-window has no data at all (no baseline to compare);
//   - the primary query has no impressions on either side.
func SearchPerformanceShift(metrics []model.SearchMetric, changeDate string, now time.Time) (SearchShift, bool) {
	change, err := time.Parse("2006-01-02", changeDate)
	if err != nil {
		return SearchShift{}, false
	}
	// Finalized cutoff: the latest day whose search data is complete. Days strictly
	// after this are partial and excluded from the correlation.
	finalCutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -gscShiftPartialDataLagDays)
	beforeStart := change.AddDate(0, 0, -gscShiftWindowDays) // [beforeStart, change)
	afterEnd := change.AddDate(0, 0, gscShiftWindowDays)     // (change, afterEnd]

	// Aggregate per query, split before/after, restricting the after-window to
	// finalized days only.
	type agg struct {
		beforeImpr, afterImpr   int64
		beforePosWt, afterPosWt float64 // impression-weighted position sums
		afterDays               map[string]struct{}
	}
	byQuery := map[string]*agg{}
	get := func(q string) *agg {
		a := byQuery[q]
		if a == nil {
			a = &agg{afterDays: map[string]struct{}{}}
			byQuery[q] = a
		}
		return a
	}

	for _, m := range metrics {
		d, derr := time.Parse("2006-01-02", m.Date)
		if derr != nil {
			continue
		}
		switch {
		case !d.Before(beforeStart) && d.Before(change):
			// before window [beforeStart, change)
			a := get(m.Query)
			a.beforeImpr += m.Impressions
			a.beforePosWt += m.Position * float64(m.Impressions)
		case d.After(change) && !d.After(afterEnd):
			// after window (change, afterEnd] — finalized days only.
			if d.After(finalCutoff) {
				continue // partial day: excluded by the dataState=final discipline
			}
			a := get(m.Query)
			a.afterImpr += m.Impressions
			a.afterPosWt += m.Position * float64(m.Impressions)
			a.afterDays[m.Date] = struct{}{}
		}
	}

	// Pick the primary query: the one with the largest combined (before+after)
	// impression volume that also has a usable comparison (before data + enough
	// finalized after days). Deterministic tiebreak by query string.
	var (
		bestQ   string
		bestA   *agg
		bestVol int64
	)
	for q, a := range byQuery {
		if a.beforeImpr <= 0 {
			continue // no baseline
		}
		if len(a.afterDays) < gscShiftMinAfterFinalDays {
			continue // not enough finalized post-change data
		}
		// A query that lost ALL impressions after the change (afterImpr == 0) stays
		// in the candidate set — that collapse is itself a valid search_performance_shift.
		vol := a.beforeImpr + a.afterImpr
		if vol > bestVol || (vol == bestVol && (bestQ == "" || q < bestQ)) {
			bestVol, bestQ, bestA = vol, q, a
		}
	}
	if bestA == nil {
		return SearchShift{}, false
	}

	beforePos := weightedAvg(bestA.beforePosWt, bestA.beforeImpr)
	afterPos := weightedAvg(bestA.afterPosWt, bestA.afterImpr)
	return SearchShift{
		Query:            bestQ,
		ImpressionsDelta: bestA.afterImpr - bestA.beforeImpr,
		PositionDelta:    afterPos - beforePos,
		BeforeImpr:       bestA.beforeImpr,
		AfterImpr:        bestA.afterImpr,
		BeforePosition:   beforePos,
		AfterPosition:    afterPos,
		AfterDays:        len(bestA.afterDays),
	}, true
}

// weightedAvg returns sum/weight, or 0 when weight is non-positive (no impressions
// → no meaningful average position).
func weightedAvg(sum float64, weight int64) float64 {
	if weight <= 0 {
		return 0
	}
	return sum / float64(weight)
}
