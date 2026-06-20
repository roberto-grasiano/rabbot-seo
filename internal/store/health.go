package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// MinScoreCoverage is the cold-start crawl-coverage floor: a scope's health
// score stays UNDEFINED (rendered "—", never a fake 100 or 0) until at least
// this fraction of its known URLs have been processed at least once
// (last_checked IS NOT NULL). Freshly added URLs default to importance 0 until
// the scheduler assigns it on first processing, so a score computed from a
// sliver of crawled pages would look confident while resting on almost nothing.
// The floor is applied per scope independently — a mostly-uncrawled segment
// renders "—" while the site score is live. See the scoring ADR.
const MinScoreCoverage = 0.5

// HealthScore is the result of ComputeHealthScore for one scope (a whole site or
// one segment). The masses are the canonical integers; Score is the derived
// 0..100 value, recomputable as 100*(1 - ImpactMass/MaxMass). Defined is false
// (and Score is meaningless) when MaxMass == 0 or the scope is below
// MinScoreCoverage; callers must render "—", never a fake number.
type HealthScore struct {
	SiteID    int64
	SegmentID *int64 // nil = whole-site scope
	Defined   bool
	Score     float64
	// Canonical integers (explainability — Score recomputes from these).
	ImpactMass int
	MaxMass    int
	// Coverage so an undefined cold-start score is self-explaining.
	KnownURLs     int
	ProcessedURLs int
	// Open-issue counts by severity over the scope's open issues.
	OpenCritical int
	OpenWarning  int
	OpenInfo     int
	// Breakdown is UNCAPPED per-rule mass JSON ({"rule_id": mass}) for ranking
	// attribution only — distinct from the capped masses the score math uses.
	Breakdown string
}

// HealthScorePoint is one persisted trend point from HealthScoreSeries.
type HealthScorePoint struct {
	ComputedAt   time.Time
	Score        float64
	ImpactMass   int
	MaxMass      int
	PageCount    int
	OpenCritical int
	OpenWarning  int
	OpenInfo     int
	Breakdown    string
}

// hsURLRow is one URL's scoring inputs within a scope.
type hsURLRow struct {
	urlID      int64
	importance float64
	processed  bool
}

// capPoints is round(1000 * clamp(importance, 0, 1)) — the most health a page
// can lose, the same scale and rounding as rules.ImpactPoints.
func capPoints(importance float64) int {
	if importance < 0 {
		importance = 0
	}
	if importance > 1 {
		importance = 1
	}
	return int(math.Round(1000 * importance))
}

