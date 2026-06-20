package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestPageCapped(t *testing.T) {
	tests := []struct {
		name      string
		monitored int
		cap       int
		want      bool
	}{
		{"unlimited cap never capped", 5000, 0, false},
		{"under cap", 10, 2000, false},
		{"exactly at cap is capped", 2000, 2000, true},
		{"over cap is capped", 2100, 2000, true},
		{"zero monitored under finite cap", 0, 2000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageCapped(tc.monitored, tc.cap); got != tc.want {
				t.Fatalf("pageCapped(%d,%d) = %v, want %v", tc.monitored, tc.cap, got, tc.want)
			}
		})
	}
}

func TestCappedSitesCount(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/k.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Site A: enabled, 1 URL, cap 1 -> capped.
	a, _ := db.AddSite(ctx, model.Site{BaseURL: "https://a.test", Enabled: true})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: a, URL: "https://a.test", NextCheckAt: time.Now()})
	// Site B: enabled, 1 URL, cap 0 (unlimited) -> not capped.
	b, _ := db.AddSite(ctx, model.Site{BaseURL: "https://b.test", Enabled: true})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: b, URL: "https://b.test", NextCheckAt: time.Now()})
	// Site C: DISABLED, 1 URL, cap 1 -> ignored even though it would be capped.
	c, _ := db.AddSite(ctx, model.Site{BaseURL: "https://c.test", Enabled: false})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: c, URL: "https://c.test", NextCheckAt: time.Now()})

	resolve := func(baseURL string) int {
		if baseURL == "https://b.test" {
			return 0 // unlimited
		}
		return 1
	}
	if got := cappedSitesCount(ctx, db, resolve); got != 1 {
		t.Fatalf("cappedSitesCount = %d, want 1 (only A; B unlimited, C disabled)", got)
	}
}

// TestCappedSitesCountOverBaseURLCapResolver exercises the SAME production wiring
// the status hook uses: newBaseURLCapResolver(&cfgMu, &cfg) feeding cappedSitesCount
// over a real *config.Config + cfgMu + store. It seeds one capped (per-site cap 1,
// 1 URL) and one uncapped (default cap 2000, 1 URL) enabled site, asserts the count
// is 1, then mutates cfg.Defaults.Discovery.MaxPagesPerSite to 1 and re-resolves to
// prove the resolver reads the LIVE cfg snapshot (the previously-uncapped site is
// now capped, so the count rises to 2). This is the seam the unit-mock test cannot
// cover: the live cfg snapshot + BaseURL->cap lookup inside baseURLCapResolver.
func TestCappedSitesCountOverBaseURLCapResolver(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/k.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Capped site: per-site discovery cap of 1, one monitored URL -> at cap.
	capped, _ := db.AddSite(ctx, model.Site{BaseURL: "https://capped.test", Enabled: true})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: capped, URL: "https://capped.test", NextCheckAt: time.Now()})
	// Uncapped site: no per-site cap (resolves to the default 2000), one URL -> not capped.
	uncapped, _ := db.AddSite(ctx, model.Site{BaseURL: "https://uncapped.test", Enabled: true})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: uncapped, URL: "https://uncapped.test", NextCheckAt: time.Now()})

	cfg := config.Defaults()
	capOne := 1
	cfg.Sites = []config.SiteConfig{
		{URL: "https://capped.test", Discovery: config.DiscoveryConfig{MaxPagesPerSite: &capOne}},
		{URL: "https://uncapped.test"},
	}
	var cfgMu sync.Mutex

	resolve := newBaseURLCapResolver(&cfgMu, &cfg)
	if got := cappedSitesCount(ctx, db, resolve); got != 1 {
		t.Fatalf("cappedSitesCount = %d, want 1 (only capped.test at its cap of 1)", got)
	}

	// Tighten the GLOBAL default cap to 1 under the lock; the resolver must observe
	// the live cfg on its next call, capping the previously-uncapped site too.
	cfgMu.Lock()
	cfg.Defaults.Discovery.MaxPagesPerSite = &capOne
	cfgMu.Unlock()
	if got := cappedSitesCount(ctx, db, resolve); got != 2 {
		t.Fatalf("cappedSitesCount after lowering default cap = %d, want 2 (live cfg snapshot)", got)
	}
}
