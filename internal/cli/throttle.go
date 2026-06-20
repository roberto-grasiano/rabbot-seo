package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// applyThrottleFloor installs a site's resolved per-host spacing on the live
// frontier via SetHostRate. The resolver (ResolveCrawl) already returns the
// correct EFFECTIVE rate per tier — a throttled site's >=60s floor OR a verified
// site's speed-scaled base (e.g. ~1s at speed:200, clamped to MinPerHostRate) — so
// this no longer special-cases verified by clearing to zero; it always applies the
// resolved eff.PerHostRate. SetHostRate is settable up OR down, so a verify
// promotion lowers the rate to the verified base and a demotion raises it to the
// >=60s floor, both WITHOUT a daemon restart (PR31 #3). It is tracked separately
// from the robots Crawl-delay floor (the larger always wins). A nil frontier or
// empty host is a no-op.
func applyThrottleFloor(front *frontier.Frontier, host string, eff config.CrawlResolved) {
	if front == nil || host == "" {
		return
	}
	front.SetHostRate(host, eff.PerHostRate)
}

// frontierBaseFromConfig derives the frontier's default un-set-host base spacing +
// concurrency from config. The rate is the verified resolution of an empty site
// (the config default base, speed-unscaled, clamped to the MinPerHostRate sanity
// floor) so the frontier's base matches what ResolveCrawl hands every host; the
// concurrency falls back through the same config-if-positive-else-default rule
// ResolveCrawl uses. This replaces the hardcoded 2s/2 at frontier construction so a
// tuned defaults.per_host_rate / per_host_concurrency is honored as the base.
func frontierBaseFromConfig(cfg *config.Config) (time.Duration, int) {
	eff := cfg.ResolveCrawl(config.SiteConfig{}, verify.StateVerified)
	return eff.PerHostRate, eff.PerHostConcurrency
}

// installThrottleFloors walks every enabled site and reconciles the live
// frontier's per-host spacing to each site's resolved crawl budget: every enabled
// site gets its resolved per-host rate (the verified speed-scaled base OR the
// >=60s throttled floor) applied via SetHostRate. It runs after reconcile (and on
// the periodic re-verify cadence) so a throttled host gets its 60s floor BEFORE
// its first crawl, and an explicit `rabbot verify` promotion lowers the rate to
// the verified base on the next reconcile WITHOUT a daemon restart (PR31 #3). It is
// best-effort: a list error is logged and swallowed; ctx cancellation aborts the
// walk early. The robots Crawl-delay floor is tracked separately and is never
// lowered by applying a host rate.
func installThrottleFloors(ctx context.Context, db *store.DB, cfg *config.Config, front *frontier.Frontier, logger *slog.Logger) {
	sites, err := db.ListSites(ctx)
	if err != nil {
		if ctx.Err() == nil && logger != nil {
			logger.Debug("throttle floors: list sites failed", obs.KeyComponent, "supervisor", obs.KeyError, err.Error())
		}
		return
	}
	byURL := make(map[string]config.SiteConfig, len(cfg.Sites))
	for _, s := range cfg.Sites {
		byURL[s.URL] = s
	}
	for _, s := range sites {
		if ctx.Err() != nil {
			return
		}
		if !s.Enabled {
			continue
		}
		eff := cfg.ResolveCrawl(byURL[s.BaseURL], verificationState(ctx, db, s.ID))
		applyThrottleFloor(front, hostFromURL(s.BaseURL), eff)
	}
}
