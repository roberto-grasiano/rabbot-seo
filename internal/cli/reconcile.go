package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/segments"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// reconcileSites synchronizes config.yaml site definitions into the DB runtime
// state (decision S1): config is authoritative for which sites exist; the DB is
// runtime state keyed by normalized base URL.
//
// For each configured site it inserts (or re-enables) the site row, seeds the
// base URL due now, and best-effort seeds the sitemap inventory by delegating to
// disc.SeedSitemaps — a recursive sitemap walk (robots.txt Sitemap: directives,
// sitemap-index expansion, gzip), NOT a flat <base>/sitemap.xml fetch (that bare
// path is only the fallback the Discoverer uses when robots declares none).
// Discovery failures are never fatal. Sites present in the DB but no longer in
// config are disabled — never deleted — so their history is retained (purge is
// RemoveSite's job).
//
// It returns the first hard store-write error; sitemap discovery failures are
// logged at debug and swallowed.
// reg (A7) is the in-memory segment registry: per site, reconcile syncs the
// segment definitions, reclassifies the site's URLs, and accumulates the freshly
// compiled SiteMatcher; after the loop it atomically Swaps the whole map so a
// config reload picks up segment edits with no daemon restart. A nil reg skips
// all segment work (callers without segments wired, e.g. some unit tests).
func reconcileSites(ctx context.Context, db *store.DB, cfg *config.Config, version string, f fetcher.Fetcher, disc interface {
	SeedSitemaps(ctx context.Context, site model.Site) (int, error)
}, now time.Time, logger *slog.Logger, reg *segments.Registry) error {
	_ = version // reserved: per-site UA derivation lands with §5A access wiring.

	// matchers accumulates one compiled SiteMatcher per configured site for the
	// single atomic registry Swap at the end (A7). nil when reg is nil.
	var matchers map[int64]*segments.SiteMatcher
	if reg != nil {
		matchers = make(map[int64]*segments.SiteMatcher, len(cfg.Sites))
	}

	inConfig := make(map[string]bool, len(cfg.Sites))
	for i := range cfg.Sites {
		site := cfg.Sites[i]
		base := site.URL
		if base == "" {
			continue
		}
		// Reject base URLs that are not safe outbound targets before admitting the
		// site or issuing any fetch: the scheme must be http/https and an IP-literal
		// host must not fall in a disallowed (loopback/private/link-local/metadata)
		// range. Name-based hosts are still validated at dial time by the fetcher's
		// and robots client's SSRF Control hook. A bad URL is skipped (logged), never
		// fatal, so one mistyped site cannot stop the whole reconcile.
		if verr := fetcher.ValidateSiteURL(base, f.AllowsPrivate()); verr != nil {
			logger.Error("site skipped: invalid base url", obs.KeyComponent, "supervisor",
				"site", base, obs.KeyError, verr.Error())
			continue
		}
		inConfig[base] = true

		maxInterval := siteMaxIntervalSeconds(site, cfg)

		// Resolve/insert the site row. An existing row is re-enabled (a site that
		// was dropped and re-added returns to active monitoring with its history).
		//
		// The per-site min recheck interval and per-host concurrency are resolved
		// through the verification-aware throttle gate (cfg.ResolveCrawl) — a
		// first-class resolver call, not an if-skip. The authoritative verification
		// state is the DB proof record; a brand-new site reads back StateThrottled
		// (the migration DEFAULT), the correct safe default, so a never-verified site
		// is throttled from its first crawl. After a successful `rabbot verify`
		// flips the proof to StateVerified, the next reconcile widens the live values
		// back to the config/default tier (via SetSiteThrottle on the existing row).
		existing, err := db.GetSiteByBaseURL(ctx, base)
		var siteID int64
		switch {
		case err == nil:
			siteID = existing.ID
			if serr := db.SetSiteEnabled(ctx, siteID, true); serr != nil {
				return serr
			}
		case errors.Is(err, store.ErrNotFound):
			// A new row has no proof record yet; it would read back StateThrottled
			// (the migration DEFAULT), so insert with the resolved StateThrottled
			// budget. The shared resolveCrawlForSite below reads that same default
			// back, so the per-URL seeding is identical with no extra resolution.
			newEff := cfg.ResolveCrawl(site, verify.StateThrottled)
			id, aerr := db.AddSite(ctx, model.Site{
				BaseURL:        base,
				Name:           site.Name,
				MinInterval:    int64(newEff.MinInterval.Seconds()),
				MaxInterval:    maxInterval,
				MaxConcurrency: newEff.PerHostConcurrency,
				SpeedScale:     siteSpeedScale(site),
				Enabled:        true,
				CreatedAt:      now,
				UpdatedAt:      now,
			})
			if aerr != nil {
				return aerr
			}
			siteID = id
		default:
			return err
		}

		// Resolve the verification-aware crawl budget ONCE for this site, now that
		// siteID is known (covering both the new and existing cases). It reads the
		// authoritative DB proof state — a never-verified site reads StateThrottled,
		// so the widened floor applies; a verified site reads back the config tier.
		// Reused below for SetSiteThrottle (existing row) and the per-URL minInterval
		// seeding, so the proof state is read once per iteration, not three times.
		eff := resolveCrawlForSite(ctx, db, cfg, site, siteID)
		minInterval := int64(eff.MinInterval.Seconds())

		// Widen/restore the existing row's live throttle to the resolved values. A
		// brand-new row was already inserted at this budget above, so SetSiteThrottle
		// is only needed for the re-enabled (existing) case.
		if err == nil {
			if serr := db.SetSiteThrottle(ctx, siteID, minInterval, eff.PerHostConcurrency); serr != nil {
				return serr
			}
		}

		// Seed the base URL, due now, as a depth-0 homepage.
		if _, err := db.UpsertURL(ctx, model.URL{
			SiteID:         siteID,
			URL:            base,
			FirstSeen:      now,
			NextCheckAt:    now,
			Interval:       minInterval,
			Importance:     scheduler.ColdStartImportance(true, 0, 0),
			Depth:          0,
			StatusType:     model.StatusPage,
			LastFetchClass: model.FetchOK,
		}); err != nil {
			return err
		}

		// Best-effort sitemap discovery via the Discoverer — never fatal. The site
		// is already in hand (siteID + base just inserted/resolved above), and
		// SeedSitemaps only reads ID + BaseURL, so skip a redundant re-fetch.
		if disc != nil {
			if _, serr := disc.SeedSitemaps(ctx, model.Site{ID: siteID, BaseURL: base, Enabled: true}); serr != nil {
				logger.Debug("sitemap discovery error", obs.KeyComponent, "supervisor", "site", base, obs.KeyError, serr.Error())
			}
		}

		// A7: sync this site's segment definitions, reclassify its URLs, and stage
		// the compiled matcher for the atomic registry swap below. A bad segment
		// config (invalid regexp / duplicate / malformed name) is logged and skips
		// this site's segments — never fatal, so one mistyped segment cannot stop
		// the whole reconcile (mirrors the invalid-base-URL skip above).
		if reg != nil {
			if serr := syncSiteSegments(ctx, db, siteID, site, matchers, now, logger); serr != nil {
				return serr
			}
		}
	}

	// A7: atomically publish the freshly compiled per-site matchers so the alert
	// pipeline and discovery classify seam read the new definitions with no
	// restart. Done after the loop so a reader never sees a half-built registry.
	if reg != nil {
		reg.Swap(matchers)
	}

	// Disable DB sites that have fallen out of config (history retained, not deleted).
	sites, err := db.ListSites(ctx)
	if err != nil {
		return err
	}
	for _, s := range sites {
		if s.Enabled && !inConfig[s.BaseURL] {
			if serr := db.SetSiteEnabled(ctx, s.ID, false); serr != nil {
				return serr
			}
		}
	}
	return nil
}

