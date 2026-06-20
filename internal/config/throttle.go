package config

import (
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// Built-in fallback constants for the unverified throttle floor. These are the
// zero/blank backstop: a ZEROED or BLANK unverified_throttle field in config can
// never silently void the floor, because each accessor falls back to the constant
// below rather than to zero. They mirror the recommended Defaults() values.
//
// They do NOT clamp a deliberate POSITIVE operator override: the throttle is
// tunable (spec D4/D5), so a small positive value (e.g. per_host_rate: "1s")
// composes through ResolveCrawl's min/max and can loosen the resolved throttle
// toward the config base — the backstop only defends the zero/blank case, not an
// intentional operator config.
const (
	floorRateFallback        = 60 * time.Second
	floorConcFallback        = 1
	floorMaxPagesFallback    = 50
	floorMinIntervalFallback = 30 * time.Minute
)

// MinPerHostRate is the absolute sanity floor on the effective per-host spacing,
// applied even to a verified site (spec D4). A hard internal constant, not
// config-tunable: it blocks an absurd speed (e.g. speed: 100000) from producing
// an abusive request rate. The robots Crawl-delay floor still composes on top in
// the frontier (spec D8). Exported so internal/frontier mirrors the SAME floor
// (SetHostRate clamps to it).
const MinPerHostRate = 250 * time.Millisecond

// throttleFloorValues is the parsed, backstopped unverified throttle floor.
type throttleFloorValues struct {
	rate        time.Duration
	conc        int
	maxPages    int
	minInterval time.Duration
}

// throttleFloor resolves the unverified throttle floor from config, applying the
// built-in fallback constants for any field that is missing, zero, or
// unparseable. This is the gate's zero/blank backstop (spec D4/D5): editing
// config to per_host_rate:0 (or blank) cannot silently remove the floor — the
// constant takes over. durOr/intOr implement the
// "config-if-positive-else-fallback" rule reused here.
//
// It backstops only the zero/blank case. A deliberate POSITIVE override is
// honored as the operator's tuned floor and composed by ResolveCrawl: a small
// positive value can loosen the resolved throttle toward the config base (the
// throttle is intentionally tunable). It can only ever SLOW/SHRINK a site below
// the full config tier, never speed it ABOVE that tier.
func (c *Config) throttleFloor() throttleFloorValues {
	ut := c.Defaults.UnverifiedThrottle
	return throttleFloorValues{
		rate:        durOr(ut.PerHostRate, "", floorRateFallback),
		conc:        intOr(ut.PerHostConcurrency, 0, floorConcFallback),
		maxPages:    intOr(ut.MaxPages, 0, floorMaxPagesFallback),
		minInterval: durOr(ut.MinInterval, "", floorMinIntervalFallback),
	}
}

// CrawlResolved is the fully-resolved per-site crawl budget the daemon enforces.
// It is the verification-aware analogue of DiscoveryResolved: Throttled records
// whether the unverified floor was applied so callers (status/inspect, the
// per-host rate floor, reconcile) can surface and act on the tier.
type CrawlResolved struct {
	PerHostRate        time.Duration
	PerHostConcurrency int
	MaxPages           int
	MinInterval        time.Duration
	Throttled          bool
}

// ResolveCrawl resolves a site's crawl budget with verification state as a
// FIRST-CLASS input (spec D2/D4) — not an if-skip branch. It first computes the
// full/config values: the base per_host_rate scaled by the per-site speed dial
// (scaleBySpeed; spec D2), merged with the resolved discovery cap and min
// interval, then:
//
//   - state == StateVerified: returns the full values, Throttled=false.
//   - any other state (StateThrottled / StateAttested / legacy-empty): composes
//     the unverified floor ELEMENT-WISE — rate = max(full, floor),
//     concurrency = min(full, floor), maxPages = min(full, floor),
//     minInterval = max(full, floor), Throttled=true.
//
// Using max/min (never a blind overwrite) keeps an already-slower operator
// config honest: the throttle can only ever SLOW or SHRINK a site, never speed
// it up. The verification state must come from the authoritative DB proof
// record, never from config intent — faking verified in config does nothing.
//
// NOTE on defaults.speed_scale: it is intentionally NOT read here. The ONLY
// speed dial that scales the resolved rate is the per-site site.Speed (spec D2).
// defaults.speed_scale only SEEDS a new site's per-site speed at insert
// (reconcile.siteSpeedScale); it is config/DB plumbing, not a global rate
// multiplier. A global speed default consulted by ResolveCrawl would be a Spec B
// enhancement (a product decision), deliberately not wired here.
func (c *Config) ResolveCrawl(site SiteConfig, state verify.State) CrawlResolved {
	base := durOr("", c.Defaults.PerHostRate, 2*time.Second) // D5: default 2s
	fullRate := scaleBySpeed(base, site.Speed)               // D2: per-site speed dial (defaults.speed_scale is NOT consulted)
	fullConc := intOr(0, c.Defaults.PerHostConcurrency, 2)
	fullMaxPages := c.ResolveDiscovery(site).MaxPages
	fullMin := siteMinInterval(site, c)

	if state == verify.StateVerified {
		return CrawlResolved{
			PerHostRate:        maxDur(fullRate, MinPerHostRate), // D4 sanity floor
			PerHostConcurrency: fullConc,
			MaxPages:           fullMaxPages,
			MinInterval:        fullMin,
			Throttled:          false,
		}
	}

	f := c.throttleFloor()
	// MaxPages: the unverified page floor is UN-LIFTABLE by config (spec A). A
	// fullMaxPages <= 0 means "unlimited" (the verified tier honors it as 0), but
	// an UNVERIFIED site must never be unlimited — minInt(0, 50) would be 0 and
	// bypass the floor, so the floor wins outright when full intent is unlimited.
	// Otherwise min(full, floor) keeps an already-smaller positive cap.
	maxPages := f.maxPages
	if fullMaxPages > 0 {
		maxPages = minInt(fullMaxPages, f.maxPages)
	}
	return CrawlResolved{
		PerHostRate:        maxDur(fullRate, f.rate),
		PerHostConcurrency: minInt(fullConc, f.conc),
		MaxPages:           maxPages,
		MinInterval:        maxDur(fullMin, f.minInterval),
		Throttled:          true,
	}
}

// siteMinInterval resolves a site's min recheck interval: the per-site override
// if it parses to a positive duration, otherwise the global default.
func siteMinInterval(site SiteConfig, c *Config) time.Duration {
	if site.MinInterval != "" {
		if d, err := time.ParseDuration(site.MinInterval); err == nil && d > 0 {
			return d
		}
	}
	return c.MinIntervalDuration()
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// scaleBySpeed scales a base per-host spacing by a speed percent: 100 = base,
// 200 = 2× faster (half the spacing), 50 = half speed (double the spacing). A
// zero/blank/negative pct is treated as 100 (no scaling) — speed never produces
// an instant rate.
func scaleBySpeed(r time.Duration, pct int) time.Duration {
	if pct <= 0 {
		pct = 100
	}
	return time.Duration(int64(r) * 100 / int64(pct))
}
