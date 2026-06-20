package wizard

import "fmt"

// ExistingAction is the operator's choice when `rabbot init` finds a config
// that already exists (spec step 1). It is a pure enum so the routing decision is
// unit-testable independent of the huh.Select that collects it.
type ExistingAction int

const (
	// ActionAddSite appends another monitored site to the existing config.
	ActionAddSite ExistingAction = iota + 1
	// ActionReconfigure re-runs the full setup wizard against the existing config.
	ActionReconfigure
	// ActionCancel exits quietly without touching the config.
	ActionCancel
)

// ResolveExistingAction maps a choice string (the value carried by the huh.Select
// option) to an ExistingAction, returning an error for an unknown choice. Keeping
// this pure lets the cli layer drive the production huh.Select while tests assert
// the mapping directly.
func ResolveExistingAction(s string) (ExistingAction, error) {
	switch s {
	case "add":
		return ActionAddSite, nil
	case "reconfigure":
		return ActionReconfigure, nil
	case "cancel":
		return ActionCancel, nil
	default:
		return 0, fmt.Errorf("wizard: unknown existing-config action %q", s)
	}
}
