package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// runStop asks the daemon to gracefully shut down via POST /v1/shutdown. On
// success it prints a "stopping" line. If the daemon is not running (the
// connection is refused -> control.ErrDaemonNotRunning) it prints a friendly
// "nothing to stop" line and returns nil so the command exits 0 — stopping an
// already-stopped daemon is not an error. Any other error (e.g. a bad control
// token -> ErrUnauthorized) is surfaced to the caller.
func runStop(ctx context.Context, client *control.Client, w io.Writer) error {
	if err := client.Shutdown(ctx); err != nil {
		if errors.Is(err, control.ErrDaemonNotRunning) {
			_, perr := fmt.Fprintln(w, "daemon not running (nothing to stop).")
			return perr
		}
		return err
	}
	_, err := fmt.Fprintln(w, "stopping the daemon…")
	return err
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Gracefully stop the running daemon (POST /v1/shutdown)",
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				return runStop(ctx, client, c.OutOrStdout())
			})
		},
	}
}
