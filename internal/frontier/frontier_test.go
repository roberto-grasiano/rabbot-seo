package frontier

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

func TestFrontierRateLimits(t *testing.T) {
	f := New(Options{PerHostRate: 50 * time.Millisecond, PerHostConcurrency: 4})
	host := "example.com"

	start := time.Now()
	for i := 0; i < 3; i++ {
		rel, err := f.Acquire(context.Background(), host)
		if err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
		rel()
	}
	elapsed := time.Since(start)
	// 3 acquisitions at 50ms spacing => at least ~100ms (first is immediate).
	if elapsed < 90*time.Millisecond {
		t.Errorf("rate limiting too fast: elapsed %v, want >= 90ms", elapsed)
	}
}

func TestFrontierConcurrencyCap(t *testing.T) {
	f := New(Options{PerHostRate: time.Microsecond, PerHostConcurrency: 2})
	host := "example.com"

	var mu sync.Mutex
	var active, maxActive int
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := f.Acquire(context.Background(), host)
			if err != nil {
				return
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if maxActive > 2 {
		t.Errorf("maxActive = %d, want <= 2", maxActive)
	}
}

func TestFrontierGlobalPause(t *testing.T) {
	f := New(Options{PerHostRate: time.Microsecond, PerHostConcurrency: 4})
	f.SetPaused(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := f.Acquire(ctx, "example.com")
	if err == nil {
		t.Errorf("Acquire() should block (and time out) while paused")
	}
	f.SetPaused(false)
	rel, err := f.Acquire(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Acquire() after resume error = %v", err)
	}
	rel()
}

func TestFrontierSetMinIntervalFloor(t *testing.T) {
	f := New(Options{PerHostRate: 10 * time.Millisecond, PerHostConcurrency: 4})
	host := "h"

	// Establishing a 5s floor must raise the effective interval to >= 5s.
	f.SetMinInterval(host, 5*time.Second)
	if got := f.effectiveInterval(host); got < 5*time.Second {
		t.Fatalf("after SetMinInterval(5s): effectiveInterval = %v, want >= 5s", got)
	}

	// The throttle-DECAY path (fast, error-free report shrinks throttle toward 1)
	// must NOT drop the host below its advertised floor.
	f.Report(host, 10*time.Millisecond, false)
	if got := f.effectiveInterval(host); got < 5*time.Second {
		t.Fatalf("after decay Report: effectiveInterval = %v, want >= 5s (floor must hold)", got)
	}

	// A smaller later floor must NOT lower an existing floor.
	f.SetMinInterval(host, 1*time.Second)
	if got := f.effectiveInterval(host); got < 5*time.Second {
		t.Fatalf("after SetMinInterval(1s): effectiveInterval = %v, want >= 5s (floor only raises)", got)
	}
}

// TestFrontierSetHostRateSetsBase pins the headline fix (#2): SetHostRate sets the
// per-host BASE spacing, so a rate BELOW the configured PerHostRate default actually
// SPEEDS UP the host (a verified speed-dial), instead of being masked by an
// unchangeable base (the floor-only no-op bug). New(2s) is the production default
// base; SetHostRate(1s) must lower the effective spacing to ~1s, NOT leave it at 2s.
func TestFrontierSetHostRateSetsBase(t *testing.T) {
	f := New(Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 4})
	host := "fast.example"

	// speed:200 → 1s requested rate, BELOW the 2s default base. It must take effect.
	f.SetHostRate(host, 1*time.Second)
	if got := f.effectiveInterval(host); got != 1*time.Second {
		t.Fatalf("after SetHostRate(1s): effectiveInterval = %v, want 1s (NOT the 2s base)", got)
	}

	// A sub-sanity rate (100ms) is clamped UP to the 250ms MinPerHostRate floor.
	f.SetHostRate(host, 100*time.Millisecond)
	if got := f.effectiveInterval(host); got != config.MinPerHostRate {
		t.Fatalf("after SetHostRate(100ms): effectiveInterval = %v, want %v (sanity floor)", got, config.MinPerHostRate)
	}

	// A slow-down (4s, above the 2s base) raises the base to 4s.
	f.SetHostRate(host, 4*time.Second)
	if got := f.effectiveInterval(host); got != 4*time.Second {
		t.Fatalf("after SetHostRate(4s): effectiveInterval = %v, want 4s", got)
	}

	// A robots Crawl-delay floor LARGER than the host rate still wins: 5s beats a 1s rate.
	f.SetMinInterval(host, 5*time.Second)
	f.SetHostRate(host, 1*time.Second)
	if got := f.effectiveInterval(host); got != 5*time.Second {
		t.Fatalf("robots 5s + rate 1s: effectiveInterval = %v, want 5s (robots floor wins)", got)
	}

	// Clearing the host rate (d<=0) reverts the base to the configured 2s default,
	// but the robots Crawl-delay floor (5s) still holds (it is never lowered).
	f.SetHostRate(host, 0)
	if got := f.effectiveInterval(host); got != 5*time.Second {
		t.Fatalf("after SetHostRate(0) with 5s robots floor: effectiveInterval = %v, want 5s", got)
	}

	// On a host with NO robots floor, clearing reverts to exactly the 2s default base.
	host2 := "plain.example"
	f.SetHostRate(host2, 1*time.Second)
	f.SetHostRate(host2, 0)
	if got := f.effectiveInterval(host2); got != 2*time.Second {
		t.Fatalf("after SetHostRate(0): effectiveInterval = %v, want 2s (config default base)", got)
	}
}

// TestFrontierSetHostRateClampsToSanityFloor pins spec D4's belt-and-suspenders
// guard: SetHostRate clamps any positive rate below config.MinPerHostRate (250ms)
// up to that floor, so even an absurd speed-dial that slipped a sub-sanity rate
// past the resolver cannot make the frontier hammer a host. A zero still clears.
func TestFrontierSetHostRateClampsToSanityFloor(t *testing.T) {
	f := New(Options{PerHostRate: time.Second, PerHostConcurrency: 4})
	host := "absurd.example"

	// A 1ms requested rate (sub-sanity) is clamped up to the 250ms floor.
	f.SetHostRate(host, time.Millisecond)
	if got := f.effectiveInterval(host); got < config.MinPerHostRate {
		t.Fatalf("after SetHostRate(1ms): effectiveInterval = %v, want >= %v (sanity floor)", got, config.MinPerHostRate)
	}

	// Zero still clears the host rate back to the ~1s base cadence.
	f.SetHostRate(host, 0)
	if got := f.effectiveInterval(host); got > 2*time.Second {
		t.Fatalf("after SetHostRate(0): effectiveInterval = %v, want ~1s base cadence", got)
	}
}

// TestFrontierSetHostRateRaisesAndLowers pins the verification-aware host rate
// (PR31 #3): SetHostRate installs a per-host spacing floor that can be
// RAISED (a host demoted to throttled gets the 60s floor) and LOWERED (a host
// promoted to verified returns to base cadence) WITHOUT a daemon restart — unlike
// SetMinInterval, which only ever raises. The host rate is tracked separately
// from the robots Crawl-delay floor, and the effective spacing is the max of the two.
func TestFrontierSetHostRateRaisesAndLowers(t *testing.T) {
	f := New(Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 4})
	host := "throttled.example"

	// A throttled host gets the 60s effective floor.
	f.SetHostRate(host, 60*time.Second)
	if got := f.effectiveInterval(host); got < 60*time.Second {
		t.Fatalf("after SetHostRate(60s): effectiveInterval = %v, want >= 60s", got)
	}

	// After promotion the host rate is lowered to zero and the host returns to
	// the base cadence (~2s) — the asymmetric only-raises gap is closed.
	f.SetHostRate(host, 0)
	if got := f.effectiveInterval(host); got >= 60*time.Second {
		t.Fatalf("after SetHostRate(0): effectiveInterval = %v, want base (~2s), not the 60s throttle floor", got)
	}
	if got := f.effectiveInterval(host); got > 3*time.Second {
		t.Fatalf("after SetHostRate(0): effectiveInterval = %v, want ~2s base cadence", got)
	}
}

