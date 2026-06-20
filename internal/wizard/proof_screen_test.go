package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestPlacementInstructions(t *testing.T) {
	const host = "example.com"
	const token = "rab_T"

	wk := PlacementInstructions(verify.MethodWellKnown, host, token)
	if !strings.Contains(wk, "/.well-known/rabbot-verify.txt") {
		t.Errorf("well_known instructions missing path:\n%s", wk)
	}
	if !strings.Contains(wk, token) {
		t.Errorf("well_known instructions missing token:\n%s", wk)
	}

	dns := PlacementInstructions(verify.MethodDNS, host, token)
	if !strings.Contains(dns, "rabbot-verify="+token) {
		t.Errorf("dns instructions missing TXT record:\n%s", dns)
	}

	meta := PlacementInstructions(verify.MethodMeta, host, token)
	if !strings.Contains(meta, `name="rabbot-verify"`) {
		t.Errorf("meta instructions missing name attr:\n%s", meta)
	}
	if !strings.Contains(meta, `content="`+token+`"`) {
		t.Errorf("meta instructions missing content attr:\n%s", meta)
	}

	// DNS placement must STRIP any :port: VerifyDNS resolves the bare hostname (DNS
	// has no concept of ports), and this mirrors the CLI's printPlacement — the two
	// must never drift. For a site added as https://example.com:8443 the wizard must
	// name "example.com", not "example.com:8443" (an invalid DNS name the lookup
	// would never match). The well_known/meta branches keep host:port (HTTP fetch).
	const hostPort = "example.com:8443"
	dnsPort := PlacementInstructions(verify.MethodDNS, hostPort, token)
	if !strings.Contains(dnsPort, "example.com:\n") {
		t.Errorf("dns instructions should name the bare host:\n%s", dnsPort)
	}
	if strings.Contains(dnsPort, "8443") {
		t.Errorf("dns instructions must not include the port:\n%s", dnsPort)
	}

	// Sanity: the well_known branch DOES keep the full host:port (its HTTP fetch needs it).
	wkPort := PlacementInstructions(verify.MethodWellKnown, hostPort, token)
	if !strings.Contains(wkPort, "example.com:8443") {
		t.Errorf("well_known instructions should keep the full host:port:\n%s", wkPort)
	}
}

// verifiedVF is a test VerifyFunc that always returns a verified outcome.
func verifiedVF(context.Context, verify.Method) (verify.Outcome, error) {
	return verify.Outcome{Record: verify.ProofRecord{State: verify.StateVerified}, Reason: verify.ReasonVerified}, nil
}

// asModel asserts the tea.Model is a proofModel and returns it (test helper).
func asModel(t *testing.T, m tea.Model) proofModel {
	t.Helper()
	pm, ok := m.(proofModel)
	if !ok {
		t.Fatalf("Update returned %T, want proofModel", m)
	}
	return pm
}

// TestProofModel_DoesNotVerifyOnEntry is the centerpiece contract: the
// place-THEN-verify rework must NOT fire the check on entry, AND the new
// method-select entry state must not fire it either. Init may start the spinner
// ticking, but the injected VerifyFunc runs ONLY after an explicit "check now"
// key — never from Init.
func TestProofModel_DoesNotVerifyOnEntry(t *testing.T) {
	checked := false
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_X",
		func(context.Context, verify.Method) (verify.Outcome, error) {
			checked = true
			return verify.Outcome{}, nil
		})
	_ = m.Init() // Init must NOT trigger the check anymore
	if checked {
		t.Fatal("verification must not fire on entry — only on an explicit 'check now'")
	}
}

// TestProofModelStartsInMethodSelect pins that a freshly built model opens on the
// method-select screen (§V V2) — the user picks a method BEFORE placement — with
// the three methods listed and nothing checked yet.
func TestProofModelStartsInMethodSelect(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	if m.state != stateMethod {
		t.Fatalf("new model should start in stateMethod, got %v", m.state)
	}
	if m.done {
		t.Error("new model must not be done")
	}
	// The method-select view lists all three methods in plain language.
	v := m.View()
	for _, want := range []string{"Add a tag to your homepage", "Upload a small file", "Add a record at your domain provider"} {
		if !strings.Contains(v, want) {
			t.Errorf("method-select view missing %q:\n%s", want, v)
		}
	}
}

