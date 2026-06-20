package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// gscPullInterval is the GSC pull cadence. Search Console finalizes data on a daily
// cadence (and the URL-Inspection quota is per-day), so a daily pull is the natural
// rhythm; WithStartImmediately makes a freshly (re)started daemon pull once up front.
// It is a fixed default in W1 (not yet a config knob — that is a W2 surface).
const gscPullInterval = 24 * time.Hour

// registerGSCPull adds the periodic Google Search Console pull to s when GSC is
// enabled (puller non-nil), returning whether it registered. For each enabled site
// the job runs the daily searchAnalytics.query (storing search_metrics) and the
// quota-bounded URL-inspection pass (storing url_index_status) — PLUMBING ONLY (the
// signals/rules over those rows are W2).
//
// Like registerGraphSweep it uses SingletonMode(LimitModeReschedule): a slow GSC
// pull still in flight when the next tick fires SKIPS the overlapping invocation
// rather than running two concurrent pulls against the same property/quota (the
// retention sweep notably LACKS this; the graph sweep HAS it — we copy the graph
// sweep). WithStartImmediately mirrors the other sweeps so a freshly (re)started
// daemon pulls without waiting a full interval. The task runs under ctx so daemon
// shutdown cancels it mid-flight; a nil puller (no GSC-configured site) registers
// nothing and the feature is simply off.
func registerGSCPull(ctx context.Context, logger *slog.Logger, s gocron.Scheduler, db *store.DB, puller *scheduler.GSCPuller, signals *scheduler.GSCSignals, interval time.Duration) (bool, error) {
	if puller == nil {
		return false, nil
	}
	_, err := s.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(func() {
			sites, lerr := db.ListSites(ctx)
			if lerr != nil {
				if ctx.Err() == nil {
					logger.Error("gsc pull: list sites failed", obs.KeyComponent, "gsc", obs.KeyError, lerr.Error())
				}
				return
			}
			for _, site := range sites {
				if !site.Enabled {
					continue
				}
				if perr := puller.Pull(ctx, site); perr != nil {
					if ctx.Err() == nil {
						logger.Error("gsc pull failed", obs.KeyComponent, "gsc",
							"site", site.BaseURL, obs.KeyError, perr.Error())
					}
					// The pull failed: this site's url_index_status may be partial/stale
					// this tick, so SKIP the W2 signal evaluation for it (the next tick
					// re-evaluates once the pull succeeds). A non-GSC site Pull returns
					// nil with nothing stored, so evaluating it is a harmless no-op
					// (every URL skips on ok=false) — only a real failure gates eval.
					continue
				}
				// W2 intelligence: evaluate index_status_discrepancy / google_canonical_
				// mismatch over the rows this site's pull just refreshed, emitting/
				// resolving alerts through the same incident pipeline. nil signals (no
				// alert sink wired) is a clean no-op.
				if eerr := signals.Evaluate(ctx, site); eerr != nil && ctx.Err() == nil {
					logger.Error("gsc signals failed", obs.KeyComponent, "gsc",
						"site", site.BaseURL, obs.KeyError, eerr.Error())
				}
			}
		}),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	)
	if err != nil {
		return false, err
	}
	return true, nil
}

// storeURLCandidates is the production scheduler.URLCandidateSource: it selects a
// site's URLs to inspect, highest-importance first, bounded by the daily budget. It
// reads through the store's exported read accessor (the same pattern every store
// read uses) rather than adding a new store method, keeping the SQL a thin
// importance-ordered projection of urls. W1 prioritizes purely by importance (the
// established ranking signal); a richer "recently changed" weighting is a W2
// refinement once the signals that consume these statuses exist.
type storeURLCandidates struct {
	db *store.DB
}

// InspectionCandidates returns up to limit page URLs for siteID, ordered by
// importance desc (then url for a stable tiebreak). Only crawled pages
// (status_type='page' with a recorded fetch class) are nominated — a never-fetched
// placeholder or a non-page asset is not a useful inspection subject. A non-positive
// limit returns nothing.
func (c *storeURLCandidates) InspectionCandidates(ctx context.Context, siteID int64, limit int) ([]scheduler.InspectCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Read().QueryContext(ctx,
		`SELECT url, importance
		   FROM urls
		  WHERE site_id = ? AND status_type = 'page' AND last_fetch_class != ''
		  ORDER BY importance DESC, url ASC
		  LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, fmt.Errorf("gsc inspection candidates (site=%d): %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []scheduler.InspectCandidate
	for rows.Next() {
		var cand scheduler.InspectCandidate
		if scanErr := rows.Scan(&cand.URL, &cand.Importance); scanErr != nil {
			return nil, fmt.Errorf("scan inspection candidate: %w", scanErr)
		}
		out = append(out, cand)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate inspection candidates: %w", rowsErr)
	}
	return out, nil
}
