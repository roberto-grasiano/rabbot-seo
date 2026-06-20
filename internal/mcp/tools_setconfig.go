package mcpsrv

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// ptrBool and mapBridgeError already live in tools.go (this package), so they are
// reused here rather than redefined — a redefinition would be a duplicate-symbol
// compile error. The plan's Task 3 code block lists local copies as a fallback for
// the case where the error-mapping/Phase-3 helpers had not yet landed; in this tree
// they have, so the single shared definitions are used (see plan §New contracts:
// "if two phases both add it, keep one").

// SetConfigInput is the set_config tool input. Only the allow-listed key is
// settable (see config.AllowConfigKey); everything else is rejected loudly with no
// write. The value is applied verbatim but NEVER echoed back (it may be a secret).
type SetConfigInput struct {
	Key   string `json:"key" jsonschema:"the allow-listed config key to set (e.g. log.level, defaults.min_interval)"`
	Value string `json:"value" jsonschema:"the new value for the key"`
}

// SetConfigOutput is the secret-safe acknowledgement. It echoes the KEY only —
// never the value — mirroring the CLI's configSetSuccessLine (finding #20.1), so a
// captured/logged/shared tool result can never leak a notifier webhook or token.
type SetConfigOutput struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// setConfigHandlerCore is the testable core of the set_config tool, decoupled from
// the SDK request type. It enforces the allow-list FIRST (fast, friendly rejection
// with no bridge call / no write), then delegates the write to the bridge, and on
// success returns a key-only acknowledgement. Failures are returned as DATA (OK
// false + friendly message), never as a Go error, so the model always gets a clean
// structured result.
func setConfigHandlerCore(ctx context.Context, b Bridge, key, value string) (SetConfigOutput, error) {
	// Allow-list guard first: reject before any write is attempted. The error
	// names the key + the allowed list, never the value.
	if err := config.AllowConfigKey(key); err != nil {
		return SetConfigOutput{OK: false, Message: err.Error()}, nil
	}
	if err := b.SetConfig(ctx, key, value); err != nil {
		return SetConfigOutput{OK: false, Message: mapBridgeError(err)}, nil
	}
	// Secret-safe echo: name the key, never the value.
	return SetConfigOutput{OK: true, Message: fmt.Sprintf("set %s", key)}, nil
}

// setConfigTool registers the set_config write tool on the server. It is a
// non-destructive, idempotent, closed-world action (DestructiveHint/OpenWorldHint
// set explicitly to false because the SDK defaults them to true).
func setConfigTool(srv *mcp.Server, b Bridge) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "set_config",
		Title: "Set a monitor config value",
		Description: "Set an allow-listed Rabbot-SEO config key (e.g. log.level, " +
			"defaults.min_interval). Only a small allow-list is settable; the unverified " +
			"throttle floor, notifier secrets, and the data location cannot be set. The " +
			"value is never echoed back.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SetConfigInput) (*mcp.CallToolResult, SetConfigOutput, error) {
		out, _ := setConfigHandlerCore(ctx, b, in.Key, in.Value)
		// Return the structured output; the SDK auto-populates Content from it. We
		// never return a Go error — failures are data (OK:false + message).
		return nil, out, nil
	})
}
