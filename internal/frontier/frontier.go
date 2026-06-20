package frontier

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// Options configures default per-host politeness.
type Options struct {
	PerHostRate        time.Duration // minimum spacing between requests to a host
	PerHostConcurrency int
}

type hostState struct {
	limiter *rate.Limiter
	sem     chan struct{}
	// rerate is closed (and replaced) whenever the limiter is REPLACED on a LOWER
	// (a verify promotion dropping the host from the unverified ~60s throttle back to
	// its verified rate — see applyEffectiveLimit). A crawl goroutine parked in
	// Acquire on the OLD limiter's stale slow reservation selects on this channel and,
	// when it closes, retries Wait on the fresh (fast) limiter — so the new rate
	// applies to an IN-FLIGHT request, not just the next fresh Acquire (#82). It is a
	// broadcast: close-then-replace under f.mu so every parked waiter wakes exactly
	// once per lower. nil only transiently before stateFor seeds it.
	rerate chan struct{}
	// baseEvery is the host's BASE per-request spacing. It is seeded from
	// f.opts.PerHostRate (the configured default) and is SETTABLE up OR down by
	// SetHostRate — the verification-aware resolver lowers it for a verified
	// speed-dialed host (e.g. ~1s at speed:200) and raises it to the >=60s floor for
	// an unverified one, both WITHOUT a daemon restart (PR31 #3). A `rabbot verify`
	// promotion lowers it back to the configured default. It is the seam the speed
	// dial drives: effectiveEvery = max(baseEvery*throttle, robotsFloor), so a base
	// BELOW the configured default actually speeds the host up.
	baseEvery time.Duration
	throttle  float64 // multiplier >= 1.0 applied to base interval
	// robotsFloor is an advertised robots.txt Crawl-delay: a RAISE-ONLY hard spacing
	// floor (a site can tighten its Crawl-delay but a smaller later value never
	// loosens it), owned by the crawl path. It is tracked separately from baseEvery
	// because it composes as an inviolable floor — lowering baseEvery on a verify
	// promotion can never drop the host below an advertised Crawl-delay on the same
	// host (effectiveEvery raises base*throttle to robotsFloor when larger).
	robotsFloor time.Duration // raise-only; zero = none
}

// effectiveEvery computes the host's effective spacing: the adaptive
// base*throttle interval, raised to the robots Crawl-delay floor. Callers must
// hold f.mu. The base is the verification-aware per-host spacing (set up OR down
// by SetHostRate); the robots Crawl-delay floor is an inviolable raise-only floor
// that wins when larger.
func effectiveEvery(hs *hostState) time.Duration {
	e := time.Duration(float64(hs.baseEvery) * hs.throttle)
	if hs.robotsFloor > e {
		e = hs.robotsFloor
	}
	return e
}

// Frontier enforces per-host rate + concurrency, a global pause flag, and an
// adaptive throttle that backs off hosts that are slow or erroring.
type Frontier struct {
	opts Options

	mu     sync.Mutex
	hosts  map[string]*hostState
	paused bool
}

// New builds a Frontier with the given defaults.
func New(opts Options) *Frontier {
	if opts.PerHostRate <= 0 {
		opts.PerHostRate = 10 * time.Second
	}
	if opts.PerHostConcurrency <= 0 {
		opts.PerHostConcurrency = 2
	}
	return &Frontier{opts: opts, hosts: map[string]*hostState{}}
}

// SetPaused toggles the global crawl kill-switch.
func (f *Frontier) SetPaused(p bool) {
	f.mu.Lock()
	f.paused = p
	f.mu.Unlock()
}

// Paused reports the global pause flag.
func (f *Frontier) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *Frontier) stateFor(host string) *hostState {
	f.mu.Lock()
	defer f.mu.Unlock()
	hs, ok := f.hosts[host]
	if !ok {
		hs = &hostState{
			limiter:   rate.NewLimiter(rate.Every(f.opts.PerHostRate), 1),
			sem:       make(chan struct{}, f.opts.PerHostConcurrency),
			baseEvery: f.opts.PerHostRate,
			throttle:  1.0,
			rerate:    make(chan struct{}),
		}
		f.hosts[host] = hs
	}
	return hs
}

