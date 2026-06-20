package wizard

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// VerifyFunc is the injected seam for the LIVE proof-of-control check. It takes
// the method the operator PICKED on the screen (§V V2) so the live check runs
// against the chosen proof surface, not a fixed one. Production closes over
// verify.Verify (verify.Request{Host,Method} + verify.Options{Now,Key}), which
// DERIVES the instance-bound token internally and returns an Outcome (State +
// Reason); tests inject a stub returning a fixture Outcome, so the model's
// lifecycle is unit-testable without any network. NOTE: this package records
// proof intent in config.yaml (config.SetSiteVerificationYAML), never the DB —
// at `init` time the daemon doesn't exist and the site isn't in the store yet —
// so there is deliberately NO store import here.
type VerifyFunc func(context.Context, verify.Method) (verify.Outcome, error)

// proofDoneMsg carries the verify outcome back into the bubbletea loop.
type proofDoneMsg struct {
	out verify.Outcome
	err error
}

// proofState is the pick-place-THEN-verify lifecycle of the proof screen (spec §V
// V2→V4). The screen NEVER fires the check on entry: it opens on the method
// picker (V2), then shows the token + placement instructions and WAITS in
// statePlace (V3) until the operator confirms they've placed it ("check now"),
// then runs the injected verify in stateChecking, then renders a specific
// friendly result in stateResult (V4) where the operator can retry, switch
// methods, or finish.
type proofState int

const (
	// stateMethod is the entry screen (§V V2): the operator picks one of the three
	// proof methods (the recommended one pre-highlighted). No verify has run.
	stateMethod proofState = iota
	// statePlace shows the token + placement instructions + provider hint and
	// awaits "check now". No verify has run yet — the place-then-verify contract.
	statePlace
	// stateChecking runs the injected verify with a spinner.
	stateChecking
	// stateResult renders the friendly VerifyMessage and offers retry/switch/finish.
	stateResult
)

// methodLabel is the plain, non-jargon label for a proof method shown on the
// method-select screen (§V V2). It mirrors the spec wording so the picker reads
// like a choice of "how do I prove this?", not a list of enum values.
func methodLabel(m verify.Method) string {
	switch m {
	case verify.MethodMeta:
		return "Add a tag to your homepage"
	case verify.MethodWellKnown:
		return "Upload a small file"
	case verify.MethodDNS:
		return "Add a record at your domain provider"
	default:
		return string(m)
	}
}

// proofMethods is the fixed, ordered list of methods offered on the picker. The
// recommended method (from RecommendMethod) is moved to the front so the cursor
// pre-highlights it; the rest follow in a stable order.
func proofMethods(recommended verify.Method) []verify.Method {
	all := []verify.Method{verify.MethodMeta, verify.MethodWellKnown, verify.MethodDNS}
	out := make([]verify.Method, 0, len(all))
	out = append(out, recommended)
	for _, m := range all {
		if m != recommended {
			out = append(out, m)
		}
	}
	return out
}

// proofModel is the bespoke bubbletea Model for the LIVE proof-of-control screen
// (flow step 6 / spec §V). It is a pick-place-THEN-verify state machine: the
// operator first picks a method (V2), then the screen shows the token +
// instructions + a provider deep-link and waits for an explicit "check now",
// runs the injected verify with a spinner, then renders a specific friendly
// result (V4) with retry / "try a different way" / finish. It quits — marking
// done — only when the operator finishes, so the orchestrator reads m.out and
// records intent in config; an Esc/Ctrl+C at any point quits WITHOUT done so the
// orchestrator treats it as a cancel.
type proofModel struct {
	ctx      context.Context //nolint:containedctx // a one-shot screen scoped to a single Run; carrying ctx lets the bubbletea Cmd call the injected VerifyFunc.
	platform precheck.Platform
	host     string
	method   verify.Method
	token    string
	verify   VerifyFunc
	sp       spinner.Model
	state    proofState
	methods  []verify.Method // the offered methods, recommended-first
	cursor   int             // highlighted method on the picker
	hint     string          // the recommendation hint (empty = no claim)
	out      verify.Outcome
	err      error
	done     bool
}

