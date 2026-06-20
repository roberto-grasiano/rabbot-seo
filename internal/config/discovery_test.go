package config

import (
	"testing"
	"time"
)

func TestResolveDiscoveryDefaults(t *testing.T) {
	t.Parallel()
	c := Defaults()
	r := c.ResolveDiscovery(SiteConfig{URL: "https://ex.com"})
	if !r.FollowLinks || !r.Sitemap {
		t.Errorf("defaults: FollowLinks/Sitemap should be true, got %+v", r)
	}
	if r.MaxDepth != 3 || r.MaxPages != 2000 || r.SitemapRefresh != 24*time.Hour {
		t.Errorf("defaults wrong: %+v", r)
	}
}

func TestResolveDiscoveryPerSiteOverride(t *testing.T) {
	t.Parallel()
	c := Defaults()
	no := false
	r := c.ResolveDiscovery(SiteConfig{URL: "https://ex.com", Discovery: DiscoveryConfig{
		FollowLinks: &no, MaxPagesPerSite: intPtr(10000), SitemapRefresh: "6h",
	}})
	if r.FollowLinks {
		t.Errorf("site override FollowLinks=false should win")
	}
	if r.MaxPages != 10000 || r.SitemapRefresh != 6*time.Hour {
		t.Errorf("site overrides wrong: %+v", r)
	}
	if r.MaxDepth != 3 { // not overridden -> inherits default
		t.Errorf("MaxDepth should inherit default 3, got %d", r.MaxDepth)
	}
}

// TestResolveDiscoveryZeroMeansUnlimited pins the advertised "0 = unlimited"
// contract (status/sites show/init + the `config set ...max_pages_per_site 0`
// remedy). A plain int conflated "unset (inherit)" with "explicit 0", so a 0
// silently fell back to the 2000 default and the cap could never be removed —
// the pointer (nil = inherit, &0 = unlimited) fixes it.
func TestResolveDiscoveryZeroMeansUnlimited(t *testing.T) {
	t.Parallel()
	// Global default set to 0 (the headline remedy) must resolve to 0 = unlimited.
	c := Defaults()
	c.Defaults.Discovery.MaxPagesPerSite = intPtr(0)
	if got := c.ResolveDiscovery(SiteConfig{}).MaxPages; got != 0 {
		t.Errorf("defaults max_pages_per_site=0: MaxPages=%d, want 0 (unlimited)", got)
	}
	// A per-site explicit 0 also lifts the cap for that one site.
	c2 := Defaults()
	r := c2.ResolveDiscovery(SiteConfig{Discovery: DiscoveryConfig{MaxPagesPerSite: intPtr(0)}})
	if r.MaxPages != 0 {
		t.Errorf("per-site max_pages_per_site=0: MaxPages=%d, want 0 (unlimited)", r.MaxPages)
	}
	// Unset (nil) still inherits the 2000 default — no behavior change.
	c3 := Defaults()
	if got := c3.ResolveDiscovery(SiteConfig{}).MaxPages; got != 2000 {
		t.Errorf("unset: MaxPages=%d, want 2000 (default)", got)
	}
	// A positive per-site cap still wins over the default.
	r2 := c3.ResolveDiscovery(SiteConfig{Discovery: DiscoveryConfig{MaxPagesPerSite: intPtr(100)}})
	if r2.MaxPages != 100 {
		t.Errorf("per-site 100: MaxPages=%d, want 100", r2.MaxPages)
	}
}