// TestFrontierHostRateDoesNotClobberRobotsFloor pins that lowering the
// host rate on promotion does NOT lower a robots Crawl-delay floor on the
// same host (PR31 #3): the two floors are tracked separately, and the effective
// spacing is max(robotsFloor, throttleFloor). A site whose robots.txt advertises
// a 90s Crawl-delay must keep that 90s floor even after a verify promotion clears
// its host rate.
func TestFrontierHostRateDoesNotClobberRobotsFloor(t *testing.T) {
	f := New(Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 4})
	host := "crawldelay.example"

	// A robots Crawl-delay floor (90s) and a host rate (60s) coexist; the
	// larger (robots 90s) governs the effective spacing.
	f.SetMinInterval(host, 90*time.Second)
	f.SetHostRate(host, 60*time.Second)
	if got := f.effectiveInterval(host); got < 90*time.Second {
		t.Fatalf("robots 90s + throttle 60s: effectiveInterval = %v, want >= 90s (max wins)", got)
	}

	// Promotion clears the host rate, but the robots Crawl-delay floor must
	// NOT be lowered — the host still spaces at >= 90s.
	f.SetHostRate(host, 0)
	if got := f.effectiveInterval(host); got < 90*time.Second {
		t.Fatalf("after promotion: effectiveInterval = %v, want >= 90s (robots Crawl-delay floor must survive)", got)
	}
}