// NewProofModel builds a proof-of-control screen model on the method picker (§V
// V2) — it does NOT start verifying. platform is the sniffed CMS used to
// recommend the easiest method (pre-highlighted) and pick the right provider
// deep-link; an unknown platform pre-highlights the sensible default with no
// claim. ctx scopes the injected verify AND, when this model is run via
// tea.NewProgram(..., tea.WithContext(ctx)), cancelling ctx deterministically
// tears down the live screen (Program.Run returns tea.ErrProgramKilled);
// host/token are shown in the placement instructions; verify is the seam that
// performs the live check on "check now".
func NewProofModel(ctx context.Context, platform precheck.Platform, host, token string, vf VerifyFunc) proofModel {
	recommended, hint := RecommendMethod(platform)
	methods := proofMethods(recommended)
	return proofModel{
		ctx:      ctx,
		platform: platform,
		host:     host,
		method:   recommended,
		token:    token,
		verify:   vf,
		sp:       spinner.New(spinner.WithSpinner(spinner.Dot)),
		state:    stateMethod,
		methods:  methods,
		cursor:   0, // recommended is first
		hint:     hint,
	}
}

// RunProofScreen runs the LIVE pick-place-then-verify proof screen and returns
// the recorded Outcome plus whether the operator cancelled (Esc/Ctrl+C before
// finishing). It mirrors the established live-screen orchestration: build the
// model, run it under tea.WithContext(ctx) so an external cancellation tears the
// screen down deterministically (Program.Run → tea.ErrProgramKilled), then read
// the final model. A cancelled run (done=false on a clean quit, or an
// ErrProgramKilled) reports cancelled=true so the caller skips persistence. The
// platform is the sniffed CMS used to recommend a method and pick provider hints.
//
// UNTESTED SEAM: tea.Program.Run needs a real terminal, so this is exercised
// only by an integration `rabbot init`. The model's lifecycle (Init/Update/
// View, the pick-place-then-verify state machine, and screenCancelled) is
// unit-tested.
func RunProofScreen(ctx context.Context, in io.Reader, out io.Writer, platform precheck.Platform, host, token string, vf VerifyFunc) (verify.Outcome, bool, error) {
	m := NewProofModel(ctx, platform, host, token, vf)
	final, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		// An external context cancellation kills the program; treat it as a cancel
		// (the same way Esc/Ctrl+C inside the screen does) rather than a hard error.
		if isUserCancel(err) {
			return verify.Outcome{}, true, nil
		}
		return verify.Outcome{}, true, err
	}
	pm, ok := final.(proofModel)
	if !ok {
		return verify.Outcome{}, true, nil
	}
	return pm.out, screenCancelled(pm.done), nil
}

// Init starts the spinner ticking but DOES NOT kick off the verify — the check
// fires only on an explicit "check now" key (the place-then-verify contract).
// m.sp.Tick is the spinner's method VALUE (a tea.Cmd), not a call.
func (m proofModel) Init() tea.Cmd {
	return m.sp.Tick
}

// verifyCmd performs the injected verify and wraps its result in a proofDoneMsg.
// It is dispatched only when the operator chooses "check now" / "check again".
func (m proofModel) verifyCmd() tea.Msg {
	out, err := m.verify(m.ctx, m.method)
	return proofDoneMsg{out: out, err: err}
}

// Update drives the pick-place-then-verify state machine. It uses the v1
// signature Update(tea.Msg) (tea.Model, tea.Cmd). Esc/ctrl+c quit WITHOUT setting
// done at any state, so the orchestrator's screenCancelled(!done) guard treats
// them as a cancel — the same way Esc aborts a huh form. Selecting a method moves
// to statePlace; a "check now"/"check again" key dispatches verifyCmd and enters
// stateChecking; a proofDoneMsg records the outcome and enters stateResult (it
// does NOT quit — the operator may retry or switch methods); a "finish" key from
// the result quits WITH done so the runner persists m.out.
func (m proofModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	case proofDoneMsg:
		// A check completed: record the outcome and show the friendly result.
		// Do NOT quit — the operator can place/fix the token and re-check, switch
		// methods, or finish from the result screen.
		m.out = msg.out
		m.err = msg.err
		m.state = stateResult
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey maps key presses per state. Esc/ctrl+c always cancel (quit without
// done). In stateMethod, up/down move the cursor and Enter selects (→ statePlace
// for that method). In statePlace, Enter starts the check. In stateResult the
// honored keys are derived from the result's action set: "check again" only when
// the reason actually offers a re-check; "try a different way" routes back to the
// method picker; "finish later" quits with done. In stateChecking only the cancel
// keys are honored (the check runs).
func (m proofModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		// Cancel: quit WITHOUT marking done so screenCancelled(!done) fires.
		return m, tea.Quit
	}
	switch m.state {
	case stateMethod:
		return m.handleMethodKey(msg)
	case statePlace:
		if msg.String() == "enter" {
			m.state = stateChecking
			return m, m.verifyCmd
		}
	case stateResult:
		return m.handleResultKey(msg)
	}
	return m, nil
}

// handleMethodKey drives the method picker (§V V2): up/down ('k'/'j') move the
// cursor, Enter selects the highlighted method and enters statePlace for IT,
// re-deriving the placement (PlacementInstructions(method, host, token) is
// rendered from m.method in View).
func (m proofModel) handleMethodKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.methods)-1 {
			m.cursor++
		}
	case "enter":
		m.method = m.methods[m.cursor]
		m.state = statePlace
	}
	return m, nil
}

