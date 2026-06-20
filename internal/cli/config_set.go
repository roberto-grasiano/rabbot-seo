// internal/cli/config_set.go
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/spf13/cobra"
)

// runConfigSet parses a "key=value" (or two-arg "key value") mutation and POSTs
// it to the daemon, which writes config.yaml and reloads (§3.4 S1).
func runConfigSet(ctx context.Context, client *control.Client, kv string) error {
	key, value, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("config set: expected key=value, got %q", kv)
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return fmt.Errorf("config set: empty key in %q", kv)
	}
	return client.SetConfig(ctx, key, value)
}

// configSetSuccessLine writes the acknowledgement for a successful `config set`
// mutation. It echoes ONLY the key parsed from the "key=value" argument — never
// the value, which may be a secret (e.g. notifiers.0.url=https://hooks.slack.com/
// services/...). CLAUDE.md forbids surfacing webhook URLs/tokens, and this line
// goes to stdout where it may be captured, logged, or shared (finding #20.1).
func configSetSuccessLine(w io.Writer, kv string) error {
	key, _, _ := strings.Cut(kv, "=")
	key = strings.TrimSpace(key)
	_, err := fmt.Fprintf(w, "set %s\n", key)
	return err
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key=value>",
		Short: "Set a config value (writes config.yaml + reloads via the daemon)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withControlClient(cmd, func(ctx context.Context, client *control.Client) error {
				if err := runConfigSet(ctx, client, args[0]); err != nil {
					return err
				}
				// Echo only the key — never the value (which may be a secret such as a
				// Slack webhook URL). See configSetSuccessLine / finding #20.1.
				return configSetSuccessLine(cmd.OutOrStdout(), args[0])
			})
		},
	}
}
