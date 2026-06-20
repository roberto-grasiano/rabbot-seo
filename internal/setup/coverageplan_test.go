// internal/setup/coverageplan_test.go
package setup

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestSourceLabels(t *testing.T) {
	if SitemapEstimate.String() != "sitemap.xml" {
		t.Fatalf("SitemapEstimate.String() = %q, want %q", SitemapEstimate.String(), "sitemap.xml")
	}
	if OperatorBallpark.String() != "operator estimate" {
		t.Fatalf("OperatorBallpark.String() = %q, want %q", OperatorBallpark.String(), "operator estimate")
	}
}

func TestBallparkBounds(t *testing.T) {
	cases := []struct {
		b        Ballpark
		wantLow  int
		wantHigh int
	}{
		{BallparkUnder1k, 0, 0},
		{Ballpark1kTo5k, 1000, 5000},
		{Ballpark5kTo10k, 5000, 10000},
		{Ballpark10kTo20k, 10000, 20000},
		{Ballpark20kTo50k, 20000, 50000},
		{Ballpark50kPlus, 50000, 50000},
		{BallparkNotSure, 0, 0},
	}
	for _, c := range cases {
		low, high := c.b.Bounds()
		if low != c.wantLow || high != c.wantHigh {
			t.Errorf("%v.Bounds() = (%d, %d), want (%d, %d)", c.b, low, high, c.wantLow, c.wantHigh)
		}
	}
}

func TestBallparkOrderAndLabels(t *testing.T) {
	wantLabels := []string{
		"Under 1,000", "1,000 – 5,000", "5,000 – 10,000",
		"10,000 – 20,000", "20,000 – 50,000", "50,000+", "Not sure",
	}
	if len(BallparkOrder) != len(wantLabels) {
		t.Fatalf("BallparkOrder len = %d, want %d", len(BallparkOrder), len(wantLabels))
	}
	if BallparkOrder[0] != BallparkUnder1k {
		t.Errorf("BallparkOrder[0] = %v, want BallparkUnder1k (the default dismissal)", BallparkOrder[0])
	}
	for i, b := range BallparkOrder {
		if b.Label() != wantLabels[i] {
			t.Errorf("BallparkOrder[%d].Label() = %q, want %q", i, b.Label(), wantLabels[i])
		}
		got, ok := BallparkByLabel(wantLabels[i])
		if !ok || got != b {
			t.Errorf("BallparkByLabel(%q) = (%v, %v), want (%v, true)", wantLabels[i], got, ok, b)
		}
	}
	if _, ok := BallparkByLabel("bogus"); ok {
		t.Errorf("BallparkByLabel(bogus) ok = true, want false")
	}
}

func TestBallparkDismissalNeverFires(t *testing.T) {
	// Under 1,000 and Not sure must never trip the cap step at the default 2000 cap
	// (their high bound is 0, and Fires is high > cap).
	for _, b := range []Ballpark{BallparkUnder1k, BallparkNotSure} {
		if _, high := b.Bounds(); high > 2000 {
			t.Errorf("%v high bound %d would fire at the default cap; must be a no-fire bucket", b, high)
		}
	}
}

func TestPlanCapFireBoundaryAtDefaultCap(t *testing.T) {
	cfg := &config.Config{} // empty config => ResolveDiscovery default cap 2000, ResolveCrawl default 2s rate
	site := config.SiteConfig{URL: "https://example.com"}

	// At the default 2000 cap: 2000 exact pages must NOT fire; 2001 must fire.
	noFire := PlanCap(cfg, site, verify.StateVerified, 2000, 2000, SitemapEstimate)
	if noFire.Fires {
		t.Errorf("PlanCap(...,2000,2000).Fires = true, want false (exactly at cap)")
	}
	if noFire.EffectiveCap != 2000 {
		t.Errorf("EffectiveCap = %d, want 2000", noFire.EffectiveCap)
	}
	if noFire.Source != SitemapEstimate {
		t.Errorf("Source = %v, want SitemapEstimate (caller-supplied)", noFire.Source)
	}

	fire := PlanCap(cfg, site, verify.StateVerified, 2001, 2001, SitemapEstimate)
	if !fire.Fires {
		t.Errorf("PlanCap(...,2001,2001).Fires = false, want true (one over cap)")
	}
	if fire.PagesLow != 2001 || fire.PagesHigh != 2001 {
		t.Errorf("exact count: PagesLow/High = %d/%d, want 2001/2001", fire.PagesLow, fire.PagesHigh)
	}
}

