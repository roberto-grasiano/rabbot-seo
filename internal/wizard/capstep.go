package wizard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/humanize"
	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// capChoice is the operator's decision when the cap step fires (Spec B D6).
type capChoice int

const (
	capKeep capChoice = iota // keep the resolved default (2000) — NO write
	capAll                   // monitor all — write 0 (unlimited)
	capSetN                  // cap at a specific N (validated ≥ 0)
)

// capChoiceToPtr maps a (choice, set-N text) pair onto the three-state
// SiteDraft.MaxPages pointer (Spec B D6 write semantics):
//
//	capKeep → nil  (no write; resolved default stands),
//	capAll  → &0   (unlimited),
//	capSetN → &N   (N parsed from setN, validated ≥ 0).
//
// A non-numeric or negative setN is rejected (the form's Validate also guards it,
// but the mapping is defensive so a bad value never silently writes garbage).
func capChoiceToPtr(choice capChoice, setN string) (*int, error) {
	switch choice {
	case capKeep:
		return nil, nil
	case capAll:
		zero := 0
		return &zero, nil
	case capSetN:
		if err := validateMaxPagesField(setN); err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(setN) // validated above
		return &n, nil
	default:
		return nil, fmt.Errorf("unknown cap choice %d", int(choice))
	}
}

// validateMaxPagesField is the "set a number" field validator: a base-10
// non-negative integer, nothing else. Rejects the empty string, signs, decimals,
// and any non-digit so the form never accepts an absurd or non-numeric cap (Spec B
// error-handling). strconv.Atoi rejects "1 000", "1.5", "abc", "-1", and "".
func validateMaxPagesField(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return errors.New("enter a whole number of pages (0 = monitor everything)")
	}
	if n < 0 {
		return errors.New("page count cannot be negative")
	}
	return nil
}

// humanDurationW renders a Duration as a compact "Xh Ym" / "Ym Zs" / "Zs" string. It
// delegates to the shared humanize.Duration (the single implementation, also used by
// cli.humanDuration) so the TUI cap line and the post-Apply coverage line read
// identically. The wizard cannot import cli, so the shared logic lives in the leaf
// package internal/humanize.
func humanDurationW(d time.Duration) string {
	return humanize.Duration(d)
}

// formatMB renders a megabyte figure to one decimal.
func formatMB(approxBytes int64) string {
	return fmt.Sprintf("%.1f MB", float64(approxBytes)/(1024*1024))
}

// capStepPrompt is the explanatory body shown above the keep/all/set-N choices when
// the step fires. It states the estimated page count vs the cap and labels the source
// honestly (Spec B D4/D6): a sitemap count says "estimated from sitemap.xml" and shows
// the single number; a ballpark range names the range. The 50,000+ bucket (low==high
// ==50000 with OperatorBallpark) reads as "50,000 or more".
func capStepPrompt(plan setup.CapPlan) string {
	if plan.Source == setup.SitemapEstimate {
		return fmt.Sprintf(
			"This site looks like ~%d pages (estimated from sitemap.xml), but the default "+
				"only watches the first %d. Right-size your coverage:",
			plan.PagesHigh, plan.EffectiveCap)
	}
	if plan.PagesLow == plan.PagesHigh {
		return fmt.Sprintf(
			"You said about %d or more pages, but the default only watches the first %d. "+
				"Right-size your coverage:",
			plan.PagesLow, plan.EffectiveCap)
	}
	return fmt.Sprintf(
		"You said about %d–%d pages, but the default only watches the first %d. "+
			"Right-size your coverage:",
		plan.PagesLow, plan.PagesHigh, plan.EffectiveCap)
}

