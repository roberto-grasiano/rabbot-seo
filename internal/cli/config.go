package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and modify configuration",
	}
	cmd.AddCommand(
		newConfigPathCmd(),
		newConfigValidateCmd(),
		newConfigGetCmd(),
		newConfigSetCmd(),
	)
	return cmd
}

func resolveConfigPath() (string, error) {
	dir, err := config.ResolveConfigDir()
	if err != nil {
		return "", err
	}
	return config.ConfigFilePath(dir), nil
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveConfigPath()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "config OK")
			return nil
		},
	}
}

// resolveConfigGet renders the current value of a gettable config key, or reports
// found=false for an unknown/secret key. It is SYMMETRIC with `config set`: every
// key in config.AllowConfigKey (the settable allow-list) is gettable here, plus the
// legacy read-only keys (control.port, data_dir, crawler.contact_email). Secret-
// family keys (notifiers.*, the unverified-throttle floor) are deliberately absent,
// so a value that could leak a webhook URL or token is never printed — they fall
// through to found=false exactly like an unknown key. A settable key that is unset
// (e.g. max_pages_per_site, a *int that is nil when never written) renders as the
// empty string but is still a KNOWN key (found=true), so the operator learns it is a
// valid key that simply has no override yet, rather than getting "unknown key".
func resolveConfigGet(cfg *config.Config, key string) (value string, found bool, err error) {
	switch key {
	case "control.port":
		return strconv.Itoa(cfg.Control.Port), true, nil
	case "log.level":
		return cfg.Log.Level, true, nil
	case "data_dir":
		return cfg.DataDir, true, nil
	case "crawler.contact_email":
		return cfg.Crawler.ContactEmail, true, nil
	case "defaults.min_interval":
		return cfg.Defaults.MinInterval, true, nil
	case "defaults.max_interval":
		return cfg.Defaults.MaxInterval, true, nil
	case "defaults.discovery.max_pages_per_site":
		if cfg.Defaults.Discovery.MaxPagesPerSite == nil {
			return "", true, nil
		}
		return strconv.Itoa(*cfg.Defaults.Discovery.MaxPagesPerSite), true, nil
	default:
		return "", false, nil
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print a configuration value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			value, found, rerr := resolveConfigGet(cfg, args[0])
			if rerr != nil {
				return rerr
			}
			if !found {
				return fmt.Errorf("unknown config key: %s", args[0])
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), value)
			return nil
		},
	}
}