// TestProofModelRecommendedHighlighted pins that a KNOWN platform pre-highlights
// the recommended method and shows its non-empty hint on the method screen (§V2).
func TestProofModelRecommendedHighlighted(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformWordPress, "example.com", "rab_T", verifiedVF)
	if m.state != stateMethod {
		t.Fatalf("new model should start in stateMethod, got %v", m.state)
	}
	// The cursor pre-selects the recommended method (meta) — index 0 in the list.
	if m.methods[m.cursor] != verify.MethodMeta {
		t.Errorf("recommended method should be pre-highlighted; cursor on %v", m.methods[m.cursor])
	}
	v := m.View()
	// The plain hint naming the platform is shown when non-empty.
	if !strings.Contains(v, "WordPress") {
		t.Errorf("method-select view should show the recommendation hint:\n%s", v)
	}
}

// TestProofModelSelectMethodEntersPlace pins V2→V3: selecting a method (Enter on
// the cursor) puts the model in statePlace for THAT method, with placement
// instructions re-derived for it — and does NOT fire the check.
func TestProofModelSelectMethodEntersPlace(t *testing.T) {
	checked := false
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T",
		func(context.Context, verify.Method) (verify.Outcome, error) {
			checked = true
			return verify.Outcome{}, nil
		})
	// Move the cursor to the well_known method, then select it.
	m = moveCursorTo(t, m, verify.MethodWellKnown)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if pm.state != statePlace {
		t.Fatalf("selecting a method should enter statePlace, got %v", pm.state)
	}
	if pm.method != verify.MethodWellKnown {
		t.Fatalf("statePlace should carry the selected method, got %v", pm.method)
	}
	if checked {
		t.Fatal("selecting a method must NOT fire the verify check")
	}
	if cmd != nil {
		// Selecting a method only transitions state; it must not dispatch verify.
		t.Errorf("selecting a method should not dispatch a verify cmd")
	}
	// The placement view reflects the chosen (well_known) method.
	if !strings.Contains(pm.View(), "/.well-known/rabbot-verify.txt") {
		t.Errorf("statePlace view should show the well_known instructions:\n%s", pm.View())
	}
}

// TestProofModelPlaceShowsProviderHint pins V3: for a KNOWN platform with a
// curated provider link, statePlace renders the provider deep-link label.
func TestProofModelPlaceShowsProviderHint(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformWordPress, "example.com", "rab_T", verifiedVF)
	// Recommended method is meta and is pre-highlighted; select it.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if pm.state != statePlace {
		t.Fatalf("selecting the recommended method should enter statePlace, got %v", pm.state)
	}
	label, url := ProviderHint(precheck.PlatformWordPress, verify.MethodMeta)
	v := pm.View()
	if !strings.Contains(v, label) {
		t.Errorf("statePlace view should show the provider hint label %q:\n%s", label, v)
	}
	if !strings.Contains(v, url) {
		t.Errorf("statePlace view should show the provider hint url %q:\n%s", url, v)
	}
}

// TestProofModelPlaceGenericHintFallback pins V3 fallback: an UNKNOWN platform (no
// curated provider link) shows the generic how-to, not an empty line.
func TestProofModelPlaceGenericHintFallback(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if !strings.Contains(pm.View(), "Not sure where to put it?") {
		t.Errorf("statePlace view should show the generic provider fallback:\n%s", pm.View())
	}
}

