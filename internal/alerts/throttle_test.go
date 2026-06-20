package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestThrottleAllowsUnderCap(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottle(2, func() time.Time { return now })
	if !th.allow("slack-digest", model.SeverityWarning) {
		t.Fatal("first warning under cap should be allowed")
	}
	if !th.allow("slack-digest", model.SeverityWarning) {
		t.Fatal("second warning under cap should be allowed")
	}
	if th.allow("slack-digest", model.SeverityWarning) {
		t.Error("third warning over cap should be blocked")
	}
}

func TestThrottleZeroCapMeansUnlimited(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottle(0, func() time.Time { return now })
	// A cap of 0 must mean "no limit", not "block every non-critical alert".
	for i := 0; i < 5; i++ {
		if !th.allow("slack-digest", model.SeverityWarning) {
			t.Fatalf("warning %d blocked: cap<=0 must allow all non-criticals", i)
		}
	}
}

func TestThrottleCriticalsBypassCap(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottle(1, func() time.Time { return now })
	_ = th.allow("slack-critical", model.SeverityWarning) // consume the only slot
	if !th.allow("slack-critical", model.SeverityCritical) {
		t.Error("criticals must bypass the per-recipient hourly cap")
	}
	if !th.allow("slack-critical", model.SeverityCritical) {
		t.Error("a second critical must also bypass the cap")
	}
}

func TestThrottleWindowResets(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	th := newThrottle(1, func() time.Time { return cur })
	if !th.allow("r", model.SeverityWarning) {
		t.Fatal("first allowed")
	}
	if th.allow("r", model.SeverityWarning) {
		t.Fatal("second blocked within hour")
	}
	cur = base.Add(61 * time.Minute) // advance past the hour window
	if !th.allow("r", model.SeverityWarning) {
		t.Error("after window reset, should allow again")
	}
}

// TestThrottleSlidingWindowBoundsRollingHour reproduces F45: a fixed tumbling
// window anchored to first-message time resets a full hour after the FIRST send,
// so cap messages near the end of that window plus cap messages just after the
// boundary deliver up to 2×cap within minutes. A correct sliding window bounds the
// rate over ANY rolling hour.
//
// Scenario (cap=2): A0 at minute 0 opens the window (count 1); A59 at minute 59
// fills it (count 2). At minute 61 a tumbling window has elapsed >1h since its
// minute-0 start, so it RESETS and admits B61 and B62 — delivering A59, B61, B62
// (3 messages) within a 3-minute rolling span, exceeding cap=2. The sliding window
// must block B62: at minute 62 the trailing hour (minute 2, 62] still holds A59
// and B61 (== cap), so B62 is over the rolling-hour cap.
func TestThrottleSlidingWindowBoundsRollingHour(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	th := newThrottle(2, func() time.Time { return cur })

	// A0: opens the window at minute 0.
	if !th.allow("r", model.SeverityWarning) {
		t.Fatal("minute 0: first under cap should allow")
	}
	// A59: fills the cap near the end of the first hour.
	cur = base.Add(59 * time.Minute)
	if !th.allow("r", model.SeverityWarning) {
		t.Fatal("minute 59: second under cap should allow")
	}
	// B61: just past the tumbling boundary. A0 (minute 0) has aged out of the
	// rolling hour (2,61]; only A59 remains, so this is the 2nd in the rolling hour
	// and is correctly admitted by BOTH window styles.
	cur = base.Add(61 * time.Minute)
	if !th.allow("r", model.SeverityWarning) {
		t.Fatal("minute 61: only A59 in the trailing hour; under cap, should allow")
	}
	// B62: the rolling hour (2,62] now holds A59 and B61 == cap. A tumbling window
	// (reset at minute 60) would admit this as a fresh count; the sliding window
	// MUST block it — admitting it would put 3 sends (A59,B61,B62) in a 3-minute span.
	cur = base.Add(62 * time.Minute)
	if th.allow("r", model.SeverityWarning) {
		t.Error("minute 62: rolling hour already holds cap (A59,B61); must block (sliding window bounds 2×cap overshoot)")
	}
}

func TestPipelineCriticalBypassesThrottle(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
	)
	// Two distinct critical events (different change_type) under a cap of 1.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "s", URL: "https://s/a", ChangeType: "indexability", Severity: model.SeverityCritical, After: "x"})
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "s", URL: "https://s/b", ChangeType: "robots_txt", Severity: model.SeverityCritical, After: "y"})
	if len(disp.got) != 2 {
		t.Errorf("both criticals must dispatch despite cap=1, got %d", len(disp.got))
	}
}
