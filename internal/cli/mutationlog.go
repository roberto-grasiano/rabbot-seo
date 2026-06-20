package cli

import (
	"log/slog"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// logMutation runs fn (a control mutation) and emits exactly one structured audit
// log line for it, regardless of which caller drove the control endpoint (the CLI
// or an MCP action tool both land on the same hook). attrs carries action-specific,
// NON-SECRET fields (e.g. obs.KeySite, obs.KeyNotifier name) — callers must never
// pass a secret value (webhook URLs, tokens); the mutation hooks already operate on
// keys/names, not secrets. On success the line is INFO with outcome=ok; on failure
// it is ERROR with outcome=error and the error string, and the original error is
// returned UNCHANGED so the control layer's status mapping is preserved.
func logMutation(logger *slog.Logger, action string, attrs map[string]any, fn func() error) error {
	err := fn()
	args := []any{obs.KeyComponent, "control", obs.KeyAction, action}
	for k, v := range attrs {
		args = append(args, k, v)
	}
	if err != nil {
		args = append(args, "outcome", "error", obs.KeyError, err.Error())
		logger.Error("control mutation", args...)
		return err
	}
	args = append(args, "outcome", "ok")
	logger.Info("control mutation", args...)
	return err
}
