package wizard

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// Shared lipgloss style tokens — the visual language the bespoke bubbletea
// screens (precheck/proof) and the huh forms reuse so the wizard reads as one
// coherent UI. They are package-level Styles (lipgloss.Style is a value type;
// these are cheap to copy and safe to share read-only). lipgloss strips ANSI
// automatically on a non-tty or under NO_COLOR, so rendering on a dumb terminal
// degrades to the plain text these wrap.
//
// Palette: a calm violet for titles, a faint grey for secondary text, and the
// canonical traffic-light triad (green/yellow/red) reused for both precheck
// verdicts and proof-of-control states so a user learns the colors once.
var (
	// StyleTitle is the bold accent style for screen/section titles.
	StyleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	// StyleSubtle dims secondary/help text.
	StyleSubtle = lipgloss.NewStyle().Faint(true)
	// StyleSuccess is the green "good to go" style (VerdictGreen / StateVerified).
	StyleSuccess = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2ECC71"))
	// StyleWarn is the yellow "proceed with caveats" style (VerdictYellow /
	// StateAttested / StateThrottled).
	StyleWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F1C40F"))
	// StyleError is the red "do not expect reliable monitoring" style (VerdictRed).
	StyleError = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E74C3C"))
)

// VerdictStyle maps a precheck Verdict to its traffic-light style. An unknown
// verdict falls back to the cautionary warn style (never a panic).
func VerdictStyle(v precheck.Verdict) lipgloss.Style {
	switch v {
	case precheck.VerdictGreen:
		return StyleSuccess
	case precheck.VerdictRed:
		return StyleError
	default:
		return StyleWarn
	}
}

// StateStyle maps a proof-of-control State to its traffic-light style. Only a
// successful verify earns the green success style; attested and throttled both
// render as the cautionary warn style (the site stays throttled until a real
// verify succeeds). An unknown state falls back to warn (never a panic).
func StateStyle(s verify.State) lipgloss.Style {
	switch s {
	case verify.StateVerified:
		return StyleSuccess
	case verify.StateAttested, verify.StateThrottled:
		return StyleWarn
	default:
		return StyleWarn
	}
}
