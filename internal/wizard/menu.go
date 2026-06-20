package wizard

import (
	"errors"
	"io"

	"github.com/charmbracelet/huh"
)

// Upgrade is one optional post-go-live step the user can pick from the menu.
type Upgrade int

const (
	UpgradeVerify        Upgrade = iota // unlock full-speed monitoring (recommended)
	UpgradeAlerts                       // get notified when it changes (Slack)
	UpgradeConnectGSC                   // connect Google Search Console (read Google's ground truth)
	UpgradeService                      // keep watching 24/7 (install a service)
	UpgradeConnectClaude                // connect Claude (runs the existing behavior, unchanged)
	UpgradeGrafana                      // see it on a dashboard (Prometheus + Grafana)
	UpgradeFineTune                     // cadence/scope (advanced)
)

// ── Item D: re-enterable upgrade menu with back-navigation ────────────────────────
//
// MenuChoice is one selectable row of the re-enterable post-go-live menu. Unlike the
// legacy multi-select (pick-many-then-run-all), the loop presents these ONE at a time:
// the operator picks an action, it runs, the menu RE-APPEARS, and they pick another —
// finishing only on the explicit MenuFinish row (or by backing out with Esc, which is a
// clean no-op at the top, never a wizard crash — finding #3).
type MenuChoice int

const (
	// MenuVerify..MenuFineTune map 1:1 onto the matching Upgrade action.
	MenuVerify MenuChoice = iota
	MenuAlerts
	MenuConnectGSC
	MenuService
	MenuConnectClaude
	MenuGrafana
	MenuFineTune
	// MenuFinish is the explicit "I'm all set" row that ends the loop. Esc at the menu
	// is ALSO a clean finish (the optional menu, declined, when you're already live).
	MenuFinish
)

// menuStep is the pure mapping from a chosen MenuChoice to a loop step: either run the
// matching upgrade and re-enter the menu (run=true, with the action), or finish the loop
// (finish=true). MenuFinish — and any unknown value, defensively — ends the loop.
func menuStep(c MenuChoice) (action Upgrade, run bool, finish bool) {
	switch c {
	case MenuVerify:
		return UpgradeVerify, true, false
	case MenuAlerts:
		return UpgradeAlerts, true, false
	case MenuConnectGSC:
		return UpgradeConnectGSC, true, false
	case MenuService:
		return UpgradeService, true, false
	case MenuConnectClaude:
		return UpgradeConnectClaude, true, false
	case MenuGrafana:
		return UpgradeGrafana, true, false
	case MenuFineTune:
		return UpgradeFineTune, true, false
	default: // MenuFinish (and any unknown choice) → finish
		return 0, false, true
	}
}

// runUpgradeMenu drives the re-enterable menu loop (Item D). It repeatedly calls prompt
// to get the next MenuChoice, runs the matching action via run, and loops back to the
// menu — so the operator can pick Verify, return, pick Slack, …, then explicitly Finish.
// It stops on:
//   - MenuFinish (the explicit "I'm all set" row) → clean finish (nil),
//   - an Esc/Ctrl-C abort at the menu (huh.ErrUserAborted, possibly wrapped) → clean
//     finish (nil): the user is already live, so backing out of the OPTIONAL menu is a
//     normal exit — it must NEVER crash or abnormally complete the wizard (finding #3),
//   - any OTHER prompt error → propagated (a real TTY failure is not swallowed).
//
// prompt is the injectable seam (production: a huh single-select form). The loop logic
// here is unit-tested directly; the huh.Form.Run inside the production prompt is the only
// untested TTY seam.
func runUpgradeMenu(prompt func() (MenuChoice, error), run func(Upgrade)) error {
	for {
		choice, err := prompt()
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil // Esc at the top: clean finish, never a wizard crash
			}
			return err
		}
		action, doRun, finish := menuStep(choice)
		if finish {
			return nil
		}
		if doRun {
			run(action)
		}
	}
}

// PromptUpgradeMenu is the production driver for the re-enterable menu (Item D). It builds
// the per-iteration single-select prompt seam (a huh.Select that lets the operator pick one
// action or "I'm all set", with Esc as a clean back-out) and hands it to runUpgradeMenu,
// which runs each chosen action via the supplied run callback and re-presents the menu until
// the operator finishes.
//
// UNTESTED SEAM: the huh.Form.Run inside promptOne needs a real terminal, so it is exercised
// only by an integration `rabbot init`; the loop logic (runUpgradeMenu / menuStep) is
// unit-tested directly.
func PromptUpgradeMenu(in io.Reader, out io.Writer, run func(Upgrade)) error {
	promptOne := func() (MenuChoice, error) {
		var choice MenuChoice
		form := huh.NewForm(huh.NewGroup(
			huh.NewSelect[MenuChoice]().
				Title("You're live ✨ — want to set up any of these?").
				Description("Pick one to set it up, then you'll come back here. Choose \"I'm all set\" "+
					"when you're done — everything works without these.").
				Options(
					huh.NewOption("Unlock full-speed monitoring (recommended)", MenuVerify),
					huh.NewOption("Get notified when it changes", MenuAlerts),
					huh.NewOption("Connect Google Search Console (Google's own index + search data)", MenuConnectGSC),
					huh.NewOption("Keep watching 24/7 (run in the background)", MenuService),
					huh.NewOption("Connect Claude (ask an AI about your site)", MenuConnectClaude),
					huh.NewOption("See it on a dashboard (Prometheus + Grafana)", MenuGrafana),
					huh.NewOption("Fine-tune (how often we check, and more)", MenuFineTune),
					huh.NewOption("I'm all set — finish", MenuFinish).Selected(true),
				).
				Value(&choice),
		)).WithInput(in).WithOutput(out)
		// Read the run error first, THEN choice: huh writes the selection into choice via the
		// &choice binding during Run (Go evaluates call args left-to-right, so returning choice
		// inline alongside form.Run() would snapshot it before Run populates it).
		err := form.Run()
		return choice, err
	}
	return runUpgradeMenu(promptOne, run)
}
