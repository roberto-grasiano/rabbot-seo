package config

import (
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestThrottleFloorBackstop pins the anti-tamper guarantee: an operator who
// zeroes/blanks every unverified_throttle field cannot void the floor — the
// accessor falls back to the built-in constants, never to zero.
func TestThrottleFloorBackstop(t *testing.T) {
	c := Defaults()
	c.Defaults.UnverifiedThrottle = UnverifiedThrottleConfig{
		PerHostRate:        "",   // blanked
		PerHostConcurrency: 0,    // zeroed
		MaxPages:           0,    // zeroed
		MinInterval:        "0s", // zeroed
	}
	f := c.throttleFloor()
	if f.rate < 60*time.Second {
		t.Errorf("rate floor = %v, want >= 60s (backstop)", f.rate)
	}
	if f.conc != 1 {
		t.Errorf("conc floor = %d, want 1 (backstop)", f.conc)
	}
	if f.maxPages != 50 {
		t.Errorf("maxPages floor = %d, want 50 (backstop)", f.maxPages)
	}
	if f.minInterval < 30*time.Minute {
		t.Errorf("minInterval floor = %v, want >= 30m (backstop)", f.minInterval)
	}
}

// TestThrottleFloorParsesConfig confirms a valid operator override is honored.
func TestThrottleFloorParsesConfig(t *testing.T) {
	c := Defaults()
	c.Defaults.UnverifiedThrottle.PerHostRate = "90s"
	if got := c.throttleFloor().rate; got != 90*time.Second {
		t.Errorf("rate floor = %v, want 90s", got)
	}
}

// TestMinPerHostRateExported pins the absolute sanity floor on effective per-host
// spacing (spec D4) as an EXPORTED constant so internal/frontier can mirror the
// same value — both the resolver clamp (ResolveCrawl) and the frontier clamp
// (SetHostRate) must agree on one floor. 250ms blocks an absurd speed (e.g.
// speed: 100000) from producing an abusive sub-quarter-second rate.
func TestMinPerHostRateExported(t *testing.T) {
	if MinPerHostRate != 250*time.Millisecond {
		t.Errorf("MinPerHostRate = %v, want 250ms", MinPerHostRate)
	}
}

// TestResolveCrawlGate is THE mandated structural guard for Phase 4. It pins the
// verification-aware throttle: any non-verified state (throttled / attested /
// legacy-empty) yields the moderate floor, while a verified state yields the
// full/config values. If the state gate is removed (ResolveCrawl returns full
// values unconditionally), the throttled/attested/legacy cases turn RED.
func TestResolveCrawlGate(t *testing.T) {
	c := Defaults() // per_host_rate 2s, conc 2, min_interval 10m, max_pages 2000

	// (a) every non-verified state => moderate floor.
	for _, st := range []verify.State{verify.StateThrottled, verify.StateAttested, verify.State("")} {
		t.Run("throttled/"+string(st), func(t *testing.T) {
			got := c.ResolveCrawl(SiteConfig{}, st)
			if !got.Throttled {
				t.Errorf("state %q: Throttled = false, want true", st)
			}
			if got.PerHostRate < 60*time.Second {
				t.Errorf("state %q: PerHostRate = %v, want >= 60s", st, got.PerHostRate)
			}
			if got.PerHostConcurrency != 1 {
				t.Errorf("state %q: PerHostConcurrency = %d, want 1", st, got.PerHostConcurrency)
			}
			if got.MaxPages > 50 {
				t.Errorf("state %q: MaxPages = %d, want <= 50", st, got.MaxPages)
			}
			if got.MinInterval < 30*time.Minute {
				t.Errorf("state %q: MinInterval = %v, want >= 30m", st, got.MinInterval)
			}
		})
	}

	// (b) verified => full/config values, never throttled.
	t.Run("verified", func(t *testing.T) {
		got := c.ResolveCrawl(SiteConfig{}, verify.StateVerified)
		if got.Throttled {
			t.Errorf("verified: Throttled = true, want false")
		}
		if got.PerHostRate != 2*time.Second {
			t.Errorf("verified: PerHostRate = %v, want 2s", got.PerHostRate)
		}
		if got.PerHostConcurrency != 2 {
			t.Errorf("verified: PerHostConcurrency = %d, want 2", got.PerHostConcurrency)
		}
		if got.MaxPages != 2000 {
			t.Errorf("verified: MaxPages = %d, want 2000", got.MaxPages)
		}
		if got.MinInterval != 10*time.Minute {
			t.Errorf("verified: MinInterval = %v, want 10m", got.MinInterval)
		}
	})
}

// TestResolveCrawlNeverSpeedsUp pins the max/min composition: an operator config
// already SLOWER than the floor keeps its slower values under throttle — the
// throttle can only slow/shrink, never relax a config to the floor.
func TestResolveCrawlNeverSpeedsUp(t *testing.T) {
	c := Defaults()
	c.Defaults.PerHostRate = "120s"       // slower than the 60s floor
	c.Defaults.PerHostConcurrency = 1     // already at the floor
	site := SiteConfig{MinInterval: "2h"} // slower than the 30m floor

	got := c.ResolveCrawl(site, verify.StateThrottled)
	if got.PerHostRate != 120*time.Second {
		t.Errorf("PerHostRate = %v, want 120s (config slower than floor must be kept)", got.PerHostRate)
	}
	if got.MinInterval != 2*time.Hour {
		t.Errorf("MinInterval = %v, want 2h (config slower than floor must be kept)", got.MinInterval)
	}
	if !got.Throttled {
		t.Errorf("Throttled = false, want true")
	}
}

// TestResolveCrawlSpeedScalesRate pins spec D2: a VERIFIED site honors the
// per-site speed dial. speed 200 halves the 2s base to 1s; speed 50 doubles it
// to 4s; speed 100 (and unset) leaves the 2s base unchanged.
func TestResolveCrawlSpeedScalesRate(t *testing.T) {
	tests := []struct {
		name  string
		speed int
		want  time.Duration
	}{
		{"unset speed keeps base", 0, 2 * time.Second},
		{"speed 100 keeps base", 100, 2 * time.Second},
		{"speed 200 halves spacing", 200, 1 * time.Second},
		{"speed 50 doubles spacing", 50, 4 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults() // per_host_rate 2s base
			site := SiteConfig{Speed: tc.speed}
			got := c.ResolveCrawl(site, verify.StateVerified)
			if got.PerHostRate != tc.want {
				t.Errorf("speed %d: PerHostRate = %v, want %v", tc.speed, got.PerHostRate, tc.want)
			}
			if got.Throttled {
				t.Errorf("speed %d: Throttled = true, want false (verified)", tc.speed)
			}
		})
	}
}