// TestProofModelCheckNowDispatches pins that the "check now" key (enter) moves the
// model into the checking state and returns a non-nil cmd (the verify command).
func TestProofModelCheckNowDispatches(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	// Enter the placement state for the recommended method first.
	entered, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = asModel(t, entered)
	if m.state != statePlace {
		t.Fatalf("setup: expected statePlace, got %v", m.state)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if pm.state != stateChecking {
		t.Fatalf("enter in statePlace should move to stateChecking, got %v", pm.state)
	}
	if cmd == nil {
		t.Fatal("check-now must dispatch a non-nil verify cmd")
	}
	if pm.done {
		t.Error("dispatching the check must not mark done")
	}
}

// TestProofModelResultThenFinish pins the result→finish lifecycle: a proofDoneMsg
// records the outcome and moves to the result state (NOT quitting — the user may
// retry), and the result view renders the friendly VerifyMessage. Pressing the
// finish key then quits with done=true so the runner persists the recorded
// outcome.
func TestProofModelResultThenFinish(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	m.state = stateChecking // simulate a check in flight

	out := verify.Outcome{Record: verify.ProofRecord{State: verify.StateVerified}, Reason: verify.ReasonVerified}
	next, cmd := m.Update(proofDoneMsg{out: out})
	pm := next.(proofModel)
	if pm.state != stateResult {
		t.Fatalf("proofDoneMsg should move to stateResult, got %v", pm.state)
	}
	if pm.done {
		t.Error("recording a result must NOT mark done — the user may still retry")
	}
	if cmd != nil {
		t.Error("recording a result should not quit; cmd must be nil")
	}
	if pm.out.Record.State != verify.StateVerified {
		t.Errorf("out.Record.State = %q, want verified", pm.out.Record.State)
	}
	// The result view shows the friendly success copy (VerifyMessage headline).
	if !strings.Contains(pm.View(), "full speed") {
		t.Errorf("verified result view should show the success copy:\n%s", pm.View())
	}

	// Finish the screen: 'f' quits and marks done so the runner persists pm.out.
	fin, fcmd := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	fm := fin.(proofModel)
	if !fm.done {
		t.Error("finishing from the result must mark done")
	}
	quitsWith(t, fcmd)
}

// TestProofModelCheckAgain pins the retry loop: from a result whose actions
// include "check again" (NotFound), "check again" (enter / 'r') re-enters the
// checking state and re-dispatches the verify cmd so a newly-placed token can be
// re-checked without leaving the screen.
func TestProofModelCheckAgain(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T",
		func(context.Context, verify.Method) (verify.Outcome, error) {
			return verify.Outcome{Record: verify.ProofRecord{State: verify.StateThrottled}, Reason: verify.ReasonNotFound}, nil
		})
	m.state = stateResult
	m.out = verify.Outcome{Record: verify.ProofRecord{State: verify.StateThrottled}, Reason: verify.ReasonNotFound}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if pm.state != stateChecking {
		t.Fatalf("enter in stateResult should re-check (stateChecking), got %v", pm.state)
	}
	if cmd == nil {
		t.Fatal("check-again must dispatch a non-nil verify cmd")
	}
	if pm.done {
		t.Error("re-checking must not mark done")
	}
}

// TestProofModelTryDifferentWay pins V4→V2: the "Try a different way" action
// ('t') from a result routes BACK to the method-select state and does NOT fire a
// check.
func TestProofModelTryDifferentWay(t *testing.T) {
	checked := false
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T",
		func(context.Context, verify.Method) (verify.Outcome, error) {
			checked = true
			return verify.Outcome{}, nil
		})
	m.state = stateResult
	m.out = verify.Outcome{Record: verify.ProofRecord{State: verify.StateThrottled}, Reason: verify.ReasonRedirected}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	pm := next.(proofModel)
	if pm.state != stateMethod {
		t.Fatalf("'try a different way' should route back to stateMethod, got %v", pm.state)
	}
	if checked {
		t.Fatal("'try a different way' must NOT fire a check")
	}
	if cmd != nil {
		t.Error("'try a different way' should not dispatch a verify cmd")
	}
	if pm.done {
		t.Error("'try a different way' must not mark done")
	}
}

