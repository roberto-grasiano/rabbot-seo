package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func runCrawl(ctx context.Context, client *control.Client, target string) (int, error) {
	resp, err := client.Crawl(ctx, control.CrawlRequest{Target: target})
	if err != nil {
		return 0, err
	}
	return resp.Queued, nil
}

func runPause(ctx context.Context, client *control.Client) error  { return client.Pause(ctx) }
func runResume(ctx context.Context, client *control.Client) error { return client.Resume(ctx) }

func newCrawlCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "crawl <url>",
		Short: "Force an immediate recheck of a URL or site",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				queued, err := runCrawl(ctx, client, args[0])
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "queued %d URL(s) for immediate recheck\n", queued)
				return err
			})
		},
	}
}

func newPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause all crawling (global kill-switch)",
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				if err := runPause(ctx, client); err != nil {
					return err
				}
				_, err := fmt.Fprintln(c.OutOrStdout(), "crawling paused")
				return err
			})
		},
	}
}

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume crawling",
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				if err := runResume(ctx, client); err != nil {
					return err
				}
				_, err := fmt.Fprintln(c.OutOrStdout(), "crawling resumed")
				return err
			})
		},
	}
}

func newHistoryCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "history <url>",
		Short: "Show the change log for a URL (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				sinceT := time.Time{}
				if since != "" {
					d, derr := time.ParseDuration(since)
					if derr != nil {
						return fmt.Errorf("invalid --since: %w", derr)
					}
					// UTC to match the UTC-serialized detected_at timestamps the store
					// compares against; a host-local now() would skew the window by the
					// UTC offset on any non-UTC host (F1).
					sinceT = time.Now().UTC().Add(-d)
				}
				u, err := db.GetURL(ctx, 0, args[0])
				if err != nil {
					return err
				}
				changes, err := db.GetURLHistory(ctx, u.ID, sinceT)
				if err != nil {
					return err
				}
				for _, ch := range changes {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%q -> %q\t[%s]\n",
						ch.DetectedAt.Format(time.RFC3339), ch.Field, ch.OldValue, ch.NewValue, ch.ChangeClass); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only show changes within this duration (e.g. 24h)")
	return cmd
}