// effectiveInterval reports the host's current effective spacing (the adaptive
// base*throttle interval raised to the larger of the robots Crawl-delay and the
// verification-aware throttle floor). It is unexported: the only readers are the
// frontier package's own white-box tests (the floors are otherwise observed
// end-to-end via Acquire timing). Callers must NOT hold f.mu.
func (f *Frontier) effectiveInterval(host string) time.Duration {
	hs := f.stateFor(host)
	f.mu.Lock()
	defer f.mu.Unlock()
	return effectiveEvery(hs)
}

// SetMinInterval installs a per-host robots.txt Crawl-delay spacing floor. This
// floor only ever RAISES: a smaller later d is ignored, and the adaptive throttle
// decay can never drop the host below an established floor. Safe to call before or
// after the host has been seen. It does NOT touch the throttle floor, so a
// Crawl-delay and an unverified throttle floor compose (the larger wins).
func (f *Frontier) SetMinInterval(host string, d time.Duration) {
	hs := f.stateFor(host)
	f.mu.Lock()
	defer f.mu.Unlock()
	if d > hs.robotsFloor {
		hs.robotsFloor = d
	}
	hs.limiter.SetLimit(rate.Every(effectiveEvery(hs)))
}

// SetHostRate sets the per-host BASE spacing requested by the resolver
// (ResolveCrawl) for ANY tier — verified or throttled. Unlike SetMinInterval (the
// raise-only robots Crawl-delay floor), this is SETTABLE up OR down: it lowers the
// base BELOW the configured default for a verified speed-dialed host (so the dial
// actually speeds the host up) and raises it to the >=60s unverified floor for a
// throttled one, both WITHOUT a daemon restart (PR31 #3). A `rabbot verify`
// promotion lowers the rate back to the verified base; a demotion raises it. The
// robots Crawl-delay floor is tracked separately and is never lowered here, so the
// effective spacing stays max(robotsFloor, base*throttle) — lowering the base on a
// promotion can never clobber a Crawl-delay floor on the same host. A positive d
// below config.MinPerHostRate is clamped UP to that sanity floor (spec D4) so no
// caller can make the frontier hammer a host; a non-positive d reverts the base to
// the configured default (f.opts.PerHostRate). Safe before or after the host is
// seen.
func (f *Frontier) SetHostRate(host string, d time.Duration) {
	switch {
	case d <= 0:
		d = f.opts.PerHostRate // revert to the configured default base
	case d < config.MinPerHostRate:
		d = config.MinPerHostRate
	}
	hs := f.stateFor(host)
	f.mu.Lock()
	defer f.mu.Unlock()
	hs.baseEvery = d
	applyEffectiveLimit(hs)
}

// applyEffectiveLimit re-applies hs's effective spacing to its rate.Limiter.
// Callers must hold f.mu.
//
// When the effective spacing is being LOWERED (a verify promotion drops a host
// from the unverified ~60s throttle back to its verified base), it REPLACES the
// limiter with a fresh full-token one rather than calling SetLimit. This is the
// #82 fix: rate.Limiter.SetLimit only changes the FUTURE accrual rate — per its
// own contract it "may be violated or underutilized by those which reserved
// (using Reserve or Wait) but did not yet act" — so a crawl goroutine already
// blocked in limiter.Wait holds a reservation whose wake time was computed at the
// OLD (slow) rate and stays parked the full old interval (the observed symptom:
// a verified site kept crawling at 60s spacing until a daemon RESTART gave it a
// fresh frontier). A brand-new limiter starts with a full bucket (burst 1, one
// token available immediately) and carries no stale reservation, so the next
// Acquire proceeds at the verified cadence. The fresh limiter is built at the
// EFFECTIVE spacing — max(base*throttle, robotsFloor) — so a Crawl-delay floor
// larger than the requested rate is still honoured (politeness is never weakened,
// LESSON 1). RAISING (an unverified host getting the throttle installed, or the
// adaptive back-off growing the interval) keeps using SetLimit: there is no stale
// reservation to clear and no token to hand out, so the existing behaviour stands.
func applyEffectiveLimit(hs *hostState) {
	want := effectiveEvery(hs)
	// rate.Limiter.Limit() is events/sec; convert back to a spacing to compare.
	// rate.Every(d) == Limit(1/d); 1/Limit() == d. Guard a zero/Inf limit.
	cur := time.Duration(0)
	if lim := hs.limiter.Limit(); lim > 0 {
		cur = time.Duration(float64(time.Second) / float64(lim))
	}
	if cur > 0 && want < cur {
		// Lowering: replace the limiter so any stale slow reservation is discarded
		// and the next Acquire gets an immediately-available token at the new rate.
		hs.limiter = rate.NewLimiter(rate.Every(want), 1)
		// Wake any request currently parked in Acquire on the OLD limiter's stale
		// slow reservation so it retries on the fresh limiter (it is holding a ref
		// to the old object and would otherwise drain the full old interval). Close
		// the current channel (broadcast) and install a fresh one for the next round.
		close(hs.rerate)
		hs.rerate = make(chan struct{})
		return
	}
	hs.limiter.SetLimit(rate.Every(want))
}