func TestPlanCapEstimatesMatchCoverage(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{URL: "https://example.com"}
	rate := cfg.ResolveCrawl(site, verify.StateVerified).PerHostRate

	// Exact count: low == high => the two AllPass figures collapse to one.
	exact := PlanCap(cfg, site, verify.StateVerified, 3000, 3000, SitemapEstimate)
	wantExact := coverage.Estimate(3000, rate)
	if exact.AllPassLow != wantExact || exact.AllPassHigh != wantExact {
		t.Errorf("exact AllPass = (%+v, %+v), want both %+v", exact.AllPassLow, exact.AllPassHigh, wantExact)
	}
	if exact.AllPassLow != exact.AllPassHigh {
		t.Errorf("exact count must collapse: AllPassLow %+v != AllPassHigh %+v", exact.AllPassLow, exact.AllPassHigh)
	}

	// Ranged answer (10k–20k bucket bounds): fires on the upper bound, and the two
	// estimates are the real engine numbers at each bound (a true range, not a midpoint).
	low, high := Ballpark10kTo20k.Bounds()
	ranged := PlanCap(cfg, site, verify.StateVerified, low, high, OperatorBallpark)
	if !ranged.Fires {
		t.Errorf("10k-20k range must fire at the default 2000 cap")
	}
	if ranged.Source != OperatorBallpark {
		t.Errorf("Source = %v, want OperatorBallpark", ranged.Source)
	}
	if ranged.AllPassLow != coverage.Estimate(low, rate) {
		t.Errorf("AllPassLow = %+v, want %+v", ranged.AllPassLow, coverage.Estimate(low, rate))
	}
	if ranged.AllPassHigh != coverage.Estimate(high, rate) {
		t.Errorf("AllPassHigh = %+v, want %+v", ranged.AllPassHigh, coverage.Estimate(high, rate))
	}
	if ranged.AllPassLow.FullPass >= ranged.AllPassHigh.FullPass {
		t.Errorf("ranged estimate must widen: low FullPass %v should be < high FullPass %v",
			ranged.AllPassLow.FullPass, ranged.AllPassHigh.FullPass)
	}
}

func TestPlanCapRangeUpperBoundFires(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{URL: "https://example.com"}

	// Under 1,000 (no-fire bucket) vs 1k-5k (fires at default cap).
	lo, hi := BallparkUnder1k.Bounds()
	if PlanCap(cfg, site, verify.StateVerified, lo, hi, OperatorBallpark).Fires {
		t.Errorf("Under 1,000 must not fire")
	}
	lo, hi = Ballpark1kTo5k.Bounds()
	if !PlanCap(cfg, site, verify.StateVerified, lo, hi, OperatorBallpark).Fires {
		t.Errorf("1,000-5,000 must fire at the default 2000 cap (high 5000 > 2000)")
	}
	// 50,000+ fires (bounds (50000,50000), 50000 > 2000).
	lo, hi = Ballpark50kPlus.Bounds()
	if !PlanCap(cfg, site, verify.StateVerified, lo, hi, OperatorBallpark).Fires {
		t.Errorf("50,000+ must fire at the default 2000 cap")
	}
}

// setupIntPtr is a local *int helper (config.intPtr is unexported) so a test can
// set an explicit 0 cap (nil = inherit, &0 = unlimited) on a DiscoveryConfig.
func setupIntPtr(v int) *int { return &v }

// TestPlanCapUnlimitedCapNeverFires pins the fix for the cap step firing on a site
// already set to UNLIMITED: with the effective cap == 0 (max_pages_per_site 0 =
// unlimited), no high page count can be "over the cap", so the step must NOT fire.
// The bug was Fires = high > effCap, where effCap 0 made any positive high fire.
func TestPlanCapUnlimitedCapNeverFires(t *testing.T) {
	cfg := config.Defaults()
	cfg.Defaults.Discovery.MaxPagesPerSite = setupIntPtr(0) // 0 = unlimited
	site := config.SiteConfig{URL: "https://example.com"}

	p := PlanCap(&cfg, site, verify.StateVerified, 0, 10000, SitemapEstimate)
	if p.EffectiveCap != 0 {
		t.Fatalf("EffectiveCap = %d, want 0 (unlimited)", p.EffectiveCap)
	}
	if p.Fires {
		t.Errorf("unlimited cap (effCap=0): Fires = true, want false (a site set to unlimited is never silently capped)")
	}
}

func TestPlanCapZeroCountNoFire(t *testing.T) {
	// A (0,0) input (Under 1,000 / Not sure / count-unknown) never fires and yields
	// the zero coverage.Result (coverage.Estimate guards pages<=0).
	cfg := &config.Config{}
	site := config.SiteConfig{URL: "https://example.com"}
	p := PlanCap(cfg, site, verify.StateVerified, 0, 0, OperatorBallpark)
	if p.Fires {
		t.Errorf("(0,0) must not fire")
	}
	if p.AllPassLow != (coverage.Result{}) || p.AllPassHigh != (coverage.Result{}) {
		t.Errorf("(0,0) must yield the zero Result, got low=%+v high=%+v", p.AllPassLow, p.AllPassHigh)
	}
}