// capStepAllLine is the "monitor everything" consequence line: the full-pass time and
// on-disk size from the engine's own estimator (coverage.Estimate via PlanCap). For an
// exact count (PagesLow==PagesHigh) the low/high collapse to a single figure; the open-
// ended 50,000+ bucket (an OperatorBallpark with low==high==50000) renders that single
// figure as a FLOOR ("≈ X+"); a finite range renders "low – high" for BOTH time and
// disk (Spec B D7).
//
// HONESTY: the plan is computed at verify.StateVerified, i.e. the BEST achievable rate,
// so the full-pass figure assumes verified ownership; an unverified site is throttled and
// will take longer. The qualifier states that in one phrase so the estimate is not read
// as a guarantee for an unverified site.
func capStepAllLine(plan setup.CapPlan) string {
	const verifiedQualifier = " (at verified speed)"
	if plan.PagesLow == plan.PagesHigh {
		suffix := ""
		if plan.Source == setup.OperatorBallpark {
			suffix = "+" // open-ended 50,000+ bucket → a floor, not a precise figure
		}
		return fmt.Sprintf("Monitor all: full pass ≈ %s%s · ~%s%s on disk%s.",
			humanDurationW(plan.AllPassHigh.FullPass), suffix,
			formatMB(plan.AllPassHigh.ApproxBytes), suffix, verifiedQualifier)
	}
	return fmt.Sprintf("Monitor all: full pass ≈ %s – %s · ~%s – %s on disk%s.",
		humanDurationW(plan.AllPassLow.FullPass), humanDurationW(plan.AllPassHigh.FullPass),
		formatMB(plan.AllPassLow.ApproxBytes), formatMB(plan.AllPassHigh.ApproxBytes), verifiedQualifier)
}

// capState is the concurrency-safe bridge between the background count goroutine and
// the lazily-evaluated huh predicates (WithHideFunc/DescriptionFunc). It keys the count
// by URL so a result for a URL the operator has since edited is discarded (stale-count
// guard). All access is mutex-guarded because the goroutine writes (record) while the
// bubbletea render loop reads (snapshot/sitemapPlan).
type capState struct {
	mu    sync.Mutex
	url   string // the URL the count is being / was gathered for
	count int
	ok    bool
	ready bool // a result has landed for the CURRENT url
}

// setURL marks the current site URL and reports whether it CHANGED. It is idempotent:
// re-setting the same URL is a no-op (changed=false) and leaves any landed result in
// place, so the form can call it on every render and start the background count exactly
// ONCE per distinct URL (on a changed=true). A real change invalidates any prior result
// (ready=false) so a stale count is never shown for the new URL.
func (s *capState) setURL(url string) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url == s.url {
		return false
	}
	s.url = url
	s.count = 0
	s.ok = false
	s.ready = false
	return true
}

// record stores a count result, but ONLY if it is for the current URL (a late result
// for a since-edited URL is dropped).
func (s *capState) record(url string, count int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url != s.url {
		return
	}
	s.count = count
	s.ok = ok
	s.ready = true
}

// snapshot returns the current (count, ok, ready) under the lock. ready=false means the
// background count for the current URL has not landed yet (the form shows "estimating…").
func (s *capState) snapshot() (count int, ok bool, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.ok, s.ready
}

// capCountPhase is the THREE-state machine the cap step's reveal logic keys off (Item C).
// It collapses the (ok, ready) snapshot into the single axis that drives which group is
// on screen, so the reading-note hold, the ranged ballpark question, and the cap choices
// each reveal off ONE explicit state rather than off ad-hoc ready/ok combinations.
type capCountPhase int

const (
	// phaseCounting: the background sitemap count is in flight (not ready). The cap step
	// shows ONLY the "⏳ Reading sitemap.xml…" hold; neither the estimate nor the ranged
	// inputs are presented yet (finding #1 — the ranged question must not show prematurely).
	phaseCounting capCountPhase = iota
	// phaseOK: a usable sitemap count landed (ready && ok). The cap-choices step drives
	// (it fires only when the count exceeds the cap; an under-cap count asks nothing).
	phaseOK
	// phaseFailed: the count resolved with no usable sitemap (ready && !ok — missing,
	// broken, or timed out). The ranged ballpark question is the honest fallback.
	phaseFailed
)

// countPhase resolves the current three-state count phase from the snapshot. It is the
// single source of truth the reveal predicates (readingGateActive / rangedQuestionVisible)
// and capStepFires read, so the reading-note hold, the ranged question, and the cap
// choices can never disagree about which beat the operator is on.
func countPhase(cs *capState) capCountPhase {
	_, ok, ready := cs.snapshot()
	switch {
	case !ready:
		return phaseCounting
	case ok:
		return phaseOK
	default:
		return phaseFailed
	}
}