func TestFrontierAdaptiveThrottle(t *testing.T) {
	f := New(Options{PerHostRate: 10 * time.Millisecond, PerHostConcurrency: 4})
	host := "slow.example.com"
	base := f.effectiveInterval(host)
	// Report repeated errors/slow responses; effective interval should grow.
	for i := 0; i < 5; i++ {
		f.Report(host, 2*time.Second, true)
	}
	grown := f.effectiveInterval(host)
	if grown <= base {
		t.Errorf("effectiveInterval did not grow under errors: base=%v grown=%v", base, grown)
	}
}

// TestFrontierLowerRateAppliesToLiveAcquire pins the #82 regression: a VERIFIED
// site's live per-host limiter must actually DROP to the verified rate when
// SetHostRate lowers it — including for a request that is ALREADY blocked in
// Acquire on the old (throttled) reservation. The root cause was rate.Limiter.
// SetLimit only changing the future accrual rate; it does NOT cancel an in-flight
// reservation, so a goroutine blocked on a ~60s reservation stayed blocked the full
// ~60s and only a daemon RESTART (a fresh frontier with no stale reservation) ever
// applied the verified rate. The fix REPLACES the limiter with a fresh full-bucket
// one when the effective spacing is lowered, so the next acquire proceeds at the
// verified cadence.
//
// Falsifiable: with the old SetLimit-only code the second Acquire waits the full
// throttled interval (~60s here) and times out under the 2s context deadline; with
// the limiter-replace fix it returns promptly.
func TestFrontierLowerRateAppliesToLiveAcquire(t *testing.T) {
	const throttled = 60 * time.Second
	// Above the 250ms config.MinPerHostRate sanity floor so it is applied verbatim
	// (not clamped up) — the point is the live limiter drops from 60s to a fast rate.
	const verified = 500 * time.Millisecond
	f := New(Options{PerHostRate: throttled, PerHostConcurrency: 4})
	host := "verified.example"

	// First acquire is immediate (full bucket) and consumes the token.
	rel, err := f.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("first Acquire error = %v", err)
	}
	rel()

	// A second acquire now blocks on a ~60s reservation computed at the throttled
	// rate. It uses a long-lived context (context.Background) exactly like a real
	// crawl goroutine — NOT a short deadline (a short deadline would make rate.Wait
	// refuse the 60s reservation up front and never actually park, which is not the
	// slow-reservation scenario). Launch it concurrently so the reservation is genuinely in
	// flight when we lower the rate (the steady state of a 60s-throttled crawl). The
	// goroutine signals `entered` right before it calls Acquire so we lower the rate
	// only once the second acquire is on its way into Wait.
	blocked := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		close(entered)
		r, e := f.Acquire(context.Background(), host)
		if e == nil {
			r()
		}
		blocked <- e
	}()

	// Wait until the goroutine is about to acquire, then yield a few times so it
	// reaches limiter.Wait and takes the stale ~60s reservation before we lower.
	<-entered
	for i := 0; i < 50; i++ {
		runtime.Gosched()
	}

	// Verify promotion: lower the live limiter to the verified rate. After this the
	// blocked acquire must complete promptly — the rerate signal wakes it to retry on
	// the fresh fast limiter. With the old SetLimit-only code the goroutine stays
	// parked on its stale ~60s reservation and the test's outer 5s budget elapses.
	f.SetHostRate(host, verified)

	select {
	case e := <-blocked:
		if e != nil {
			t.Fatalf("blocked Acquire returned an error after lowering the rate: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Acquire never returned after lowering the rate " +
			"(stale ~60s reservation was not cleared — the live limiter did not drop to the verified rate)")
	}

	// Sanity: the effective spacing now reflects the verified rate, not the throttle.
	if got := f.effectiveInterval(host); got != verified {
		t.Fatalf("after lowering: effectiveInterval = %v, want %v", got, verified)
	}
}

