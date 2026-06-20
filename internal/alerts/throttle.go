package alerts

import (
	"sync"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// throttle enforces a per-recipient hourly message cap using a SLIDING window:
// at most cap messages may pass in any rolling 60-minute span. Criticals always
// bypass. A fixed tumbling window anchored to first-message time would let cap
// messages near the end of one hour plus cap messages just after the boundary
// deliver 2×cap within minutes; the sliding window bounds the rate over every
// rolling hour instead.
type throttle struct {
	mu     sync.Mutex
	cap    int
	now    func() time.Time
	counts map[string]*window // keyed by recipient name
}

// window holds the send timestamps within the trailing hour for one recipient.
// Entries older than an hour are pruned on each allow() so the slice stays
// bounded by the cap.
type window struct {
	sends []time.Time
}

func newThrottle(hourlyCap int, now func() time.Time) *throttle {
	if now == nil {
		now = time.Now
	}
	return &throttle{cap: hourlyCap, now: now, counts: map[string]*window{}}
}

// allow reports whether a message to recipient may be sent now. Criticals always
// return true and do not consume the cap. The cap is enforced over a sliding
// 60-minute window: a send is admitted only if fewer than cap sends have occurred
// in the trailing hour.
func (t *throttle) allow(recipient string, sev model.Severity) bool {
	if sev == model.SeverityCritical {
		return true
	}
	// A non-positive cap disables throttling (unlimited real-time delivery) rather
	// than blocking every non-critical alert — `per_recipient_hourly_cap: 0` reads
	// as "no limit", not "send nothing".
	if t.cap <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	cutoff := now.Add(-time.Hour)
	w := t.counts[recipient]
	if w == nil {
		w = &window{}
		t.counts[recipient] = w
	}
	// Drop sends that have aged out of the trailing hour.
	kept := w.sends[:0]
	for _, ts := range w.sends {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	w.sends = kept
	if len(w.sends) >= t.cap {
		return false
	}
	w.sends = append(w.sends, now)
	return true
}