// readingGateActive reports whether the cap step should show ONLY the "⏳ Reading
// sitemap.xml…" hold (Item C). It is active exactly while the count is in flight
// (phaseCounting); both the ranged question and the cap choices are suppressed behind
// the note until the count resolves.
func readingGateActive(cs *capState) bool {
	return countPhase(cs) == phaseCounting
}

// rangedQuestionVisible reports whether the ranged ballpark question should be shown.
// It is visible ONLY once the count has genuinely failed/timed out (phaseFailed): while
// counting it stays hidden behind the reading note (finding #1 fix), and on a usable
// count (phaseOK) the cap-choices step drives instead.
func rangedQuestionVisible(cs *capState) bool {
	return countPhase(cs) == phaseFailed
}

// sitemapPlan builds the CapPlan for the sitemap-estimate branch and reports whether
// that branch fires. It fires only when a count has landed (ready), the sitemap was
// usable (ok), and PlanCap says the count exceeds the cap. A not-ready or !ok count →
// fires=false, and the form routes to the estimating beat / ranged question.
func (s *capState) sitemapPlan(cfg *config.Config, site config.SiteConfig) (setup.CapPlan, bool) {
	count, ok, ready := s.snapshot()
	if !ready || !ok {
		return setup.CapPlan{}, false
	}
	plan := setup.PlanCap(cfg, site, verify.StateVerified, count, count, setup.SitemapEstimate)
	return plan, plan.Fires
}

// rangedPlan builds the CapPlan for the operator-ballpark branch from a selected bucket
// LABEL (resolved via setup.BallparkByLabel) and reports whether the cap choices should
// appear. An unrecognized label, or a no-count bucket (Under 1,000 / Not sure, bounds
// (0,0)), yields a plan whose HIGH is below the cap so Fires is false — the clean path.
func rangedPlan(label string, cfg *config.Config, site config.SiteConfig) (setup.CapPlan, bool) {
	b, ok := setup.BallparkByLabel(label)
	if !ok {
		return setup.CapPlan{}, false
	}
	low, high := b.Bounds()
	plan := setup.PlanCap(cfg, site, verify.StateVerified, low, high, setup.OperatorBallpark)
	return plan, plan.Fires
}

// activeCapPlan returns the CapPlan currently driving the cap choices: the sitemap plan
// when the background count produced a usable over-cap number, otherwise the ranged plan
// for the selected ballpark bucket. It is the single source the cap-step copy builders
// read so the title/consequence lines always match the live branch.
func activeCapPlan(cs *capState, rangedBucket string, cfg *config.Config, site config.SiteConfig) setup.CapPlan {
	if plan, fires := cs.sitemapPlan(cfg, site); fires {
		return plan
	}
	plan, _ := rangedPlan(rangedBucket, cfg, site)
	return plan
}

// capStepFires reports whether the cap choices group should be shown at all: either the
// sitemap branch fired, or the chosen ballpark bucket fired. A usable sitemap count that
// landed UNDER the cap (countLanded but the sitemap plan does not fire) is the essential
// path — nothing to ask — so neither this nor the ranged group shows.
func capStepFires(cs *capState, rangedBucket string, cfg *config.Config, site config.SiteConfig) bool {
	// Item C: while the count is in flight, the reading-note hold is the ONLY thing on
	// screen — neither the estimate nor the ranged inputs may fire yet, regardless of the
	// (default) ballpark bucket. Gate the whole step on the resolved count.
	if readingGateActive(cs) {
		return false
	}
	if _, fires := cs.sitemapPlan(cfg, site); fires {
		return true
	}
	// A usable count landed but did NOT exceed the cap → the operator is never asked.
	// Suppress the ranged plan's vote so a small COUNTED site matches the main essential
	// flow (it never falls through to the ballpark question).
	if countLanded(cs) {
		return false
	}
	_, fires := rangedPlan(rangedBucket, cfg, site)
	return fires
}

