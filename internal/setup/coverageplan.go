// internal/setup/coverageplan.go
//
// Package-internal: the Spec B onboarding coverage planner core. Pure and
// UI-free — it decides whether the page-cap step fires while ADDING a site and
// what each choice costs (full-pass time + disk), reusing the Spec A estimator
// (coverage.Estimate) and the resolved crawl budget/cap (config.Resolve*). It
// also owns the ranged-ballpark buckets the TUI renders from. The network
// sitemap COUNT stays in internal/cli (countSitemapPages); this core takes the
// (low, high) page count as INPUT. No UI, no goroutine, no I/O.
package setup

import (
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// Source labels where the page-count estimate came from, so the wizard can be
// honest ("(estimated from sitemap.xml)" vs an operator ballpark).
type Source int

const (
	// SitemapEstimate: the count came from the site's sitemap.xml (exact count,
	// PagesLow == PagesHigh), labeled an estimate because sitemaps lie.
	SitemapEstimate Source = iota
	// OperatorBallpark: no/broken sitemap — the operator picked a ranged bucket,
	// so PagesLow/PagesHigh are the bucket bounds.
	OperatorBallpark
)

// String renders the human label embedded in the wizard's coverage copy.
func (s Source) String() string {
	switch s {
	case SitemapEstimate:
		return "sitemap.xml"
	case OperatorBallpark:
		return "operator estimate"
	default:
		return "unknown"
	}
}

// Ballpark is the operator's ranged page-count answer when the sitemap can't be
// read (Spec B D5). The default (BallparkUnder1k) is a one-keystroke dismissal.
// These buckets are OWNED here; the Phase 4 TUI renders its select from
// BallparkOrder + Label() and resolves a chosen label via BallparkByLabel.
type Ballpark int

const (
	// BallparkUnder1k: "Under 1,000" — the default; never fires (bounds (0,0)).
	BallparkUnder1k Ballpark = iota
	// Ballpark1kTo5k: "1,000 – 5,000" — fires at the default 2000 cap.
	Ballpark1kTo5k
	// Ballpark5kTo10k: "5,000 – 10,000".
	Ballpark5kTo10k
	// Ballpark10kTo20k: "10,000 – 20,000".
	Ballpark10kTo20k
	// Ballpark20kTo50k: "20,000 – 50,000".
	Ballpark20kTo50k
	// Ballpark50kPlus: "50,000+" — open-ended; bounds (50000,50000) so it always
	// fires below a 50k cap; the TUI renders its "monitor all" line as a floor.
	Ballpark50kPlus
	// BallparkNotSure: "Not sure" — treated like Under 1,000: no cap change.
	BallparkNotSure
)

// BallparkOrder is the display order for the ranged question, BallparkUnder1k
// first (the default, one-keystroke dismissal).
var BallparkOrder = []Ballpark{
	BallparkUnder1k,
	Ballpark1kTo5k,
	Ballpark5kTo10k,
	Ballpark10kTo20k,
	Ballpark20kTo50k,
	Ballpark50kPlus,
	BallparkNotSure,
}

// Label is the human-readable bucket label rendered in the ranged question.
func (b Ballpark) Label() string {
	switch b {
	case BallparkUnder1k:
		return "Under 1,000"
	case Ballpark1kTo5k:
		return "1,000 – 5,000"
	case Ballpark5kTo10k:
		return "5,000 – 10,000"
	case Ballpark10kTo20k:
		return "10,000 – 20,000"
	case Ballpark20kTo50k:
		return "20,000 – 50,000"
	case Ballpark50kPlus:
		return "50,000+"
	case BallparkNotSure:
		return "Not sure"
	default:
		return "Under 1,000"
	}
}

// Bounds maps a bucket to its (low, high) page bounds. The dismissal buckets
// (Under 1,000, Not sure) return (0, 0) so high (0) can never exceed a positive
// cap — they never fire and write no cap change. The open-ended 50,000+ bucket
// returns (50000, 50000): it fires below a 50k cap, and the TUI renders its
// "monitor all" estimate (at 50k) as a floor ("≈ X+").
func (b Ballpark) Bounds() (low, high int) {
	switch b {
	case Ballpark1kTo5k:
		return 1000, 5000
	case Ballpark5kTo10k:
		return 5000, 10000
	case Ballpark10kTo20k:
		return 10000, 20000
	case Ballpark20kTo50k:
		return 20000, 50000
	case Ballpark50kPlus:
		return 50000, 50000
	default: // BallparkUnder1k, BallparkNotSure
		return 0, 0
	}
}

// BallparkByLabel resolves a selected label back to its bucket. ok is false for
// an unrecognized label (defensive — the select only emits known labels).
func BallparkByLabel(label string) (Ballpark, bool) {
	for _, b := range BallparkOrder {
		if b.Label() == label {
			return b, true
		}
	}
	return BallparkUnder1k, false
}

// CapPlan is the pure decision for the onboarding page-cap step: does it fire,
// what is the count (or range), and what does "monitor all" cost. It is computed
// by PlanCap from a resolved count + the site's resolved crawl budget/cap; the
// TUI step (Phase 4) and any caller render it without recomputing.
type CapPlan struct {
	// Fires is true when the effective cap is positive AND the high page bound
	// exceeds it, i.e. the site would be silently capped — the only case the step
	// appears. An effective cap of 0 (unlimited) never fires: nothing is capped.
	Fires bool
	// PagesLow is the low page bound; == PagesHigh for an exact sitemap count.
	PagesLow int
	// PagesHigh is the high page bound; Fires is decided against this.
	PagesHigh int
	// Source labels where the count came from (sitemap vs operator ballpark).
	Source Source
	// EffectiveCap is the site's current resolved cap (default 2000).
	EffectiveCap int
	// AllPassLow is coverage.Estimate(PagesLow, rate) — the "monitor all" cost at
	// the low bound; == AllPassHigh for an exact count.
	AllPassLow coverage.Result
	// AllPassHigh is coverage.Estimate(PagesHigh, rate) — the cost at the high bound.
	AllPassHigh coverage.Result
}

// PlanCap is the pure onboarding cap decision and the ONLY CapPlan constructor
// (there is no NewCapPlan). Given a resolved page count or range (low, high —
// supplied by the caller; the sitemap count lives in cli.countSitemapPages, the
// ranged answer in Ballpark.Bounds()) and the count's Source, it resolves the
// site's effective cap and per-host rate from cfg and decides whether the cap
// step fires (cap > 0 && high > cap; an unlimited cap of 0 never fires) and what
// "monitor all" costs at each bound.
//
// state selects the crawl budget tier; callers pass verify.StateVerified so the
// estimate reflects the site's BEST (verified) rate, mirroring resolveCrawlBudget.
// src is recorded verbatim onto Source — PlanCap never infers it (low/high alone
// cannot distinguish an exact sitemap count from a bucket whose bounds coincide).
// It performs no I/O and is fully deterministic.
func PlanCap(cfg *config.Config, site config.SiteConfig, state verify.State, low, high int, src Source) CapPlan {
	effCap := cfg.ResolveDiscovery(site).MaxPages
	rate := cfg.ResolveCrawl(site, state).PerHostRate
	return CapPlan{
		// effCap == 0 means the site is set to UNLIMITED (0 = unlimited): no high
		// count can be "over the cap", so the step must not fire. Without the
		// effCap > 0 guard, any positive high would fire on an already-unlimited site.
		Fires:        effCap > 0 && high > effCap,
		PagesLow:     low,
		PagesHigh:    high,
		Source:       src,
		EffectiveCap: effCap,
		AllPassLow:   coverage.Estimate(low, rate),
		AllPassHigh:  coverage.Estimate(high, rate),
	}
}
