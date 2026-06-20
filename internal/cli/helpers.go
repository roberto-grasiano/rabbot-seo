package cli

import (
	"context"
	"net"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// loadConfig resolves the per-OS config path and loads the effective Config.
// Every subcommand uses this; later milestones may layer command flags on top.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	dir, err := config.ResolveConfigDir()
	if err != nil {
		return nil, err
	}
	c, err := config.Load(config.ConfigFilePath(dir), nil)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// databasePath returns the SQLite database file path for the configured data
// dir. When DataDir is empty (the default), it resolves to the same per-OS
// XDG data directory the daemon uses — never a relative CWD path.
func databasePath(cfg *config.Config) string {
	return filepath.Join(config.DataDirPath(cfg.DataDir), "rabbot.db")
}

// instanceKeyPath returns the per-instance secret key file path in the same data
// dir as the database, mirroring databasePath so the CLI and daemon agree on the
// key location regardless of DataDir override.
func instanceKeyPath(cfg *config.Config) string {
	return filepath.Join(config.DataDirPath(cfg.DataDir), "instance.key")
}

// controlAddr returns the loopback control-API address "127.0.0.1:<port>".
func controlAddr(cfg *config.Config) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Control.Port))
}

// newControlClient builds a typed control-API client for the configured port,
// loading the bearer token from <config-dir>/control.token.
func newControlClient(cfg *config.Config) (*control.Client, error) {
	dir, err := config.ResolveConfigDir()
	if err != nil {
		return nil, err
	}
	token, err := control.LoadOrCreateToken(filepath.Join(dir, "control.token"))
	if err != nil {
		return nil, err
	}
	return control.NewClient(cfg.Control.Port, token), nil
}

// withControlClient is the shared preamble for daemon-routed commands: it loads the
// effective config, builds the loopback control client (token from
// <config-dir>/control.token), and invokes fn with the command's context and that
// client. ~10 mutating/status commands previously repeated this exact load+construct
// boilerplate; this is its single home. A control client holds no resource to close,
// so there is nothing to defer here — the store variant (withStore) does the
// Close. Behaviour is identical to the inline version it replaces.
func withControlClient(cmd *cobra.Command, fn func(context.Context, *control.Client) error) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	client, err := newControlClient(cfg)
	if err != nil {
		return err
	}
	return fn(cmd.Context(), client)
}

// withStore is the shared preamble for the read-only store commands: it loads the
// effective config, opens the SQLite store at the resolved database path, and
// invokes fn with the command's context and the open *store.DB, guaranteeing Close
// on return (the defer that every inline caller previously wrote by hand).
// Behaviour is identical to the inline version it replaces.
func withStore(cmd *cobra.Command, fn func(context.Context, *store.DB) error) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	db, err := store.Open(cmd.Context(), databasePath(cfg))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(cmd.Context(), db)
}

// loadConfigFrom loads config from an explicit config-file path (empty → the
// per-OS default), applying an explicit data-dir override. It is the flag-driven
// sibling of loadConfig, used by `rabbot mcp`. The load-bearing input is
// --config: its directory holds control.token (read by newControlClientFromConfigDir
// below), which is what makes the child reach the same daemon. --data-dir is
// accepted for forward-compat but no longer governs reads — the control bridge
// fetches everything over the loopback client and opens no DB.
func loadConfigFrom(configPath, dataDir string) (*config.Config, error) {
	if configPath == "" {
		dir, err := config.ResolveConfigDir()
		if err != nil {
			return nil, err
		}
		configPath = config.ConfigFilePath(dir)
	}
	c, err := config.Load(configPath, nil)
	if err != nil {
		return nil, err
	}
	if dataDir != "" {
		c.DataDir = dataDir
	}
	return &c, nil
}

// newControlClientFromConfigDir builds a control client whose token is read from
// the directory containing configPath (empty → the per-OS config dir). This keeps
// the token co-located with the config the daemon used, so a custom --config dir
// stays coherent.
func newControlClientFromConfigDir(cfg *config.Config, configPath string) (*control.Client, error) {
	var dir string
	if configPath != "" {
		dir = filepath.Dir(configPath)
	} else {
		d, err := config.ResolveConfigDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	token, err := control.LoadOrCreateToken(filepath.Join(dir, "control.token"))
	if err != nil {
		return nil, err
	}
	return control.NewClient(cfg.Control.Port, token), nil
}