// syncSiteSegments converges one site's segment definitions + URL memberships and
// stages its compiled matcher in the shared matchers map for the atomic registry
// swap. The order is: compile (validate the config) -> sync definitions (assigns
// ids) -> bind ids onto the matcher -> reclassify the site's URLs via the bound
// matcher's MatchIDs. A compile error (invalid regexp / duplicate / bad name) is
// logged and the site's segments are skipped (the matchers map keeps no entry, so
// the registry has no stale matcher for it); a store error IS returned so a
// genuine DB failure surfaces as a failed reconcile.
func syncSiteSegments(ctx context.Context, db *store.DB, siteID int64, site config.SiteConfig, matchers map[int64]*segments.SiteMatcher, now time.Time, logger *slog.Logger) error {
	matcher, cerr := segments.Compile(siteID, site.Segments)
	if cerr != nil {
		// Invalid segment config: skip this site's segments, keep the rest of the
		// reconcile going. The definitions already in the DB are left as-is rather
		// than half-synced from a config that won't compile.
		logger.Error("segment config invalid; segments skipped for site",
			obs.KeyComponent, "supervisor", "site", site.URL, obs.KeyError, cerr.Error())
		return nil
	}

	// Map config -> model.Segment for the store sync (store stays config-free).
	defs := make([]model.Segment, 0, len(site.Segments))
	for _, sc := range site.Segments {
		defs = append(defs, model.Segment{SiteID: siteID, Name: sc.Name, MatchRule: sc.Match})
	}
	nameToID, serr := db.SyncSiteSegments(ctx, siteID, defs)
	if serr != nil {
		return serr
	}
	matcher.Bind(nameToID)

	// Reclassify every URL of the site against the bound matcher in one write-tx.
	if rerr := db.ReclassifySite(ctx, siteID, matcher.MatchIDs); rerr != nil {
		return rerr
	}
	matchers[siteID] = matcher

	// A6/A7 coordination: the reclassification just moved membership wholesale, so
	// re-score the site as one event (whole site + every segment) — the per-segment
	// health trend starts at reconcile, not only at the first recheck of a member.
	// Write-on-change and the coverage floor are enforced inside the store call, so
	// a no-op reconcile records nothing. Best-effort: a score-recompute failure is
	// logged but must not fail the whole reconcile (the segment sync already
	// converged), mirroring the non-fatal sitemap-discovery handling above.
	if rerr := db.RecordSiteHealthScores(ctx, siteID, now); rerr != nil {
		logger.Error("health score record after re-segmentation failed",
			obs.KeyComponent, "supervisor", "site", site.URL, obs.KeyError, rerr.Error())
	}
	return nil
}