// TestResolveCrawlSanityFloorClampsAbsurdSpeed pins spec D4: an absurd speed on a
// verified site cannot drop the effective spacing below the MinPerHostRate sanity
// floor (250ms). speed 100000 on a 2s base would compute 20µs; the floor clamps
// it to 250ms.
func TestResolveCrawlSanityFloorClampsAbsurdSpeed(t *testing.T) {
	c := Defaults()
	site := SiteConfig{Speed: 100000}
	got := c.ResolveCrawl(site, verify.StateVerified)
	if got.PerHostRate != MinPerHostRate {
		t.Errorf("PerHostRate = %v, want %v (sanity floor)", got.PerHostRate, MinPerHostRate)
	}
	if got.PerHostRate < 250*time.Millisecond {
		t.Errorf("PerHostRate = %v, want >= 250ms", got.PerHostRate)
	}
}

// TestResolveCrawlSpeedUpClampedWhenThrottled pins spec D3: a speed-up on an
// UNVERIFIED site is clamped away by the 60s unverified floor — speed can only
// ever SLOW an unverified site, never speed it up. speed 200 (which would compute
// a 1s base) still resolves to the 60s floor.
func TestResolveCrawlSpeedUpClampedWhenThrottled(t *testing.T) {
	c := Defaults()
	site := SiteConfig{Speed: 200}
	got := c.ResolveCrawl(site, verify.StateThrottled)
	if got.PerHostRate < 60*time.Second {
		t.Errorf("PerHostRate = %v, want >= 60s (speed-up clamped by unverified floor)", got.PerHostRate)
	}
	if !got.Throttled {
		t.Errorf("Throttled = false, want true")
	}
}

// TestResolveCrawlCapZeroUnlimitedFloorPreserved pins the "cap 0 = unlimited"
// coherence WITHOUT lifting the un-liftable unverified 50-page floor:
//   - VERIFIED + cap 0 => MaxPages 0 (unlimited; discovery admits all).
//   - UNVERIFIED + cap 0 => MaxPages 50 (the floor WINS; 0 must NOT mean
//     unlimited for an unverified site — minInt(0,50)=0 would bypass the floor).
//   - UNVERIFIED + a large positive cap (2000) still shrinks to the 50 floor.
//   - UNVERIFIED + a small cap (30) below the floor stays 30 (min wins).
func TestResolveCrawlCapZeroUnlimitedFloorPreserved(t *testing.T) {
	verifiedZero := Defaults()
	verifiedZero.Defaults.Discovery.MaxPagesPerSite = intPtr(0)
	if got := verifiedZero.ResolveCrawl(SiteConfig{}, verify.StateVerified).MaxPages; got != 0 {
		t.Errorf("verified cap 0: MaxPages = %d, want 0 (unlimited)", got)
	}

	unverifiedZero := Defaults()
	unverifiedZero.Defaults.Discovery.MaxPagesPerSite = intPtr(0)
	if got := unverifiedZero.ResolveCrawl(SiteConfig{}, verify.StateThrottled).MaxPages; got != 50 {
		t.Errorf("unverified cap 0: MaxPages = %d, want 50 (un-liftable floor, NOT unlimited)", got)
	}

	unverified2000 := Defaults() // default cap is 2000
	if got := unverified2000.ResolveCrawl(SiteConfig{}, verify.StateThrottled).MaxPages; got != 50 {
		t.Errorf("unverified cap 2000: MaxPages = %d, want 50 (floor)", got)
	}

	unverified30 := Defaults()
	unverified30.Defaults.Discovery.MaxPagesPerSite = intPtr(30)
	if got := unverified30.ResolveCrawl(SiteConfig{}, verify.StateThrottled).MaxPages; got != 30 {
		t.Errorf("unverified cap 30: MaxPages = %d, want 30 (below floor, min wins)", got)
	}
}

// TestResolveCrawlDefaultIsBehaviorPreserving pins spec D5: with no per_host_rate
// override and an unset speed, a verified site resolves to the 2s default —
// exactly the rate the frontier hardcoded today, proving no silent slowdown when
// Phase 2 wires the resolver into the frontier.
func TestResolveCrawlDefaultIsBehaviorPreserving(t *testing.T) {
	c := Defaults()
	got := c.ResolveCrawl(SiteConfig{}, verify.StateVerified)
	if got.PerHostRate != 2*time.Second {
		t.Errorf("PerHostRate = %v, want 2s (behavior-preserving default)", got.PerHostRate)
	}
}
