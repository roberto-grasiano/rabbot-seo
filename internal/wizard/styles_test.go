package wizard

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// renderContains asserts that styling s.Render(in) does not panic and that the
// plain input substring survives. lipgloss strips ANSI when NO_COLOR / a
// non-tty is detected, so we assert the PLAIN substring is present — never exact
// escape codes (which are environment dependent and would make the test brittle).
func renderContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("rendered output %q does not contain %q", got, want)
	}
}

func TestStylesRenderPreservesInput(t *testing.T) {
	cases := []struct {
		name   string
		render func(string) string
	}{
		{"title", func(s string) string { return StyleTitle.Render(s) }},
		{"subtle", func(s string) string { return StyleSubtle.Render(s) }},
		{"success", func(s string) string { return StyleSuccess.Render(s) }},
		{"warn", func(s string) string { return StyleWarn.Render(s) }},
		{"error", func(s string) string { return StyleError.Render(s) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.render("Welcome")
			renderContains(t, out, "Welcome")
			if out == "" {
				t.Fatalf("%s rendered empty string", tc.name)
			}
		})
	}
}

func TestStylesVerdictStyleCoversAllVerdicts(t *testing.T) {
	verdicts := []precheck.Verdict{
		precheck.VerdictGreen,
		precheck.VerdictYellow,
		precheck.VerdictRed,
	}
	for _, v := range verdicts {
		st := VerdictStyle(v)
		out := st.Render("X")
		renderContains(t, out, "X")
		if out == "" {
			t.Fatalf("VerdictStyle(%q) rendered empty", v)
		}
	}
}

func TestStylesStateStyleCoversAllStates(t *testing.T) {
	states := []verify.State{
		verify.StateVerified,
		verify.StateAttested,
		verify.StateThrottled,
	}
	for _, s := range states {
		st := StateStyle(s)
		out := st.Render("X")
		renderContains(t, out, "X")
		if out == "" {
			t.Fatalf("StateStyle(%q) rendered empty", s)
		}
	}
}
