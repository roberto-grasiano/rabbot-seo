package wizard

import (
	"context"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

// RunFunc is the injected seam for the LIVE precheck. Production closes over
// precheck.Run + the resolved Options; tests inject a stub that returns a fixture
// Report, so the model's lifecycle is unit-testable without any network.
type RunFunc func(context.Context) (precheck.Report, error)

// precheckDoneMsg carries the precheck outcome back into the bubbletea loop.
type precheckDoneMsg struct {
	rep precheck.Report
	err error
}

// precheckModel is the bespoke bubbletea Model for the LIVE precheck screen
// (flow step 5). It shows a spinner while the (injected) precheck runs, then
// renders the verdict + summary + per-signal lines. It self-quits once the
// precheck completes so the orchestrator can read its final state and move on.
type precheckModel struct {
	ctx  context.Context //nolint:containedctx // a one-shot screen scoped to a single Run; carrying ctx lets the bubbletea Cmd call the injected RunFunc.
	url  string
	run  RunFunc
	sp   spinner.Model
	rep  precheck.Report
	err  error
	done bool
}

// NewPrecheckModel builds a precheck screen model. ctx scopes the injected run
// AND, when this model is run via tea.NewProgram(..., tea.WithContext(ctx)),
// cancelling ctx deterministically tears down the live screen (Program.Run
// returns tea.ErrProgramKilled); url is shown beside the spinner; run is the seam
// that performs the check.
func NewPrecheckModel(ctx context.Context, url string, run RunFunc) precheckModel {
	return precheckModel{
		ctx: ctx,
		url: url,
		run: run,
		sp:  spinner.New(spinner.WithSpinner(spinner.Dot)),
	}
}

// Init starts the spinner ticking and kicks off the precheck concurrently.
// m.sp.Tick is the spinner's method VALUE (a tea.Cmd), not a call.
func (m precheckModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, m.runCmd)
}

// runCmd performs the injected precheck and wraps its result in a precheckDoneMsg.
func (m precheckModel) runCmd() tea.Msg {
	rep, err := m.run(m.ctx)
	return precheckDoneMsg{rep: rep, err: err}
}

// Update advances the spinner, records the precheck outcome (then quits), and
// honors ctrl+c / Esc as a cancel. It uses the v1 signature
// Update(tea.Msg) (tea.Model, tea.Cmd). Esc/ctrl+c quit WITHOUT setting done, so
// the orchestrator's screenCancelled(!done) guard treats them as a cancel — the
// same way Esc aborts a huh form.
func (m precheckModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	case precheckDoneMsg:
		m.rep = msg.rep
		m.err = msg.err
		m.done = true
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// View shows the spinner while the precheck runs, then the rendered report (or a
// styled error line on failure).
func (m precheckModel) View() string {
	if !m.done {
		return m.sp.View() + " " + StyleSubtle.Render("checking "+m.url+" …")
	}
	if m.err != nil {
		return StyleError.Render("precheck failed: " + m.err.Error())
	}
	return RenderReport(m.rep)
}

// RenderReport renders a precheck.Report to a styled, plain-text block: the
// traffic-light verdict label, the honest summary, the reused preflight facts,
// the JS-rendering read, and every evaluated signal (fired or not) so the verdict
// is never a black box. It is pure (no tea loop) and unit-tested directly. It
// mirrors the doctor command's text output so the wizard and `rabbot doctor`
// agree.
func RenderReport(rep precheck.Report) string {
	var b strings.Builder

	label := precheckVerdictLabel(rep.Verdict)
	b.WriteString(StyleTitle.Render("Precheck") + "  " + VerdictStyle(rep.Verdict).Render(label))
	b.WriteString("\n")
	b.WriteString(rep.Summary)
	b.WriteString("\n\n")

	d := rep.Doctor
	b.WriteString(StyleSubtle.Render("Preflight") + "\n")
	b.WriteString("  homepage status: " + strconv.Itoa(d.HomepageStatus) + "\n")
	robots := d.RobotsVerdict
	if robots == "" {
		robots = "unknown"
	}
	b.WriteString("  robots:          " + robots + "\n")
	if d.Blocked {
		b.WriteString("  " + StyleError.Render("blocked") + "\n")
	}

	js := rep.JS
	b.WriteString("\n" + StyleSubtle.Render("Rendering check") + "\n")
	b.WriteString("  render mode:     " + string(js.Kind) + " (confidence: " + string(js.Confidence) + ")\n")
	b.WriteString("  visible words:   " + strconv.Itoa(js.VisibleWordCount) + "\n")

	b.WriteString("\n" + StyleSubtle.Render("Signals") + "\n")
	for _, s := range js.Signals {
		b.WriteString("  [" + signalMark(s.Present) + "] " + s.Name + "  " + StyleSubtle.Render(s.Detail) + "\n")
	}
	return b.String()
}

// precheckVerdictLabel renders an uppercase verdict label (mirrors doctor.go).
func precheckVerdictLabel(v precheck.Verdict) string {
	switch v {
	case precheck.VerdictGreen:
		return "GREEN"
	case precheck.VerdictRed:
		return "RED"
	default:
		return "YELLOW"
	}
}

// signalMark renders a checkbox for a fired/absent signal.
func signalMark(present bool) string {
	if present {
		return "x"
	}
	return " "
}
