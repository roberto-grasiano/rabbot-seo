package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// metricsLoopbackAddr is the single loopback default the metrics listener binds
// when a setup path enables it: 127.0.0.1:9464 — the Prometheus exporter
// convention (decision 12). Off by default (config Defaults() leaves Addr ""); a
// setup path turns it on by writing exactly this value. Shared by the generator,
// the wizard fork, and `init --with-grafana` so every "enable observability"
// route agrees on the bind.
const metricsLoopbackAddr = "127.0.0.1:9464"

// observabilityBundleSubdir is the directory under the config dir into which the
// provisioned bundle is materialised.
const observabilityBundleSubdir = "observability"

func newObservabilityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "observability",
		Short: "Self-observability: generate the Prometheus + Grafana bundle",
	}
	cmd.AddCommand(newObservabilityInitCmd())
	return cmd
}

func newObservabilityInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Enable metrics and write the provisioned Prometheus + Grafana bundle",
		Long: "Deterministic generator: sets metrics.addr to the loopback default " +
			"(only when unset, so a custom address survives re-runs), writes the " +
			"provisioned observability bundle under the config directory, and prints " +
			"the one command to bring the stack up.\n\n" +
			"Rabbot never runs docker — it only writes files and config. Re-running " +
			"is byte-identical, so it is safe to retry.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgDir, err := config.ResolveConfigDir()
			if err != nil {
				return err
			}
			cfgPath := config.ConfigFilePath(cfgDir)
			return runObservabilityInit(cmd, cfgDir, cfgPath)
		},
	}
}

// runObservabilityInit is the shared generator seam every "enable observability"
// route runs: `rabbot observability init`, the wizard's technical path, and
// `init --with-grafana`. It (1) sets metrics.addr to the loopback default but
// ONLY when the current value is unset/empty (a custom addr survives), (2) writes
// the provisioned bundle under <config-dir>/observability/, and (3) prints the
// next steps — the compose command, the Grafana credentials warning, and the
// daemon-restart note. It never prompts and never runs docker (decision 18):
// bring-up belongs to the operator or the agent's own shell.
func runObservabilityInit(cmd *cobra.Command, cfgDir, cfgPath string) error {
	// (1) Enable metrics — but only if not already set, so a custom addr (or a
	// re-run after a hand-edit) is preserved. Read the effective config; an
	// unreadable/absent file means "unset", so we proceed to set the default.
	addrAlreadySet := false
	if loaded, lerr := config.Load(cfgPath, nil); lerr == nil && loaded.Metrics.Addr != "" {
		addrAlreadySet = true
	}
	if !addrAlreadySet {
		// SetKeyYAML is comment-preserving and creates the metrics mapping if
		// absent. It requires a well-formed document at cfgPath; the callers that
		// reach here have already seeded the scaffold (init paths) or the operator
		// ran `rabbot init` first.
		if err := config.SetKeyYAML(cfgPath, "metrics.addr", metricsLoopbackAddr); err != nil {
			return fmt.Errorf("observability: enable metrics.addr: %w", err)
		}
	}

	// (2) Write the provisioned bundle.
	bundleDir := filepath.Join(cfgDir, observabilityBundleSubdir)
	if err := obs.WriteObservabilityBundle(bundleDir); err != nil {
		return fmt.Errorf("observability: write bundle: %w", err)
	}

	// (3) Print the next steps. Rabbot names the command; it never execs docker.
	printObservabilityNextSteps(cmd, bundleDir, addrAlreadySet)
	return nil
}

// printObservabilityNextSteps writes the post-generation guidance: where the
// bundle landed, the single command to bring the stack up, the dashboard URL,
// the Grafana stock-credentials warning, and the daemon-restart note. It exec's
// nothing — the operator (or an agent in its own shell) runs docker.
func printObservabilityNextSteps(cmd *cobra.Command, bundleDir string, addrPreserved bool) {
	out := cmd.OutOrStdout()
	composePath := filepath.Join(bundleDir, "docker-compose.observability.yml")

	_, _ = fmt.Fprintf(out, "Wrote the observability bundle to %s\n", bundleDir)
	if addrPreserved {
		_, _ = fmt.Fprintln(out, "Left metrics.addr unchanged (a non-empty value was already set).")
	} else {
		_, _ = fmt.Fprintf(out, "Set metrics.addr to %s (loopback).\n", metricsLoopbackAddr)
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Next steps:")
	_, _ = fmt.Fprintf(out, "  1. Bring the stack up (Rabbot never runs docker for you):\n")
	_, _ = fmt.Fprintf(out, "       docker compose -f %s up -d\n", composePath)
	_, _ = fmt.Fprintln(out, "  2. Open the dashboard at http://localhost:3000")
	_, _ = fmt.Fprintln(out, "  3. Restart the daemon so it starts serving /metrics "+
		"(e.g. `rabbot stop` then `rabbot run`, or restart the service).")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "WARNING: Grafana starts with the stock admin/admin login and forces a "+
		"password change on first sign-in. Change it immediately, and do not expose "+
		"Grafana (:3000) to the public internet without a proxy/firewall in front.")
}

// runWithGrafana is the non-TTY entrypoint shared by `init --with-grafana`: it
// runs the same generator seam (writes + prints, identical bytes), prompting
// nothing. It resolves the config dir/path itself so the headless init path can
// call it after writing the config. Advisory: a generator failure surfaces but
// is left to the caller to decide whether to propagate.
func runWithGrafana(cmd *cobra.Command) error {
	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		return err
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	// A missing config is a no-op-able edge: the headless path always writes the
	// scaffold before reaching here, so cfgPath exists. Guard anyway so a stray
	// call without a config gives a clear error rather than a confusing YAML one.
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		return fmt.Errorf("observability: --with-grafana needs a config at %s: %w", cfgPath, statErr)
	}
	return runObservabilityInit(cmd, cfgDir, cfgPath)
}
