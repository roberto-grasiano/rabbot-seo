package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// TestCountPhase pins the three-state count machine the cap step's reveal logic
// keys off (Item C): counting (in flight, not ready), ok (a usable sitemap count
// landed), failed (ready but no usable sitemap — missing/broken/timeout).
func TestCountPhase(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")
	if got := countPhase(cs); got != phaseCounting {
		t.Errorf("not-ready count phase = %v, want phaseCounting", got)
	}
	cs.record("https://x.example", 1234, true)
	if got := countPhase(cs); got != phaseOK {
		t.Errorf("ready && ok count phase = %v, want phaseOK", got)
	}

	cs2 := &capState{}
	cs2.setURL("https://nositemap.example")
	cs2.record("https://nositemap.example", 0, false)
	if got := countPhase(cs2); got != phaseFailed {
		t.Errorf("ready && !ok count phase = %v, want phaseFailed", got)
	}
}

// TestReadingGateActive is the heart of Item C: while the background count is in
// flight (counting), the reading-note hold is active and BOTH the ranged ballpark
// question and the cap-choices step are suppressed. Once the count resolves (ok or
// failed) the gate lifts.
func TestReadingGateActive(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://x.example")
	if !readingGateActive(cs) {
		t.Error("an in-flight count must keep the reading gate active")
	}
	cs.record("https://x.example", 9000, true)
	if readingGateActive(cs) {
		t.Error("a resolved (ok) count must lift the reading gate")
	}

	cs2 := &capState{}
	cs2.setURL("https://y.example")
	cs2.record("https://y.example", 0, false)
	if readingGateActive(cs2) {
		t.Error("a resolved (failed) count must lift the reading gate")
	}
}

// TestRangedQuestionVisible is the regression for finding #1: the ranged ballpark
// question must NOT show while the count is still in flight (counting) — it shows
// ONLY once the count has genuinely failed/timed out (failed). On a usable count
// (ok) the ranged question stays hidden (the cap-choices step drives instead).
func TestRangedQuestionVisible(t *testing.T) {
	// counting → hidden (the bug being fixed: it used to show prematurely).
	counting := &capState{}
	counting.setURL("https://x.example")
	if rangedQuestionVisible(counting) {
		t.Error("the ranged question must be HIDDEN while the count is in flight (finding #1)")
	}

	// failed → visible (the only state where the ballpark question is honest).
	failed := &capState{}
	failed.setURL("https://nositemap.example")
	failed.record("https://nositemap.example", 0, false)
	if !rangedQuestionVisible(failed) {
		t.Error("the ranged question must be VISIBLE once the count fails/times out")
	}

	// ok → hidden (a real number landed; the cap-choices step drives instead).
	ok := &capState{}
	ok.setURL("https://big.example")
	ok.record("https://big.example", 9000, true)
	if rangedQuestionVisible(ok) {
		t.Error("the ranged question must be HIDDEN once a usable count lands")
	}
}

// TestCancelledCountRoutesToRanged joins the timeout/cancel → ranged-question path
// end-to-end (finding #15): a count whose context is cancelled before it can read a
// sitemap (the production timeout posture) records a failed result, which must land
// the state in phaseFailed AND surface the ranged ballpark question. This pins the
// contract that a slow/aborted sitemap read degrades gracefully to the operator-asked
// fallback rather than hanging on the reading gate forever.
func TestCancelledCountRoutesToRanged(t *testing.T) {
	cs := &capState{}
	cs.setURL("https://slow.example")

	started := make(chan struct{})
	// A ctx-respecting seam that only resolves on cancellation — mirrors a sitemap read
	// that times out / is aborted, returning the (0, false) "no usable count" signal.
	count := func(ctx context.Context, _ string) (int, bool) {
		close(started)
		<-ctx.Done()
		return 0, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startCount(ctx, cs, "https://slow.example", count)
	<-started
	cancel()
	<-done // must return promptly; a block hangs the test under -race/-timeout

	if got := countPhase(cs); got != phaseFailed {
		t.Errorf("a cancelled/timed-out count must land in phaseFailed; got %v", got)
	}
	if !rangedQuestionVisible(cs) {
		t.Error("a cancelled/timed-out count must surface the ranged ballpark question")
	}
}

// TestCapStepFiresGatedWhileCounting asserts that the cap-choices step never fires
// while the count is in flight, regardless of the (default) ballpark bucket — the
// reading note is the only thing on screen until the count resolves (Item C: counting
// hides BOTH the estimate and ranged inputs).
func TestCapStepFiresGatedWhileCounting(t *testing.T) {
	cfg := &config.Config{}
	site := config.SiteConfig{}

	counting := &capState{}
	counting.setURL("https://x.example")
	// Even a large ballpark bucket must NOT fire the cap choices while counting:
	// the reading gate suppresses everything until the count resolves.
	if capStepFires(counting, "20,000 – 50,000", cfg, site) {
		t.Error("the cap choices must NOT fire while the count is in flight (Item C reading gate)")
	}
}

// TestReadingNote pins the unmistakable "Reading sitemap.xml…" hold copy shown while
// the count is in flight, and that it carries the ⏳ signal (Item C).
func TestReadingNote(t *testing.T) {
	note := ReadingSitemapNote
	if !strings.Contains(note, "sitemap.xml") {
		t.Errorf("reading note %q must mention sitemap.xml", note)
	}
	if !strings.Contains(note, "⏳") {
		t.Errorf("reading note %q must carry the ⏳ reading signal", note)
	}
}