// verificationState reads a site's authoritative living proof state from the DB,
// treating any read error (including ErrNotFound) as StateThrottled — the safe
// default that mirrors the migration DEFAULT. The row was just inserted/resolved
// in reconcile so it exists, but this is defensive: an unverified site stays
// throttled rather than ever silently running at full speed on a read glitch.
func verificationState(ctx context.Context, db *store.DB, siteID int64) verify.State {
	rec, err := db.GetVerification(ctx, siteID)
	if err != nil {
		return verify.StateThrottled
	}
	if rec.State == "" {
		return verify.StateThrottled
	}
	return rec.State
}

// resolveCrawlForSite resolves a site's verification-aware crawl budget: it reads
// the live proof state for siteID and runs it through cfg.ResolveCrawl.
func resolveCrawlForSite(ctx context.Context, db *store.DB, cfg *config.Config, site config.SiteConfig, siteID int64) config.CrawlResolved {
	return cfg.ResolveCrawl(site, verificationState(ctx, db, siteID))
}

// siteMaxIntervalSeconds resolves a site's max recheck interval in seconds.
func siteMaxIntervalSeconds(site config.SiteConfig, cfg *config.Config) int64 {
	if site.MaxInterval != "" {
		if d, err := time.ParseDuration(site.MaxInterval); err == nil && d > 0 {
			return int64(d.Seconds())
		}
	}
	return int64(cfg.MaxIntervalDuration().Seconds())
}

// siteSpeedScale resolves a site's speed scale (0..N), defaulting to 100.
func siteSpeedScale(site config.SiteConfig) int {
	if site.Speed > 0 {
		return site.Speed
	}
	return 100
}