// countLanded reports whether a USABLE sitemap count has landed for the current URL
// (ready && ok). It is the predicate that distinguishes "we counted this site" (hide the
// ranged ballpark question entirely — there is a real number, whether it fires the cap
// step or not) from "no sitemap / still counting" (ask the ballpark question). A counted
// small site (ready && ok && the count is under the cap) therefore shows NEITHER the
// ranged nor the cap group, exactly like the main essential flow.
func countLanded(cs *capState) bool {
	_, ok, ready := cs.snapshot()
	return ready && ok
}

// resolveCapDraft is the single mapping from the cap step's collected state to the *int
// carried on SiteDraft.MaxPages. If the step never fired, the operator was never asked,
// so the resolved default stands (nil). Otherwise the recorded choice maps through
// capChoiceToPtr.
func resolveCapDraft(fired bool, choice capChoice, setN string) (*int, error) {
	if !fired {
		return nil, nil
	}
	return capChoiceToPtr(choice, setN)
}

// startCount launches the bounded sitemap count in a goroutine and records the result
// against capState (URL-keyed, so a stale result for a since-edited URL is dropped by
// record). It returns a done channel the live form's defer waits on so the goroutine can
// never outlive the form. The CountPages seam itself is ctx-aware (production:
// cli.productionCountPages → countSitemapPages, whose ~12s budget derives from this ctx),
// so cancelling ctx unblocks it; this launcher adds no second timeout. A nil seam (tests
// not exercising the branch) records a !ok result immediately so the form routes straight
// to the ranged question.
func startCount(ctx context.Context, cs *capState, url string, count func(ctx context.Context, url string) (int, bool)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if count == nil {
			cs.record(url, 0, false)
			return
		}
		n, ok := count(ctx, url)
		cs.record(url, n, ok)
	}()
	return done
}

// rangedBucketOptions builds the huh select options for the ballpark question, rendered
// FROM setup.BallparkOrder + Label() (the buckets are owned by package setup; the wizard
// defines none). "Under 1,000" (first in BallparkOrder) is pre-selected as the
// one-keystroke dismissal. Option values are the labels, resolved back via
// setup.BallparkByLabel.
func rangedBucketOptions() []huh.Option[string] {
	opts := make([]huh.Option[string], 0, len(setup.BallparkOrder))
	for i, b := range setup.BallparkOrder {
		o := huh.NewOption(b.Label(), b.Label())
		if i == 0 {
			o = o.Selected(true)
		}
		opts = append(opts, o)
	}
	return opts
}

// estimatingNote is the D4 "estimating…" beat shown in the ranged question's description.
// It has three honest states, keyed off snapshot():
//
//   - not ready          → the background sitemap count is still in flight ("estimating…").
//   - ready && !ok       → no usable sitemap (missing/broken/slow) → the ballpark keeps
//     coverage right-sized; this is the ONLY state where the ranged question is genuinely
//     shown, so the "couldn't read a sitemap" copy is truthful.
//   - ready && ok        → the site WAS counted. The ranged group's WithHideFunc hides it
//     in this state (a real number landed — countLanded), so this branch is reached only
//     on a transient render; it must NOT claim the sitemap was unreadable.
func estimatingNote(cs *capState) string {
	_, ok, ready := cs.snapshot()
	switch {
	case !ready:
		return "Estimating from sitemap.xml… you can answer below if it's taking a moment."
	case ok:
		// A usable count landed (the ranged group is hidden in this state); never claim
		// the sitemap was unreadable.
		return "Counted from sitemap.xml."
	default:
		return "We couldn't read a sitemap, so a ballpark keeps your coverage right-sized."
	}
}

// maybeStartCount is the render-time trigger the form's DescriptionFunc calls: it starts
// the background count for url ONLY when url is new (cs.setURL reports changed), so huh's
// repeated lazy re-evaluation never spawns more than one goroutine per distinct URL. It
// returns the started goroutine's done channel (or nil when nothing started) so the
// form's defer can drain it. The count is NEVER started from a Validate callback (huh
// re-runs Validate on every commit/back-nav); this idempotent setURL gate is why.
func maybeStartCount(ctx context.Context, cs *capState, url string, count func(ctx context.Context, url string) (int, bool)) <-chan struct{} {
	if !cs.setURL(url) {
		return nil
	}
	return startCount(ctx, cs, url, count)
}