// TestFrontierLowerRateNeverWeakensPolitenessFloor pins LESSON 1: the limiter-
// replace on a LOWER must never let a site exceed a robots Crawl-delay floor that
// is larger than the requested rate. A promotion that clears the host rate (or sets
// it below the floor) must keep the inviolable robots floor — the fresh limiter is
// built at effectiveEvery (max(base*throttle, robotsFloor)), not the raw requested
// rate.
func TestFrontierLowerRateNeverWeakensPolitenessFloor(t *testing.T) {
	f := New(Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 4})
	host := "crawldelay-live.example"

	// A robots Crawl-delay floor of 5s is advertised, and the host is throttled.
	f.SetMinInterval(host, 5*time.Second)
	f.SetHostRate(host, 60*time.Second)

	// First acquire is immediate; the second would block.
	rel, err := f.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("first Acquire error = %v", err)
	}
	rel()

	// "Promotion": clear the host rate. The effective spacing must NOT drop below
	// the 5s robots floor (politeness is sacred), even though we just rebuilt the
	// limiter on the lower path — the fresh limiter is built at effectiveEvery
	// (max(base*throttle, robotsFloor)), so the floor still governs.
	f.SetHostRate(host, 0)
	if got := f.effectiveInterval(host); got < 5*time.Second {
		t.Fatalf("after promotion: effectiveInterval = %v, want >= 5s (robots Crawl-delay floor is inviolable)", got)
	}

	// And the live SPACING between requests must respect the 5s floor. The fresh
	// limiter starts with one token (one immediate request is fine — the operator
	// just proved control), but the NEXT request must be spaced by >= 5s: an acquire
	// with a sub-floor deadline must time out. This is the politeness guard — the
	// rebuild can shorten a stale slow wait but never lets a host hammer below its
	// Crawl-delay floor.
	first, ferr := f.Acquire(context.Background(), host)
	if ferr != nil {
		t.Fatalf("first post-promotion Acquire error = %v", ferr)
	}
	first()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, e := f.Acquire(ctx, host); e == nil {
		t.Fatal("second acquire succeeded within 500ms despite a 5s robots Crawl-delay floor — politeness weakened")
	}
}
