package wizard

import (
	"testing"
	"time"
)

func TestCadenceChoice_MapsToIntervals(t *testing.T) {
	min, max := CadenceIntervals(CadenceFewTimesADay)
	if min == "" || max == "" {
		t.Fatal("default cadence must map to concrete intervals")
	}
	if _, _, ok := parseCadence("garbage"); ok {
		t.Fatal("unknown cadence label should not parse")
	}
}

// TestCadenceIntervals_AllChoicesValidAndOrdered pins the contract the config
// writers depend on: every choice maps to a pair of POSITIVE Go durations that
// time.ParseDuration accepts AND with min <= max (setup/throttle reject an
// inverted pair). A regression here would write a config the daemon rejects.
func TestCadenceIntervals_AllChoicesValidAndOrdered(t *testing.T) {
	for _, c := range []CadenceChoice{CadenceFewTimesADay, CadenceAboutHourly, CadenceFewTimesAnHour} {
		min, max := CadenceIntervals(c)
		minD, err := time.ParseDuration(min)
		if err != nil || minD <= 0 {
			t.Fatalf("choice %d min %q is not a positive Go duration (err=%v)", c, min, err)
		}
		maxD, err := time.ParseDuration(max)
		if err != nil || maxD <= 0 {
			t.Fatalf("choice %d max %q is not a positive Go duration (err=%v)", c, max, err)
		}
		if maxD < minD {
			t.Fatalf("choice %d has max %q < min %q (inverted pair)", c, max, min)
		}
	}
}

// TestParseCadence_RoundTrips asserts each friendly menu label parses back to a
// distinct choice (so the fine-tune Select can map the chosen label to intervals).
func TestParseCadence_RoundTrips(t *testing.T) {
	labels := CadenceLabels()
	if len(labels) == 0 {
		t.Fatal("CadenceLabels must offer at least one friendly option")
	}
	seen := make(map[CadenceChoice]bool, len(labels))
	for _, label := range labels {
		c, got, ok := parseCadence(label)
		if !ok {
			t.Fatalf("offered label %q did not parse", label)
		}
		if got != label {
			t.Fatalf("parseCadence(%q) echoed %q", label, got)
		}
		if seen[c] {
			t.Fatalf("label %q maps to an already-seen choice %d", label, c)
		}
		seen[c] = true
	}
}
