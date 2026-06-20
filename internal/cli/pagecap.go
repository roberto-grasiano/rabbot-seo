package cli

import (
	"context"
	"fmt"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// pageCapped reports whether a site is at or above its page cap. A cap of 0 (or
// negative) means unlimited and is never capped. Used to surface a cap-hit as a
// queryable fact in status / get_site / sites show / the init summary, instead of
// only a buried daemon log line (Spec A D6).
func pageCapped(monitored, pageCap int) bool {
	if pageCap <= 0 {
		return false
	}
	return monitored >= pageCap
}

// baseURLCapResolver returns a site's resolved page cap (0 = unlimited) keyed by
// its base URL. cappedSitesCount uses it instead of the siteID-keyed capResolver
// so the cap can be resolved from the BaseURL already present in the ListSites
// rows — avoiding a per-site GetSite round-trip (the cap is config-derived, not
// stored on the row), turning the status hook's 1+2N queries into 1+N.
type baseURLCapResolver func(baseURL string) int

// cappedSitesCount walks enabled sites and counts how many are at their page cap.
// It is read-only: ListSites + CountSiteURLs per site + the injected resolver,
// which resolves each cap from the BaseURL already on the listed row (no extra
// GetSite). A per-site count error skips that site (best-effort; status must never
// fail on a count hiccup). Disabled sites are ignored (a paused site can't hit its
// cap).
func cappedSitesCount(ctx context.Context, db *store.DB, resolveCap baseURLCapResolver) int {
	sites, err := db.ListSites(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range sites {
		if !s.Enabled {
			continue
		}
		monitored, cerr := db.CountSiteURLs(ctx, s.ID)
		if cerr != nil {
			continue
		}
		if pageCapped(monitored, resolveCap(s.BaseURL)) {
			n++
		}
	}
	return n
}

// sitePagesLine renders the human "pages:" line for `sites show` and the init
// summary. cap 0 = unlimited. A capped site names the exact knob to raise/remove
// the cap (Spec A D6); 0 = monitor everything.
func sitePagesLine(monitored, pageCap int) string {
	if pageCap <= 0 {
		return fmt.Sprintf("pages: monitoring %d (cap: unlimited)", monitored)
	}
	if pageCapped(monitored, pageCap) {
		return fmt.Sprintf(
			"pages: monitoring %d of %d cap (capped — raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>'; 0 = all)",
			monitored, pageCap)
	}
	return fmt.Sprintf("pages: monitoring %d of %d cap", monitored, pageCap)
}

// siteConfigCap resolves a site's page cap from a loaded config by base URL,
// returning the resolved discovery MaxPages (0 = unlimited). A site absent from
// config resolves against the global defaults.
func siteConfigCap(cfg *config.Config, baseURL string) int {
	for _, s := range cfg.Sites {
		if s.URL == baseURL {
			return cfg.ResolveDiscovery(s).MaxPages
		}
	}
	return cfg.ResolveDiscovery(config.SiteConfig{}).MaxPages
}
