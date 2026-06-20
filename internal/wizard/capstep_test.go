package wizard

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestCapChoiceToPtr(t *testing.T) {
	cases := []struct {
		name    string
		choice  capChoice
		setN    string
		wantNil bool
		wantVal int
		wantErr bool
	}{
		{"keep default", capKeep, "", true, 0, false},
		{"monitor all", capAll, "", false, 0, false},
		{"set 500", capSetN, "500", false, 500, false},
		{"set 0 via N is unlimited", capSetN, "0", false, 0, false},
		{"set negative rejected", capSetN, "-3", false, 0, true},
		{"set non-numeric rejected", capSetN, "lots", false, 0, true},
		{"set empty rejected", capSetN, "", false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := capChoiceToPtr(tc.choice, tc.setN)
			if (err != nil) != tc.wantErr {
				t.Fatalf("capChoiceToPtr err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.wantNil {
				if got != nil {
					t.Errorf("capChoiceToPtr = %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("capChoiceToPtr = nil, want &%d", tc.wantVal)
			}
			if *got != tc.wantVal {
				t.Errorf("capChoiceToPtr = &%d, want &%d", *got, tc.wantVal)
			}
		})
	}
}

func TestValidateMaxPagesField(t *testing.T) {
	for _, ok := range []string{"0", "500", "100000"} {
		if err := validateMaxPagesField(ok); err != nil {
			t.Errorf("validateMaxPagesField(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-1", "1.5", "abc", "1 000"} {
		if err := validateMaxPagesField(bad); err == nil {
			t.Errorf("validateMaxPagesField(%q) = nil, want error", bad)
		}
	}
}

// Helper: build a CapPlan via the real planner core at the default 2s rate / 2000 cap.
func capPlanFor(low, high int, src setup.Source) setup.CapPlan {
	cfg := &config.Config{}
	site := config.SiteConfig{URL: "https://example.com"}
	return setup.PlanCap(cfg, site, verify.StateVerified, low, high, src)
}

// TestCapStepPromptExactCount: a sitemap-counted plan (PagesLow==PagesHigh) renders a
// SINGLE "monitor all" figure (low==high collapse) and labels the source an estimate.
func TestCapStepPromptExactCount(t *testing.T) {
	plan := capPlanFor(10000, 10000, setup.SitemapEstimate)
	if !plan.Fires {
		t.Fatalf("10000 pages vs cap 2000 must fire")
	}
	body := capStepPrompt(plan)
	if !strings.Contains(body, "estimated from sitemap.xml") {
		t.Errorf("exact-count prompt must label the source an estimate:\n%s", body)
	}
	all := capStepAllLine(plan)
	// Single figure: no ranged "low – high" dash between two durations.
	if strings.Contains(all, humanDurationW(plan.AllPassLow.FullPass)+" – "+humanDurationW(plan.AllPassHigh.FullPass)) {
		t.Errorf("exact count must not render a ranged duration:\n%s", all)
	}
	if !strings.Contains(all, humanDurationW(plan.AllPassLow.FullPass)) {
		t.Errorf("all-line missing the full-pass figure:\n%s", all)
	}
}

// TestCapStepPromptRanged: an OperatorBallpark range renders a RANGE (low–high) for
// both time and disk (Spec B D7 — never a single midpoint).
func TestCapStepPromptRanged(t *testing.T) {
	low, high := setup.Ballpark10kTo20k.Bounds()
	plan := capPlanFor(low, high, setup.OperatorBallpark)
	if !plan.Fires {
		t.Fatalf("10k–20k vs cap 2000 must fire")
	}
	all := capStepAllLine(plan)
	if !strings.Contains(all, humanDurationW(plan.AllPassLow.FullPass)+" – "+humanDurationW(plan.AllPassHigh.FullPass)) {
		t.Errorf("ranged all-line must show low – high full pass:\n%s", all)
	}
}

// TestCapStepAllLineFloor: the 50,000+ bucket (bounds (50000,50000)) collapses to a
// single figure rendered as a FLOOR ("≈ X+") — never a precise finite number.
func TestCapStepAllLineFloor(t *testing.T) {
	low, high := setup.Ballpark50kPlus.Bounds()
	plan := capPlanFor(low, high, setup.OperatorBallpark)
	if low != 50000 || high != 50000 {
		t.Fatalf("50k+ bounds = (%d,%d), want (50000,50000)", low, high)
	}
	all := capStepAllLine(plan)
	if !strings.Contains(all, humanDurationW(plan.AllPassHigh.FullPass)+"+") {
		t.Errorf("50,000+ all-line must render a floor (≈ X+):\n%s", all)
	}
}

// TestHumanDurationW pins the wizard-local compact duration formatter.
func TestHumanDurationW(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "<1s"},
		{90 * time.Second, "1m 30s"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
	}
	for _, tc := range cases {
		if got := humanDurationW(tc.d); got != tc.want {
			t.Errorf("humanDurationW(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

var _ = coverage.Result{} // import guard until coverage is referenced directly

// TestCapStateKeyedByURL: a result recorded for one URL is visible only for that URL;
// a result whose URL no longer matches the current site is discarded (stale-count
// guard). setURL is idempotent: re-setting the same URL does NOT reset a landed result.
func TestCapStateKeyedByURL(t *testing.T) {
	cs := &capState{}
	if changed := cs.setURL("https://a.example"); !changed {
		t.Fatalf("first setURL must report changed=true")
	}
	if changed := cs.setURL("https://a.example"); changed {
		t.Fatalf("re-setURL with the SAME url must be a no-op (changed=false)")
	}
	cs.record("https://a.example", 10000, true)
	if got, ok, ready := cs.snapshot(); !ready || !ok || got != 10000 {
		t.Fatalf("snapshot = (%d,%v,ready=%v), want (10000,true,true)", got, ok, ready)
	}
	// Setting the SAME url again must NOT clear the landed result.
	cs.setURL("https://a.example")
	if _, _, ready := cs.snapshot(); !ready {
		t.Errorf("idempotent setURL must not clear a landed result")
	}
	// URL changes → previous count is stale; snapshot reports not-ready again.
	if changed := cs.setURL("https://b.example"); !changed {
		t.Fatalf("changing the url must report changed=true")
	}
	if _, _, ready := cs.snapshot(); ready {
		t.Errorf("after URL change snapshot must report not-ready (stale discarded)")
	}
	// A late result for the OLD url is ignored.
	cs.record("https://a.example", 99, true)
	if _, _, ready := cs.snapshot(); ready {
		t.Errorf("late result for old URL must be ignored")
	}
}

// TestCapStateSitemapPlan: sitemapPlan returns (plan, fires) only when a usable
// sitemap count is in AND it fires; a not-ready or !ok count yields fires=false (the
// form falls through to the ranged question / estimating beat).
func TestCapStateSitemapPlan(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}

	cs := &capState{}
	cs.setURL("https://big.example")
	// Not ready yet → no sitemap plan.
	if _, fires := cs.sitemapPlan(cfg, site); fires {
		t.Errorf("not-ready count must not fire the sitemap branch")
	}
	// ok with a big count → fires, with SitemapEstimate source.
	cs.record("https://big.example", 10000, true)
	plan, fires := cs.sitemapPlan(cfg, site)
	if !fires {
		t.Fatalf("10000 vs 2000 must fire")
	}
	if plan.Source != setup.SitemapEstimate || plan.PagesHigh != 10000 {
		t.Errorf("sitemap plan = %+v, want SitemapEstimate/10000", plan)
	}

	// ok with a small count → does NOT fire (clean path, no step).
	cs2 := &capState{}
	cs2.setURL("https://small.example")
	cs2.record("https://small.example", 50, true)
	if _, fires := cs2.sitemapPlan(cfg, site); fires {
		t.Errorf("50 pages vs 2000 must NOT fire")
	}

	// !ok (no sitemap) → sitemap branch never fires.
	cs3 := &capState{}
	cs3.setURL("https://nositemap.example")
	cs3.record("https://nositemap.example", 0, false)
	if _, fires := cs3.sitemapPlan(cfg, site); fires {
		t.Errorf("!ok count must not fire the sitemap branch")
	}
}

var _ = sync.Mutex{} // capState uses a mutex internally

// TestRangedPlan: a chosen bucket whose HIGH exceeds the cap fires (ranged plan); Under
// 1,000 / Not sure never fire; an unknown label never fires.
func TestRangedPlan(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}

	plan, fires := rangedPlan("10,000 – 20,000", cfg, site)
	if !fires {
		t.Fatalf("10k–20k must fire vs cap 2000")
	}
	if plan.Source != setup.OperatorBallpark || plan.PagesLow != 10000 || plan.PagesHigh != 20000 {
		t.Errorf("ranged plan = %+v, want OperatorBallpark/10000/20000", plan)
	}
	if _, fires := rangedPlan("Under 1,000", cfg, site); fires {
		t.Errorf("Under 1,000 must NOT fire")
	}
	if _, fires := rangedPlan("Not sure", cfg, site); fires {
		t.Errorf("Not sure must NOT fire")
	}
	if _, fires := rangedPlan("1,000 – 5,000", cfg, site); !fires {
		t.Errorf("1k–5k must fire at default cap 2000")
	}
	if _, fires := rangedPlan("bogus-label", cfg, site); fires {
		t.Errorf("an unknown label must NOT fire")
	}
}

// TestActiveCapPlanAndFires: the active plan prefers a fired sitemap count, else the
// ranged plan for the bucket; capStepFires is true when EITHER branch fires.
func TestActiveCapPlanAndFires(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}

	// Sitemap fires → active plan is the sitemap plan; capStepFires true.
	cs := &capState{}
	cs.setURL("https://x.example")
	cs.record("https://x.example", 8000, true)
	got := activeCapPlan(cs, "Under 1,000", cfg, site)
	if got.Source != setup.SitemapEstimate || got.PagesHigh != 8000 {
		t.Errorf("active plan = %+v, want sitemap/8000", got)
	}
	if !capStepFires(cs, "Under 1,000", cfg, site) {
		t.Error("sitemap over-cap count must fire the step")
	}

	// No usable sitemap → ranged plan; small bucket does NOT fire.
	cs2 := &capState{}
	cs2.setURL("https://y.example")
	cs2.record("https://y.example", 0, false)
	got2 := activeCapPlan(cs2, "10,000 – 20,000", cfg, site)
	if got2.Source != setup.OperatorBallpark || got2.PagesHigh != 20000 {
		t.Errorf("active plan = %+v, want ballpark/20000", got2)
	}
	if capStepFires(cs2, "Under 1,000", cfg, site) {
		t.Error("no sitemap + Under-1,000 bucket must NOT fire")
	}
	if !capStepFires(cs2, "20,000 – 50,000", cfg, site) {
		t.Error("no sitemap + big bucket must fire")
	}
}

// TestResolveCapDraft: end-to-end mapping from the step's collected fields to the
// carried *int, covering keep/all/set-N and the not-fired case.
func TestResolveCapDraft(t *testing.T) {
	cases := []struct {
		name    string
		fired   bool
		choice  capChoice
		setN    string
		wantNil bool
		wantVal int
	}{
		{"step did not fire", false, capKeep, "", true, 0},
		{"fired, keep", true, capKeep, "", true, 0},
		{"fired, all", true, capAll, "", false, 0},
		{"fired, set 750", true, capSetN, "750", false, 750},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCapDraft(tc.fired, tc.choice, tc.setN)
			if err != nil {
				t.Fatalf("resolveCapDraft: %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Errorf("got &%d, want nil", *got)
				}
				return
			}
			if got == nil || *got != tc.wantVal {
				t.Errorf("got %v, want &%d", got, tc.wantVal)
			}
		})
	}
}

// TestStartCountCancels asserts the background-count launcher honors ctx cancellation
// and never blocks: a CountPages seam that respects ctx returns promptly once the parent
// ctx is cancelled, and the recorded result is gated by the URL key. This is the
// goroutine-lifecycle contract the live Run relies on (its defer-cancel).
func TestStartCountCancels(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")

	started := make(chan struct{})
	release := make(chan struct{})
	count := func(ctx context.Context, url string) (int, bool) {
		close(started)
		select {
		case <-ctx.Done():
			return 0, false
		case <-release:
			return 12345, true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startCount(ctx, cs, "https://x.example", count)
	<-started
	cancel()
	<-done // must return promptly; a leak/block hangs the test (-race/-timeout catches it)

	if _, ok, ready := cs.snapshot(); ready && ok {
		t.Errorf("cancelled count must not record a usable result; got ok=%v ready=%v", ok, ready)
	}
	_ = release
}

// TestStartCountNilSeam: a nil CountPages seam records !ok immediately so the form
// routes straight to the ranged question.
func TestStartCountNilSeam(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")
	<-startCount(context.Background(), cs, "https://x.example", nil)
	if _, ok, ready := cs.snapshot(); !ready || ok {
		t.Errorf("nil seam must record (ok=false, ready=true); got ok=%v ready=%v", ok, ready)
	}
}

// TestRangedBucketOptionsFromSetup: the select options are built from setup.BallparkOrder
// + Label() (NOT a wizard-local list), Under 1,000 first.
func TestRangedBucketOptionsFromSetup(t *testing.T) {
	opts := rangedBucketOptions()
	if len(opts) != len(setup.BallparkOrder) {
		t.Fatalf("options len = %d, want %d (one per setup.BallparkOrder)", len(opts), len(setup.BallparkOrder))
	}
	for i, b := range setup.BallparkOrder {
		if opts[i].Key != b.Label() {
			t.Errorf("option[%d].Key = %q, want %q", i, opts[i].Key, b.Label())
		}
	}
}

func TestEstimatingNote(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")
	if note := estimatingNote(cs); !strings.Contains(note, "Estimating") {
		t.Errorf("not-ready note must say Estimating; got %q", note)
	}
	// ready && !ok (no sitemap) → the ballpark-keeps-coverage line, which is the only
	// state where claiming the sitemap was unreadable is truthful.
	cs.record("https://x.example", 0, false)
	note := estimatingNote(cs)
	if strings.Contains(note, "Estimating") {
		t.Errorf("ready note must drop the Estimating beat; got %q", note)
	}
	if !strings.Contains(note, "couldn't read a sitemap") {
		t.Errorf("ready && !ok note must say the sitemap was unreadable; got %q", note)
	}
	// ready && ok (a usable count landed) → must NOT claim the sitemap was unreadable
	// (Finding #4: the false "couldn't read a sitemap" message on a counted site).
	cs2 := &capState{}
	cs2.setURL("https://counted.example")
	cs2.record("https://counted.example", 42, true)
	if okNote := estimatingNote(cs2); strings.Contains(okNote, "couldn't read a sitemap") {
		t.Errorf("ready && ok note must NOT claim the sitemap was unreadable; got %q", okNote)
	}
}

// TestCountLanded pins the predicate behind Finding #4's ranged-group hide decision:
// countLanded is true ONLY when a usable count has landed (ready && ok), false while the
// count is in flight (not ready) and when the sitemap was unusable (ready && !ok).
func TestCountLanded(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")
	if countLanded(cs) {
		t.Error("not-ready count must not be countLanded")
	}
	cs.record("https://x.example", 50, true)
	if !countLanded(cs) {
		t.Error("ready && ok count must be countLanded")
	}

	cs2 := &capState{}
	cs2.setURL("https://nositemap.example")
	cs2.record("https://nositemap.example", 0, false)
	if countLanded(cs2) {
		t.Error("ready && !ok (no sitemap) must NOT be countLanded")
	}
}

// TestCapStepEssentialPathForCountedSmallSite is the Finding #4 regression: a site whose
// sitemap was COUNTED and lands UNDER the cap must hide BOTH the ranged ballpark question
// and the cap choices — the essential path is preserved, exactly like the main flow. A
// no-sitemap site shows the ranged question; a counted LARGE (over-cap) site shows the
// cap choices.
func TestCapStepEssentialPathForCountedSmallSite(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}
	const anyBucket = "Under 1,000" // a default bucket that never fires on its own

	// Counted SMALL site (50 pages, under the 2000 cap): nothing to ask.
	small := &capState{}
	small.setURL("https://small.example")
	small.record("https://small.example", 50, true)
	if !countLanded(small) {
		t.Fatal("counted small site must report countLanded (drives the ranged-group hide)")
	}
	if capStepFires(small, anyBucket, cfg, site) {
		t.Error("counted small site must NOT fire the cap choices (essential path)")
	}

	// Even a LARGE ballpark bucket selection must not resurrect the ranged path once a
	// usable count is in: a real number beats the operator's guess.
	if capStepFires(small, "20,000 – 50,000", cfg, site) {
		t.Error("a usable counted-small result must suppress an over-cap ballpark bucket")
	}

	// No-sitemap site: the ranged question is shown (not countLanded) and the chosen big
	// bucket fires the cap choices.
	noSitemap := &capState{}
	noSitemap.setURL("https://nositemap.example")
	noSitemap.record("https://nositemap.example", 0, false)
	if countLanded(noSitemap) {
		t.Error("no-sitemap site must NOT be countLanded (ranged question stays visible)")
	}
	if !capStepFires(noSitemap, "20,000 – 50,000", cfg, site) {
		t.Error("no-sitemap site with a big ballpark bucket must fire the cap choices")
	}

	// Counted LARGE site (over the cap): the cap choices fire directly.
	large := &capState{}
	large.setURL("https://large.example")
	large.record("https://large.example", 9000, true)
	if !countLanded(large) {
		t.Fatal("counted large site must report countLanded")
	}
	if !capStepFires(large, anyBucket, cfg, site) {
		t.Error("counted large (over-cap) site must fire the cap choices")
	}
}

// TestCapStateConcurrentRecordSnapshot (Finding #9) genuinely overlaps a mid-count
// goroutine's record with hammered snapshot()/sitemapPlan() reads, so -race flags any
// unguarded capState field. A blocked startCount goroutine is released via a channel
// while a second goroutine spins on the read path, then both are joined.
func TestCapStateConcurrentRecordSnapshot(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}

	cs := &capState{}
	const url = "https://race.example"
	cs.setURL(url)

	release := make(chan struct{})
	started := make(chan struct{})
	count := func(ctx context.Context, _ string) (int, bool) {
		close(started)
		select {
		case <-ctx.Done():
			return 0, false
		case <-release:
			return 10000, true // over cap → sitemapPlan would fire once recorded
		}
	}

	done := startCount(context.Background(), cs, url, count)
	<-started // the count goroutine is now blocked mid-count, about to record

	// Hammer the read path concurrently so record() and snapshot()/sitemapPlan() overlap.
	stop := make(chan struct{})
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		for {
			select {
			case <-stop:
				return
			default:
				_, _, _ = cs.snapshot()
				_, _ = cs.sitemapPlan(cfg, site)
			}
		}
	}()

	close(release) // unblock the count → record() now runs concurrently with the reader
	<-done         // count goroutine finished recording
	close(stop)
	<-reader // reader joined

	if _, ok, ready := cs.snapshot(); !ready || !ok {
		t.Fatalf("after release the count must be recorded; got ok=%v ready=%v", ok, ready)
	}
	if _, fires := cs.sitemapPlan(cfg, site); !fires {
		t.Error("10000 pages vs cap 2000 must fire after the recorded count")
	}
}

// TestMaybeStartCountIdempotent: the render-time trigger starts the count exactly once
// per distinct URL no matter how many times huh re-evaluates the DescriptionFunc, and
// restarts it when the URL changes. It models huh calling the trigger repeatedly.
func TestMaybeStartCountIdempotent(t *testing.T) {
	cs := &capState{}
	var calls int32
	count := func(_ context.Context, _ string) (int, bool) {
		atomic.AddInt32(&calls, 1)
		return 100, true
	}
	var dones []<-chan struct{}
	start := func(url string) {
		if ch := maybeStartCount(context.Background(), cs, url, count); ch != nil {
			dones = append(dones, ch)
		}
	}
	// huh evaluates the DescriptionFunc many times for the same URL.
	for i := 0; i < 5; i++ {
		start("https://a.example")
	}
	start("https://b.example") // URL changed → one more start
	for _, ch := range dones {
		<-ch
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("count started %d times, want 2 (once per distinct URL)", got)
	}
}