// handleResultKey maps the result-screen keys to the result's ACTUAL action set
// (§V V4). It never advertises or fires an action the reason doesn't offer: a
// "check again" only re-checks when the action set contains one (NotFound /
// Mismatch / Unreachable); "try a different way" ('t') routes back to the method
// picker (so a redirecting homepage is not a dead end); "finish later" ('f'/'q')
// quits with done so the runner persists m.out.
func (m proofModel) handleResultKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	res := m.result()
	switch msg.String() {
	case "enter", "r":
		// Check again — only if the reason actually offers a re-check. For a
		// redirect there is no check-again action, so Enter does nothing (the
		// operator must switch methods or finish).
		if resultHasCheckAgain(res) {
			m.state = stateChecking
			return m, m.verifyCmd
		}
	case "t":
		// Try a different way — route back to the method picker so the operator can
		// pick a method that doesn't hit the failure (e.g. a redirecting homepage).
		if resultHasTryDifferent(res) {
			m.state = stateMethod
		}
	case "f", "q":
		// Finish: quit and mark done so the orchestrator persists m.out.
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

// View renders the current state. stateMethod shows the method picker; statePlace
// shows the token + placement instructions + provider hint + the "check now"
// prompt; stateChecking shows the spinner; stateResult shows the friendly
// VerifyMessage (headline + detail) plus the available actions — never the raw
// verify.Reason enum.
func (m proofModel) View() string {
	switch m.state {
	case stateMethod:
		return m.methodView()
	case stateChecking:
		return PlacementInstructions(m.method, m.host, m.token) + "\n" +
			m.sp.View() + " " + StyleSubtle.Render("checking "+m.host+" …")
	case stateResult:
		return m.resultView()
	default: // statePlace
		return m.placeView()
	}
}

// methodView renders the method picker (§V V2): the three methods in plain
// language with the cursor-highlighted one marked, plus the recommendation hint
// when non-empty so the recommended method reads as a suggestion, not a default.
func (m proofModel) methodView() string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render("How do you want to prove you control "+m.host+"?") + "\n")
	for i, meth := range m.methods {
		marker := "  "
		label := methodLabel(meth)
		if i == m.cursor {
			marker = StyleTitle.Render("› ")
			label = StyleTitle.Render(label)
		}
		b.WriteString(marker + label + "\n")
	}
	if m.hint != "" {
		b.WriteString(StyleSubtle.Render(m.hint) + "\n")
	}
	b.WriteString(StyleSubtle.Render("↑/↓ to choose · Enter to continue · Esc to finish later") + "\n")
	return b.String()
}

// placeView renders the placement screen (§V V3): the method-specific token
// placement instructions plus the provider deep-link. For a known platform with
// a curated link we show "› <label>: <url>"; otherwise a generic, provider-
// agnostic fallback so there is always actionable guidance.
func (m proofModel) placeView() string {
	var b strings.Builder
	b.WriteString(PlacementInstructions(m.method, m.host, m.token))
	if label, link := ProviderHint(m.platform, m.method); label != "" {
		b.WriteString(StyleSubtle.Render("› "+label+": "+link) + "\n")
	} else {
		b.WriteString(StyleSubtle.Render("Not sure where to put it? See your site/host docs.") + "\n")
	}
	b.WriteString(StyleSubtle.Render("Place the code above, then press Enter to check now (Esc to finish later).") + "\n")
	return b.String()
}