// Acquire blocks until the host's rate + concurrency budget allows one request,
// honoring the global pause flag. The returned func releases the concurrency slot.
func (f *Frontier) Acquire(ctx context.Context, host string) (release func(), err error) {
	// Block while globally paused. Poll on a single reused timer rather than a
	// fresh time.After each iteration: with O(MaxParallel) goroutines spinning
	// here during a pause, per-iteration timer allocation is needless GC pressure.
	if f.Paused() {
		t := time.NewTimer(20 * time.Millisecond)
		defer t.Stop()
		for f.Paused() {
			select {
			case <-ctx.Done():
				return func() {}, ctx.Err()
			case <-t.C:
				t.Reset(20 * time.Millisecond) // safe: only reached after t.C drained
			}
		}
	}

	hs := f.stateFor(host)

	select {
	case hs.sem <- struct{}{}:
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}

	if err := f.waitHost(ctx, hs); err != nil {
		<-hs.sem
		return func() {}, err
	}

	var once sync.Once
	return func() { once.Do(func() { <-hs.sem }) }, nil
}

// waitHost blocks until the host's rate limiter permits one request, honoring ctx.
// It is rate-change-aware (#82): when the limiter is REPLACED on a verify-promotion
// LOWER (applyEffectiveLimit), a request already parked here on the OLD limiter's
// stale slow reservation is woken via the host's rerate channel and retries on the
// fresh, faster limiter — so the new rate applies to an in-flight request, not just
// the next fresh Acquire. Each retry calls Wait on the current limiter, so the
// effective spacing (including any robots Crawl-delay floor baked into the fresh
// limiter) is always honoured — a rerate can only ever SHORTEN the wait, never grant
// a request faster than the new limiter allows. A rerate only fires on a lower, and
// raises/decay reuse the same limiter object (no signal), so this loops at most once
// per concurrent lower and cannot spin.
func (f *Frontier) waitHost(ctx context.Context, hs *hostState) error {
	for {
		f.mu.Lock()
		lim := hs.limiter
		rerate := hs.rerate
		f.mu.Unlock()

		// Cancel this Wait if the limiter is replaced under us (rerate closes), so we
		// retry on the fresh one rather than draining the stale slow reservation. A
		// watcher goroutine bridges the rerate channel to wctx cancellation; it also
		// exits when the wait finishes (done closes) so it never leaks. The parent ctx
		// still governs real cancellation/deadline.
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			select {
			case <-rerate:
				cancel()
			case <-done:
			}
		}()

		err := lim.Wait(wctx)
		close(done) // stop the watcher
		cancel()    // release the wctx (no-op if already cancelled)

		if err == nil {
			return nil
		}
		// If the PARENT context is done, this is a genuine cancellation/deadline.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Otherwise the wait was cancelled by a rerate (the limiter was lowered):
		// retry on the now-current (faster) limiter.
		select {
		case <-rerate:
			// rerate fired: loop and re-snapshot the fresh limiter.
		default:
			// Defensive: Wait errored without parent-ctx cancellation and without a
			// rerate (should not happen — burst is always 1 so n<=burst). Surface it
			// rather than spin.
			return err
		}
	}
}

// Report feeds back response latency + error to drive the adaptive throttle.
func (f *Frontier) Report(host string, latency time.Duration, hadError bool) {
	hs := f.stateFor(host)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case hadError || latency > 1500*time.Millisecond:
		hs.throttle *= 1.5
		if hs.throttle > 16 {
			hs.throttle = 16
		}
	default:
		hs.throttle *= 0.9
		if hs.throttle < 1 {
			hs.throttle = 1
		}
	}
	hs.limiter.SetLimit(rate.Every(effectiveEvery(hs)))
}
