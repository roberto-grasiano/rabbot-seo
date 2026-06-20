package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// newDBCmd is the `db` maintenance command group.
func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "db", Short: "Database maintenance"}
	cmd.AddCommand(newDBCompactCmd())
	return cmd
}

func newDBCompactCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compact",
		Short: "Reclaim disk space by running VACUUM (stop the daemon first)",
		Long: "Rebuilds the database file with VACUUM, returning freed pages to the OS. " +
			"VACUUM needs exclusive access, so the daemon must be stopped first " +
			"(rabbot stop). Day-to-day growth is already bounded by the retention " +
			"sweep; compact is for a one-time shrink after a large cleanup.",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig(c)
			if err != nil {
				return err
			}
			client, err := newControlClient(cfg)
			if err != nil {
				return err
			}
			return runDBCompact(c.Context(), client, databasePath(cfg), c.OutOrStdout())
		},
	}
}

// runDBCompact refuses to run while a daemon is reachable on the control port
// (VACUUM would contend for the exclusive lock), then opens the database and
// compacts it, reporting bytes reclaimed.
func runDBCompact(ctx context.Context, client *control.Client, dbPath string, w io.Writer) error {
	// Only ErrDaemonNotRunning (transport refused) is a safe "no daemon" signal.
	// A nil error (healthy), ErrUnauthorized (answered 401), or any other response
	// all mean something is listening — refuse.
	if herr := client.Health(ctx); !errors.Is(herr, control.ErrDaemonNotRunning) {
		return fmt.Errorf("the daemon appears to be running — stop it first with `rabbot stop`, then retry `rabbot db compact`")
	}

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Measure `before` AFTER Open so any migration applied on open (e.g. a first-run
	// index build) is already reflected — otherwise "reclaimed" subtracts a
	// pre-migration size from a post-VACUUM size and undercounts (or shows 0).
	// Checkpoint first so both before/after are measured against a settled, WAL-
	// flushed main file. ErrCheckpointBusy is tolerated: the daemon-liveness guard
	// above already ensures no other holder.
	if cpErr := db.Checkpoint(ctx); cpErr != nil && !errors.Is(cpErr, store.ErrCheckpointBusy) {
		return cpErr
	}
	before, _ := os.Stat(dbPath)

	if err := db.Compact(ctx); err != nil {
		return err
	}

	// The DB is WAL mode, so VACUUM rebuilds into the -wal file and the main .db
	// does not shrink until a checkpoint flushes the WAL back into it. Without this,
	// the os.Stat below sees the un-shrunk file and reports "reclaimed 0" for real
	// compactions. ErrCheckpointBusy is tolerated: the daemon-liveness guard above
	// already ensures no other holder, so a busy result is best-effort-ignorable.
	if cpErr := db.Checkpoint(ctx); cpErr != nil && !errors.Is(cpErr, store.ErrCheckpointBusy) {
		return cpErr
	}

	after, _ := os.Stat(dbPath)
	if before != nil && after != nil {
		reclaimed := before.Size() - after.Size()
		if reclaimed < 0 {
			reclaimed = 0
		}
		_, _ = fmt.Fprintf(w, "compacted: %d → %d bytes (reclaimed %d)\n", before.Size(), after.Size(), reclaimed)
	} else {
		_, _ = fmt.Fprintln(w, "compacted.")
	}
	return nil
}
