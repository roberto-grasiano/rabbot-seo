package cli

import (
	"context"

	"github.com/spf13/cobra"

	mcpsrv "github.com/roberto-grasiano/rabbot-seo/internal/mcp"
)

// serveFn is the seam through which the mcp command enters the blocking stdio MCP
// loop. Production points at mcpsrv.Serve (which runs srv.Run over the stdio
// transport); tests replace it with a no-op so the bridge-construction path can be
// exercised without blocking on stdin/stdout.
var serveFn = mcpsrv.Serve

// connectWriteFn is the seam through which the headless path and the wizard runner
// write a Connect-Claude config for a chosen target. Production points at
// connectWrite (which resolves the per-target path and merge-writes via the
// internal/mcp writer); tests replace it to assert the chosen target flows through
// without touching the real per-OS Claude path. It returns the written path.
var connectWriteFn = connectWrite

// connectWrite resolves the binary + per-target path and merge-writes the
// Connect-Claude config via the internal/mcp writer, returning the written path.
// The "print" target writes no file and returns an empty path (the caller prints
// the snippet instead).
func connectWrite(target string) (string, error) {
	tgt, err := mcpsrv.ParseTarget(target)
	if err != nil {
		return "", err
	}
	if tgt == mcpsrv.TargetPrint {
		return "", nil
	}
	bin := mcpsrv.ResolveBinary()
	path, err := mcpsrv.TargetPath(tgt)
	if err != nil {
		return "", err
	}
	if err := mcpsrv.WriteConfig(path, bin); err != nil {
		return "", err
	}
	return path, nil
}

// connectWriteDirs is connectWrite with non-default --data-dir/--config baked
// into the launch args, for a daemon under a custom dir. path is the explicit
// target file (the caller resolves it via mcpsrv.TargetPath). NOTE: of the two,
// only --config governs Hop-2 reachability — the control.token lives in the config
// dir and the child reads it from there (helpers.go newControlClientFromConfigDir).
// --data-dir is baked for forward-compat only: the child no longer opens the DB,
// so the data dir affects neither the token nor the control port today.
func connectWriteDirs(path, bin, dataDir, configPath string) error {
	return mcpsrv.WriteConfigWithDirs(path, bin, dataDir, configPath)
}

// connectWriteRemote writes the SSH-transport (remote/VPS) connect config to
// path: Claude launches `ssh <dest> <remoteBin> mcp`, so the child runs on the
// VPS beside the daemon and the token never leaves the box (D9).
func connectWriteRemote(path, dest, remoteBin string) error {
	return mcpsrv.WriteRemoteConfig(path, dest, remoteBin)
}

// newMCPCmd builds the `rabbot mcp` command: a STDIO-ONLY, read-only MCP server
// that exposes the daemon's health/status and the monitored sites. It mirrors
// newDoctorCmd's shape — load config, build the loopback control client, then run.
//
// CRITICAL: stdout is the MCP JSON-RPC channel, so this command writes NOTHING to
// stdout itself; any user-facing notice would go to stderr. The RunE is thin: all
// server logic lives in internal/mcp behind the Bridge seam.
func newMCPCmd(bi BuildInfo) *cobra.Command {
	var dataDir, configPath string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run a read-only MCP server over stdio (connect Claude to this monitor)",
		Long: "mcp runs a Model Context Protocol server over STDIO (stdin/stdout) — there is " +
			"no network endpoint. It talks to the running daemon over the loopback control " +
			"API. An MCP client (e.g. Claude Desktop / Claude Code) launches this command; see " +
			"`rabbot init --connect-claude` to generate the client config. Pass --data-dir/" +
			"--config to match a daemon running under a non-default directory. stdout is the " +
			"MCP channel, so this command prints nothing there.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfigFrom(configPath, dataDir)
			if err != nil {
				return err
			}
			client, err := newControlClientFromConfigDir(cfg, configPath)
			if err != nil {
				return err
			}
			bridge := mcpsrv.NewControlBridge(client)

			// cobra's Execute guarantees a non-nil context in RunE, so this guard is
			// dead in production; it exists so direct unit-test calls (which construct
			// a *cobra.Command without Execute) get a usable context.
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Disconnect classification is single-owner: mcpsrv.Serve normalizes a
			// routine client disconnect (stdin closed → io.EOF) and Ctrl-C
			// (context.Canceled), plus the SDK's ErrConnectionClosed / closing
			// substrings, to a nil error (see internal/mcp.isGracefulDisconnect).
			// So any non-nil return here is a genuine failure — surface it. The
			// command must NOT re-filter, or it would mask real errors that happen
			// to wrap io.EOF/context.Canceled for unrelated reasons.
			return serveFn(ctx, bridge, bi.Version)
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "",
		"data directory the daemon uses (default: per-OS data dir); match the daemon's --data-dir")
	cmd.Flags().StringVar(&configPath, "config", "",
		"path to config.yaml (default: per-OS config dir); match the daemon's --config")
	return cmd
}
