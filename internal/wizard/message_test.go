package wizard

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestVerifyMessage_AllReasonsCovered asserts the mapping is TOTAL over every
// verify.Reason: each yields a non-empty headline, and every failure reason
// offers at least one retry action (only the success case may offer none).
func TestVerifyMessage_AllReasonsCovered(t *testing.T) {
	for _, r := range []verify.Reason{
		verify.ReasonVerified, verify.ReasonNotFound, verify.ReasonMismatch,
		verify.ReasonRedirected, verify.ReasonUnreachable,
	} {
		m := VerifyMessage(r, "yoursite.com", "rab_ABC")
		if m.Headline == "" {
			t.Fatalf("reason %q produced an empty headline", r)
		}
		if r != verify.ReasonVerified && len(m.Actions) == 0 {
			t.Fatalf("failure reason %q offered no retry actions", r)
		}
	}
}

// TestVerifyMessage_NotFoundMismatchActionsAreRenderedAndBound asserts that the
// NotFound and Mismatch action sets contain ONLY labels the proof screen actually
// renders in the footer and binds in handleResultKey — guarding against the
// dead/invisible entries reported in #42 ("See the steps", redundant "Finish
// later"). The only honored labels are the re-check labels ("Check again"/"Try
// again", surfaced by resultHasCheckAgain + bound to Enter/r) and "Try a different
// way" (resultHasTryDifferent, bound to t). "Finish later" is NEVER an explicit
// action: resultFooter always appends finish-later itself, so listing it is dead
// drift; "See the steps" is neither rendered nor bound. (Scoped to the two reasons
// #42 covers; the broader audit of every reason is intentionally out of scope.)
func TestVerifyMessage_NotFoundMismatchActionsAreRenderedAndBound(t *testing.T) {
	rendered := map[string]bool{
		"Check again":         true, // resultHasCheckAgain → Enter/r
		"Try again":           true, // resultHasCheckAgain → Enter/r
		"Try a different way": true, // resultHasTryDifferent → t
	}
	for _, r := range []verify.Reason{verify.ReasonNotFound, verify.ReasonMismatch} {
		m := VerifyMessage(r, "yoursite.com", "rab_ABC")
		for _, a := range m.Actions {
			if !rendered[a] {
				t.Fatalf("reason %q lists action %q that the proof screen never renders or binds", r, a)
			}
		}
	}
}

// TestVerifyMessage_NotFoundActions pins the exact NotFound action set to only the
// rendered+bound labels after dropping the dead "See the steps" and the redundant
// "Finish later" (the footer appends finish-later itself).
func TestVerifyMessage_NotFoundActions(t *testing.T) {
	m := VerifyMessage(verify.ReasonNotFound, "yoursite.com", "rab_ABC")
	want := []string{"Check again", "Try a different way"}
	if len(m.Actions) != len(want) {
		t.Fatalf("NotFound actions = %v; want %v", m.Actions, want)
	}
	for i, a := range want {
		if m.Actions[i] != a {
			t.Fatalf("NotFound actions = %v; want %v", m.Actions, want)
		}
	}
}

// TestVerifyMessage_MismatchActions pins the exact Mismatch action set to only the
// rendered+bound "Check again" after dropping the dead "See the steps".
func TestVerifyMessage_MismatchActions(t *testing.T) {
	m := VerifyMessage(verify.ReasonMismatch, "yoursite.com", "rab_ABC")
	want := []string{"Check again"}
	if len(m.Actions) != len(want) {
		t.Fatalf("Mismatch actions = %v; want %v", m.Actions, want)
	}
	for i, a := range want {
		if m.Actions[i] != a {
			t.Fatalf("Mismatch actions = %v; want %v", m.Actions, want)
		}
	}
}

// TestVerifyMessage_MismatchShowsToken asserts the mismatch copy surfaces the
// exact expected token so the user can fix what they pasted. The token lives in
// the Detail line for that reason.
func TestVerifyMessage_MismatchShowsToken(t *testing.T) {
	m := VerifyMessage(verify.ReasonMismatch, "yoursite.com", "rab_ABC")
	if !strings.Contains(m.Detail, "rab_ABC") {
		t.Fatalf("mismatch message should show the expected token; detail = %q", m.Detail)
	}
}

// TestVerifyMessage_VerifiedNamesHost asserts the success headline weaves in the
// host so the celebration is concrete.
func TestVerifyMessage_VerifiedNamesHost(t *testing.T) {
	m := VerifyMessage(verify.ReasonVerified, "yoursite.com", "rab_ABC")
	if !strings.Contains(m.Headline, "yoursite.com") {
		t.Fatalf("verified headline should name the host; got %q", m.Headline)
	}
}

// TestVerifyMessage_NoEnumLeak asserts no raw verify.Reason enum identifier leaks
// into any user-facing field across the whole mapping.
func TestVerifyMessage_NoEnumLeak(t *testing.T) {
	for _, r := range []verify.Reason{
		verify.ReasonVerified, verify.ReasonNotFound, verify.ReasonMismatch,
		verify.ReasonRedirected, verify.ReasonUnreachable,
	} {
		m := VerifyMessage(r, "yoursite.com", "rab_ABC")
		blob := m.Headline + " " + m.Detail + " " + strings.Join(m.Actions, " ")
		for _, leak := range []string{"ReasonVerified", "ReasonNotFound", "ReasonMismatch", "ReasonRedirected", "ReasonUnreachable"} {
			if strings.Contains(blob, leak) {
				t.Fatalf("reason %q leaked the enum identifier %q into user-facing copy: %q", r, leak, blob)
			}
		}
	}
}
