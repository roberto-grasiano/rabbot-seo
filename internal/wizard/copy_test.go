package wizard

import (
	"strings"
	"testing"
)

func TestContactExample_NoJargon(t *testing.T) {
	got := ContactExample("1.2.3", "ops@me.example", "me.example")
	// Shows how the operator appears, WITHOUT the term "User-Agent".
	if !strings.Contains(got, "ops@me.example") {
		t.Fatalf("example %q missing the contact email", got)
	}
	// The preview frames the real resolved User-Agent (the mailto identity string).
	if !strings.Contains(got, "mailto:ops@me.example") {
		t.Fatalf("example %q must show the resulting mailto User-Agent", got)
	}
	if strings.Contains(got, "User-Agent") || strings.Contains(got, "user_agent") {
		t.Fatalf("example %q leaks the User-Agent jargon", got)
	}
}

// TestContactExample_PerSiteUA pins #17: the preview must render the realistic
// PRE-VERIFICATION per-site User-Agent (UserAgentFor(host, version, false)) for the
// site the operator just named — NOT the host-agnostic ResolvedUserAgent — so "what
// owners see" matches what the daemon actually sends before verification.
func TestContactExample_PerSiteUA(t *testing.T) {
	// Same-domain email + host → the per-site form carries the "<domain> contact,
	// unverified" trust suffix that the host-agnostic ResolvedUserAgent never emits.
	got := ContactExample("1.2.3", "ops@me.example", "me.example")
	if !strings.Contains(got, "me.example contact, unverified") {
		t.Fatalf("example %q must show the realistic pre-verification per-site trust suffix", got)
	}
	if strings.Contains(got, "verified for") {
		t.Fatalf("example %q must be the UNVERIFIED per-site form (verified=false)", got)
	}

	// Cross-domain → the most cautious per-site branch, again absent from the base form.
	cross := ContactExample("1.2.3", "ops@me.example", "other.example")
	if !strings.Contains(cross, "unverified — confirm or block") {
		t.Fatalf("cross-domain example %q must show the cautious per-site suffix", cross)
	}
}

// TestContactExample_NoHostSoftensToBase pins the #17 fallback: when the named-site
// host is not available at preview time, soften the copy to the base identity rather
// than fabricate a per-site trust suffix.
func TestContactExample_NoHostSoftensToBase(t *testing.T) {
	got := ContactExample("1.2.3", "ops@me.example", "")
	if !strings.Contains(got, "mailto:ops@me.example") {
		t.Fatalf("base-identity fallback %q must still show the mailto identity", got)
	}
	// No host → no per-site trust suffix may be invented.
	if strings.Contains(got, "contact, unverified") || strings.Contains(got, "confirm or block") {
		t.Fatalf("base-identity fallback %q must not fabricate a per-site trust suffix", got)
	}
	if strings.Contains(got, "User-Agent") {
		t.Fatalf("base-identity fallback %q leaks the User-Agent jargon", got)
	}
}

func TestStagingNudge_MentionsStagingAndIsSkippableCopy(t *testing.T) {
	if !strings.Contains(strings.ToLower(StagingNudge), "staging") {
		t.Fatalf("nudge %q does not mention staging", StagingNudge)
	}
}

func TestGoLiveLine_SaysLiveAndPolite(t *testing.T) {
	got := GoLiveLine("yoursite.com")
	if !strings.Contains(got, "yoursite.com") {
		t.Fatalf("go-live line %q missing the site", got)
	}
}

// TestSleepNudge_HonestAndPointsToGuide pins criterion 9: the nudge is non-empty,
// points at the README "Where to run Rabbot" section, and stays honest — it frames
// the limitation as a pause/gap, never claiming monitoring is broken (no "error").
func TestSleepNudge_HonestAndPointsToGuide(t *testing.T) {
	if SleepNudge == "" {
		t.Fatal("SleepNudge must be non-empty")
	}
	if !strings.Contains(SleepNudge, "Where to run Rabbot") {
		t.Fatalf("nudge %q must point at the \"Where to run Rabbot\" section", SleepNudge)
	}
	low := strings.ToLower(SleepNudge)
	if !strings.Contains(low, "pause") && !strings.Contains(low, "gap") {
		t.Fatalf("nudge %q must use pause/gap honesty wording", SleepNudge)
	}
	if strings.Contains(low, "error") {
		t.Fatalf("nudge %q must not claim monitoring is broken (no \"error\")", SleepNudge)
	}
}