// scopeURLsQuery returns the (url_id, importance, processed) rows for a scope.
// segmentID nil scopes to every URL of the site; non-nil scopes to the segment's
// members via url_segments.
func (db *DB) scopeURLs(ctx context.Context, siteID int64, segmentID *int64) ([]hsURLRow, error) {
	var (
		q    string
		args []any
	)
	if segmentID == nil {
		q = `SELECT id, importance, (last_checked IS NOT NULL) FROM urls WHERE site_id = ?`
		args = []any{siteID}
	} else {
		q = `SELECT u.id, u.importance, (u.last_checked IS NOT NULL)
		     FROM urls u
		     JOIN url_segments us ON us.url_id = u.id
		     WHERE u.site_id = ? AND us.segment_id = ?`
		args = []any{siteID, *segmentID}
	}
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("scope urls (site=%d seg=%v): %w", siteID, segmentID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []hsURLRow
	for rows.Next() {
		var r hsURLRow
		var processed int
		if err := rows.Scan(&r.urlID, &r.importance, &processed); err != nil {
			return nil, fmt.Errorf("scan scope url: %w", err)
		}
		r.processed = processed != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// hsOpenIssueRow is one open issue's scoring inputs.
type hsOpenIssueRow struct {
	urlID    int64
	ruleID   string
	impact   int
	severity model.Severity
}

// scopeOpenIssues returns the OPEN issues (ignored and closed excluded — the
// frozen product stance) for a scope's URLs.
func (db *DB) scopeOpenIssues(ctx context.Context, siteID int64, segmentID *int64) ([]hsOpenIssueRow, error) {
	var (
		q    string
		args []any
	)
	if segmentID == nil {
		q = `SELECT i.url_id, i.rule_id, i.impact_points, i.severity
		     FROM issues i
		     JOIN urls u ON u.id = i.url_id
		     WHERE u.site_id = ? AND i.status = 'open'`
		args = []any{siteID}
	} else {
		q = `SELECT i.url_id, i.rule_id, i.impact_points, i.severity
		     FROM issues i
		     JOIN urls u ON u.id = i.url_id
		     JOIN url_segments us ON us.url_id = i.url_id
		     WHERE u.site_id = ? AND us.segment_id = ? AND i.status = 'open'`
		args = []any{siteID, *segmentID}
	}
	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("scope open issues (site=%d seg=%v): %w", siteID, segmentID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []hsOpenIssueRow
	for rows.Next() {
		var r hsOpenIssueRow
		var sev string
		if err := rows.Scan(&r.urlID, &r.ruleID, &r.impact, &sev); err != nil {
			return nil, fmt.Errorf("scan open issue: %w", err)
		}
		r.severity = model.Severity(sev)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ComputeHealthScore computes the live health score for one scope: the whole
// site (segmentID nil) or one segment (segmentID non-nil), built only from
// persisted urls.importance and open issues.impact_points. It never persists.
//
// cap(u)      = round(1000 * clamp(importance, 0, 1))
// deficit(u)  = min(Σ impact_points of OPEN issues on u, cap(u))   (page-capped)
// impact_mass = Σ deficit(u), max_mass = Σ cap(u)                  (integers)
// score       = 100 * (1 - impact_mass/max_mass)                  (when defined)
//
// Defined ⇔ max_mass > 0 AND processed ≥ ceil(MinScoreCoverage * known), each
// computed per scope with integer math and an inclusive boundary. Ignored and
// closed issues are excluded (the frozen stance). The breakdown is the UNCAPPED
// per-rule mass (ranking only); the score math uses the capped masses.
func (db *DB) ComputeHealthScore(ctx context.Context, siteID int64, segmentID *int64) (HealthScore, error) {
	urls, err := db.scopeURLs(ctx, siteID, segmentID)
	if err != nil {
		return HealthScore{}, err
	}
	issues, err := db.scopeOpenIssues(ctx, siteID, segmentID)
	if err != nil {
		return HealthScore{}, err
	}

	hs := HealthScore{SiteID: siteID, SegmentID: segmentID}

	// Per-URL caps and coverage counts.
	caps := make(map[int64]int, len(urls))
	maxMass := 0
	known := 0
	processed := 0
	for _, u := range urls {
		known++
		if u.processed {
			processed++
		}
		c := capPoints(u.importance)
		caps[u.urlID] = c
		maxMass += c
	}
	hs.KnownURLs = known
	hs.ProcessedURLs = processed

	// Per-URL summed open-issue mass (for capping) + per-rule uncapped mass (for
	// the breakdown) + severity tallies.
	urlMass := make(map[int64]int, len(urls))
	ruleMass := make(map[string]int, len(issues))
	for _, iss := range issues {
		urlMass[iss.urlID] += iss.impact
		ruleMass[iss.ruleID] += iss.impact
		switch iss.severity {
		case model.SeverityCritical:
			hs.OpenCritical++
		case model.SeverityWarning:
			hs.OpenWarning++
		default:
			hs.OpenInfo++
		}
	}

	impactMass := 0
	for urlID, mass := range urlMass {
		c := caps[urlID] // 0 if the URL is not in the cap map (cannot happen: same scope)
		if mass > c {
			mass = c
		}
		impactMass += mass
	}
	hs.ImpactMass = impactMass
	hs.MaxMass = maxMass

	bd, err := json.Marshal(ruleMass)
	if err != nil {
		return HealthScore{}, fmt.Errorf("marshal breakdown: %w", err)
	}
	hs.Breakdown = string(bd)

	// Coverage floor + max_mass>0 define-ness, integer math, inclusive boundary.
	if maxMass > 0 && processed >= requiredCoverage(known) {
		hs.Defined = true
		hs.Score = 100 * (1 - float64(impactMass)/float64(maxMass))
	}
	return hs, nil
}

// requiredCoverage is ceil(MinScoreCoverage * known) with no float traps:
// MinScoreCoverage is 0.5, so this is ceil(known/2) = (known + 1) / 2.
// IMPORTANT: the integer formula is hardcoded for 0.5 — update it together
// with MinScoreCoverage. TestRequiredCoverageMatchesConstant fails on drift.
func requiredCoverage(known int) int {
	if known <= 0 {
		return 0
	}
	return (known + 1) / 2
}

// RecordHealthScores recomputes the health score for the URL's site and only the
// segments containing urlID, and inserts a history row per scope ONLY when the
// integer tuple (impact_mass, max_mass, page_count) differs from the latest
// persisted row for that (site_id, segment_id). A scope below the coverage floor
// (undefined) persists nothing — the trend starts at the first defined score.
// computed_at is always stored UTC (the local-time write bug class).
func (db *DB) RecordHealthScores(ctx context.Context, siteID, urlID int64, now time.Time) error {
	now = now.UTC()

	// Whole-site scope plus exactly the segments containing urlID — a segment
	// that does not contain the rechecked URL cannot have moved.
	segIDs, err := db.segmentsContaining(ctx, urlID)
	if err != nil {
		return err
	}
	scopes := make([]*int64, 0, len(segIDs)+1)
	scopes = append(scopes, nil) // whole site
	for i := range segIDs {
		scopes = append(scopes, &segIDs[i])
	}

	for _, seg := range scopes {
		hs, err := db.ComputeHealthScore(ctx, siteID, seg)
		if err != nil {
			return err
		}
		if !hs.Defined {
			continue // nothing persisted below the coverage floor
		}
		if err := db.insertHealthScoreIfChanged(ctx, siteID, seg, hs, now); err != nil {
			return err
		}
	}
	return nil
}

// RecordSiteHealthScores recomputes and (on change) persists the health score for
// a site's WHOLE-SITE scope plus EVERY segment of the site. It is the
// A7-coordination seam: a reconcile-time re-segmentation changes membership
// wholesale (any URL may have entered/left any segment), so — unlike
// RecordHealthScores, which scopes to the segments of one rechecked URL — the
// whole site is re-scored as one event. Write-on-change and the coverage floor
// hold per scope, identical to RecordHealthScores. computed_at is stored UTC.
func (db *DB) RecordSiteHealthScores(ctx context.Context, siteID int64, now time.Time) error {
	now = now.UTC()

	segIDs, err := db.siteSegmentIDs(ctx, siteID)
	if err != nil {
		return err
	}
	scopes := make([]*int64, 0, len(segIDs)+1)
	scopes = append(scopes, nil) // whole site
	for i := range segIDs {
		scopes = append(scopes, &segIDs[i])
	}

	for _, seg := range scopes {
		hs, err := db.ComputeHealthScore(ctx, siteID, seg)
		if err != nil {
			return err
		}
		if !hs.Defined {
			continue // nothing persisted below the coverage floor
		}
		if err := db.insertHealthScoreIfChanged(ctx, siteID, seg, hs, now); err != nil {
			return err
		}
	}
	return nil
}

// siteSegmentIDs returns the segment ids defined for a site (ascending).
func (db *DB) siteSegmentIDs(ctx context.Context, siteID int64) ([]int64, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id FROM segments WHERE site_id = ? ORDER BY id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("site segment ids (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan site segment id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// segmentsContaining returns the segment ids whose members include urlID.
func (db *DB) segmentsContaining(ctx context.Context, urlID int64) ([]int64, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT segment_id FROM url_segments WHERE url_id = ? ORDER BY segment_id`, urlID)
	if err != nil {
		return nil, fmt.Errorf("segments containing url %d: %w", urlID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan segment id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// insertHealthScoreIfChanged writes a row only when (impact_mass, max_mass,
// page_count) differs from the latest persisted row for the scope. page_count is
// the number of processed URLs the score is computed over.
func (db *DB) insertHealthScoreIfChanged(ctx context.Context, siteID int64, segmentID *int64, hs HealthScore, now time.Time) error {
	return db.WriteTx(ctx, func(tx Tx) error {
		var (
			lastImpact, lastMax, lastPages int
			haveLast                       bool
		)
		var row *sql.Row
		if segmentID == nil {
			row = tx.QueryRowContext(ctx,
				`SELECT impact_mass, max_mass, page_count FROM health_scores
				 WHERE site_id = ? AND segment_id IS NULL
				 ORDER BY computed_at DESC, id DESC LIMIT 1`, siteID)
		} else {
			row = tx.QueryRowContext(ctx,
				`SELECT impact_mass, max_mass, page_count FROM health_scores
				 WHERE site_id = ? AND segment_id = ?
				 ORDER BY computed_at DESC, id DESC LIMIT 1`, siteID, *segmentID)
		}
		switch err := row.Scan(&lastImpact, &lastMax, &lastPages); {
		case err == nil:
			haveLast = true
		case errors.Is(err, sql.ErrNoRows):
			haveLast = false
		default:
			return fmt.Errorf("read latest health row: %w", err)
		}

		pageCount := hs.ProcessedURLs
		if haveLast && lastImpact == hs.ImpactMass && lastMax == hs.MaxMass && lastPages == pageCount {
			return nil // unchanged — storage moves only when reality moves
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO health_scores
			   (site_id, segment_id, computed_at, score, impact_mass, max_mass, page_count,
			    open_critical, open_warning, open_info, breakdown)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			siteID, segmentID, now, hs.Score, hs.ImpactMass, hs.MaxMass, pageCount,
			hs.OpenCritical, hs.OpenWarning, hs.OpenInfo, hs.Breakdown)
		if err != nil {
			return fmt.Errorf("insert health score (site=%d seg=%v): %w", siteID, segmentID, err)
		}
		return nil
	})
}

// HealthScoreSeries returns the persisted trend points for a scope (whole site
// when segmentID nil), oldest-first, with computed_at >= since (a zero since
// returns the whole series). Points are read live for the dedicated score
// surfaces; the current score is computed by ComputeHealthScore.
func (db *DB) HealthScoreSeries(ctx context.Context, siteID int64, segmentID *int64, since time.Time) ([]HealthScorePoint, error) {
	var (
		q    string
		args []any
	)
	base := `SELECT computed_at, score, impact_mass, max_mass, page_count,
	                open_critical, open_warning, open_info, breakdown
	         FROM health_scores WHERE site_id = ?`
	if segmentID == nil {
		q = base + ` AND segment_id IS NULL`
		args = []any{siteID}
	} else {
		q = base + ` AND segment_id = ?`
		args = []any{siteID, *segmentID}
	}
	if !since.IsZero() {
		q += ` AND computed_at >= ?`
		args = append(args, since.UTC())
	}
	q += ` ORDER BY computed_at ASC, id ASC`

	rows, err := db.Read().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("health score series (site=%d seg=%v): %w", siteID, segmentID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []HealthScorePoint
	for rows.Next() {
		var p HealthScorePoint
		if err := rows.Scan(&p.ComputedAt, &p.Score, &p.ImpactMass, &p.MaxMass, &p.PageCount,
			&p.OpenCritical, &p.OpenWarning, &p.OpenInfo, &p.Breakdown); err != nil {
			return nil, fmt.Errorf("scan health point: %w", err)
		}
		p.ComputedAt = p.ComputedAt.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}