// TestProofModelRedirectedNoCheckAgain pins V4 footer/key correctness: a
// ReasonRedirected result has NO "check again" action, so Enter must NOT fire a
// futile re-check, and the footer must not advertise one.
func TestProofModelRedirectedNoCheckAgain(t *testing.T) {
	checked := false
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T",
		func(context.Context, verify.Method) (verify.Outcome, error) {
			checked = true
			return verify.Outcome{}, nil
		})
	m.state = stateResult
	m.out = verify.Outcome{Record: verify.ProofRecord{State: verify.StateThrottled}, Reason: verify.ReasonRedirected}

	// Enter must not re-check for a redirect (no check-again action).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := next.(proofModel)
	if pm.state == stateChecking {
		t.Fatal("Enter on a redirect result must NOT fire a futile re-check")
	}
	if checked {
		t.Fatal("a redirect result must not fire a check")
	}
	v := pm.View()
	if strings.Contains(strings.ToLower(v), "check again") {
		t.Errorf("redirect footer must not advertise 'check again':\n%s", v)
	}
	if !strings.Contains(strings.ToLower(v), "different way") {
		t.Errorf("redirect footer should advertise 'try a different way':\n%s", v)
	}
}

// TestProofModelResultMissShowsReason pins that a miss result surfaces the
// specific friendly reason copy (not the raw enum) so the operator knows what to fix.
func TestProofModelResultMissShowsReason(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_ABC", verifiedVF)
	m.state = stateChecking
	out := verify.Outcome{Record: verify.ProofRecord{State: verify.StateThrottled}, Reason: verify.ReasonNotFound}
	next, _ := m.Update(proofDoneMsg{out: out})
	pm := next.(proofModel)
	v := pm.View()
	if strings.Contains(v, "not_found") || strings.Contains(v, "ReasonNotFound") {
		t.Errorf("result view must not leak the raw reason enum:\n%s", v)
	}
	// NotFound copy weaves in the token so the operator can fix the mismatch.
	if !strings.Contains(v, "rab_ABC") {
		t.Errorf("not-found result should show the expected token:\n%s", v)
	}
}

func TestProofModelSpinnerTick(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	m.state = stateChecking

	next, cmd := m.Update(spinner.TickMsg{})
	pm := next.(proofModel)
	if pm.done {
		t.Error("spinner tick should not mark done")
	}
	if cmd == nil {
		t.Error("spinner tick should return a non-nil cmd to keep ticking")
	}
}

func TestProofModelInitNonNil(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)
	if m.Init() == nil {
		t.Error("Init should return a non-nil cmd (the spinner tick)")
	}
}

// TestProofModelUpdateEscCancels pins the abort UX on the live proof-of-control
// screen: pressing Esc must quit WITHOUT marking done, so the orchestrator's
// screenCancelled(!done) guard treats it as a cancel (matching huh forms).
func TestProofModelUpdateEscCancels(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm, ok := next.(proofModel)
	if !ok {
		t.Fatalf("Update returned %T, want proofModel", next)
	}
	if pm.done {
		t.Error("Esc must NOT mark done; the orchestrator's !done guard must see a cancel")
	}
	quitsWith(t, cmd)
}

// TestProofModelUpdateCtrlCCancels pins that ctrl+c keeps quitting without
// marking done (regression guard alongside the Esc branch).
func TestProofModelUpdateCtrlCCancels(t *testing.T) {
	m := NewProofModel(context.Background(), precheck.PlatformUnknown, "example.com", "rab_T", verifiedVF)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	pm := next.(proofModel)
	if pm.done {
		t.Error("ctrl+c must NOT mark done")
	}
	quitsWith(t, cmd)
}

// moveCursorTo drives the method-select cursor to the target method via "down"
// keys (the list is short and ordered), returning the resulting model. It fails
// the test if the target is not present.
func moveCursorTo(t *testing.T, m proofModel, target verify.Method) proofModel {
	t.Helper()
	for i := 0; i < len(m.methods); i++ {
		if m.methods[m.cursor] == target {
			return m
		}
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = next.(proofModel)
	}
	if m.methods[m.cursor] != target {
		t.Fatalf("could not move cursor to %v; methods=%v", target, m.methods)
	}
	return m
}