// effectiveReason is the Reason that drives the result copy: a transport/DNS
// error surfaces as ReasonUnreachable regardless of the (zero) Reason on the
// error path; otherwise the outcome's own Reason is used.
func (m proofModel) effectiveReason() verify.Reason {
	if m.err != nil {
		return verify.ReasonUnreachable
	}
	return m.out.Reason
}

// result computes the friendly copy for the recorded check (used by the key
// handlers to discover which actions the current result actually offers).
func (m proofModel) result() VerifyResult {
	return VerifyMessage(m.effectiveReason(), m.host, m.token)
}

// resultView renders the friendly, specific outcome copy for the recorded check.
// A verified result is styled success; a miss stays in the cautionary warn
// palette. The footer is driven by the result's ACTUAL action set so it never
// advertises a key the reason doesn't honor (e.g. no "check again" on a redirect).
func (m proofModel) resultView() string {
	reason := m.effectiveReason()
	res := VerifyMessage(reason, m.host, m.token)

	var b strings.Builder
	b.WriteString(StyleTitle.Render("Proof of control") + "\n")
	if reason == verify.ReasonVerified {
		b.WriteString(StyleSuccess.Render(res.Headline) + "\n")
	} else {
		b.WriteString(StyleWarn.Render(res.Headline) + "\n")
	}
	if res.Detail != "" {
		b.WriteString(res.Detail + "\n")
	}
	b.WriteString(StyleSubtle.Render(resultFooter(res)) + "\n")
	return b.String()
}

// resultFooter builds the key-hint footer from the result's action set so the
// advertised keys exactly match what handleResultKey honors. A verified result
// (no actions) offers only "finish".
func resultFooter(res VerifyResult) string {
	if len(res.Actions) == 0 {
		return "f: finish"
	}
	parts := make([]string, 0, 3)
	if resultHasCheckAgain(res) {
		parts = append(parts, "Enter: check again")
	}
	if resultHasTryDifferent(res) {
		parts = append(parts, "t: try a different way")
	}
	parts = append(parts, "f: finish later")
	return strings.Join(parts, " · ")
}

// resultHasCheckAgain reports whether the result's actions include a re-check
// ("Check again" / "Try again"). Only these reasons (NotFound, Mismatch,
// Unreachable) gain Enter=re-check; a redirect, which can never succeed via the
// same method, does not.
func resultHasCheckAgain(res VerifyResult) bool {
	for _, a := range res.Actions {
		if a == "Check again" || a == "Try again" {
			return true
		}
	}
	return false
}

// resultHasTryDifferent reports whether the result's actions include switching
// methods ("Try a different way"). NotFound and Redirected offer it.
func resultHasTryDifferent(res VerifyResult) bool {
	for _, a := range res.Actions {
		if a == "Try a different way" {
			return true
		}
	}
	return false
}

// PlacementInstructions returns method-specific instructions for placing the
// proof token. It MIRRORS the CLI's printPlacement (internal/cli/verify.go) so
// the wizard and `rabbot verify` give identical guidance — the two must never
// drift. host is the literal host[:port]; token is the public proof token.
func PlacementInstructions(method verify.Method, host, token string) string {
	switch method {
	case verify.MethodWellKnown:
		return fmt.Sprintf("  Place this file:  https://%s/.well-known/rabbot-verify.txt\n", host) +
			fmt.Sprintf("  With contents:    %s\n", token)
	case verify.MethodDNS:
		// DNS resolves names, not host:port pairs — VerifyDNS strips the port via
		// url.URL.Hostname(), so this hint must show the same bare hostname or it
		// would tell the operator to add the record on "example.com:8443" while the
		// lookup targets "example.com". The HTTP-fetch branches keep the full host.
		return fmt.Sprintf("  Add a DNS TXT record on %s:\n", (&url.URL{Host: host}).Hostname()) +
			fmt.Sprintf("    rabbot-verify=%s\n", token)
	case verify.MethodMeta:
		return fmt.Sprintf("  Add to the <head> of https://%s/:\n", host) +
			fmt.Sprintf("    <meta name=\"rabbot-verify\" content=\"%s\">\n", token)
	default:
		return ""
	}
}
