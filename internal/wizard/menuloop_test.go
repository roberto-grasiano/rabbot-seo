package wizard

import (
	"errors"
	"testing"

	"github.com/charmbracelet/huh"
)

// TestMenuChoiceMapping pins the pure mapping from a menu choice to a loop step
// (Item D): each upgrade maps to "run that upgrade, then re-enter the menu", and the
// explicit Finish choice ends the loop.
func TestMenuChoiceMapping(t *testing.T) {
	cases := []struct {
		choice     MenuChoice
		wantFinish bool
		wantRun    bool
		wantAction Upgrade
	}{
		{MenuVerify, false, true, UpgradeVerify},
		{MenuAlerts, false, true, UpgradeAlerts},
		{MenuConnectGSC, false, true, UpgradeConnectGSC},
		{MenuService, false, true, UpgradeService},
		{MenuConnectClaude, false, true, UpgradeConnectClaude},
		{MenuFineTune, false, true, UpgradeFineTune},
		{MenuFinish, true, false, 0},
	}
	for _, tc := range cases {
		action, run, finish := menuStep(tc.choice)
		if finish != tc.wantFinish {
			t.Errorf("menuStep(%v) finish = %v, want %v", tc.choice, finish, tc.wantFinish)
		}
		if run != tc.wantRun {
			t.Errorf("menuStep(%v) run = %v, want %v", tc.choice, run, tc.wantRun)
		}
		if run && action != tc.wantAction {
			t.Errorf("menuStep(%v) action = %v, want %v", tc.choice, action, tc.wantAction)
		}
	}
}

// TestRunUpgradeMenu_LoopReEntersUntilFinish is the heart of Item D: the menu is a
// re-enterable loop — the operator picks Verify, returns, picks Slack, then explicitly
// Finishes. The loop runs each chosen action IN PICK ORDER and stops only on Finish.
func TestRunUpgradeMenu_LoopReEntersUntilFinish(t *testing.T) {
	script := []MenuChoice{MenuVerify, MenuAlerts, MenuFinish}
	i := 0
	prompt := func() (MenuChoice, error) {
		c := script[i]
		i++
		return c, nil
	}
	var ran []Upgrade
	run := func(u Upgrade) { ran = append(ran, u) }

	if err := runUpgradeMenu(prompt, run); err != nil {
		t.Fatalf("runUpgradeMenu returned err: %v", err)
	}
	if len(ran) != 2 || ran[0] != UpgradeVerify || ran[1] != UpgradeAlerts {
		t.Fatalf("ran = %v, want [verify alerts] in pick order", ran)
	}
	if i != len(script) {
		t.Fatalf("loop consumed %d prompts, want %d (must finish exactly on Finish)", i, len(script))
	}
}

// TestRunUpgradeMenu_EscAtTopIsCleanFinish covers Item D's "Esc steps back / is a no-op
// at the top rather than completing the wizard": an Esc on the menu (huh.ErrUserAborted
// from the prompt seam) ends the OPTIONAL menu loop cleanly — it never propagates as a
// failure and never crashes the wizard. The operator is already live, so backing out of
// the menu is a normal exit.
func TestRunUpgradeMenu_EscAtTopIsCleanFinish(t *testing.T) {
	prompt := func() (MenuChoice, error) {
		return 0, huh.ErrUserAborted // Esc at the menu
	}
	ran := 0
	run := func(Upgrade) { ran++ }

	if err := runUpgradeMenu(prompt, run); err != nil {
		t.Fatalf("Esc at the menu must be a clean finish, got err: %v", err)
	}
	if ran != 0 {
		t.Fatalf("Esc at the menu must run no actions, ran %d", ran)
	}
}

// TestRunUpgradeMenu_BackThenFinish covers re-entry after running an action: pick an
// action, run it, return to the menu, then Esc/Finish — the loop must NOT keep looping
// forever and must end cleanly after the explicit exit.
func TestRunUpgradeMenu_BackThenFinish(t *testing.T) {
	script := []MenuChoice{MenuService, MenuFinish}
	i := 0
	prompt := func() (MenuChoice, error) {
		c := script[i]
		i++
		return c, nil
	}
	var ran []Upgrade
	if err := runUpgradeMenu(prompt, func(u Upgrade) { ran = append(ran, u) }); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(ran) != 1 || ran[0] != UpgradeService {
		t.Fatalf("ran = %v, want [service]", ran)
	}
}

// TestRunUpgradeMenu_RealPromptErrorPropagates: a non-abort error from the prompt seam
// (e.g. a TTY failure) is surfaced, not swallowed as a clean finish.
func TestRunUpgradeMenu_RealPromptErrorPropagates(t *testing.T) {
	sentinel := errors.New("tty exploded")
	prompt := func() (MenuChoice, error) { return 0, sentinel }
	if err := runUpgradeMenu(prompt, func(Upgrade) {}); !errors.Is(err, sentinel) {
		t.Fatalf("a real prompt error must propagate, got %v", err)
	}
}
