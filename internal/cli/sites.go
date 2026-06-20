package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// runSitesAdd forwards a sites-add to the daemon control API and returns the new id.
func runSitesAdd(ctx context.Context, client *control.Client, url, name, minInterval, maxInterval string, speed int) (int64, error) {
	resp, err := client.AddSite(ctx, control.AddSiteRequest{
		URL:         url,
		Name:        name,
		MinInterval: minInterval,
		MaxInterval: maxInterval,
		Speed:       speed,
	})
	if err != nil {
		return 0, err
	}
	return resp.SiteID, nil
}

// runSitesRemove forwards a sites-remove to the daemon control API.
func runSitesRemove(ctx context.Context, client *control.Client, id string, purge bool) error {
	return client.RemoveSite(ctx, id, purge)
}

func newSitesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sites", Short: "Manage monitored sites"}
	cmd.AddCommand(newSitesAddCmd(), newSitesListCmd(), newSitesShowCmd(), newSitesRemoveCmd())
	return cmd
}

func newSitesAddCmd() *cobra.Command {
	var name, minInterval, maxInterval string
	var speed int
	add := &cobra.Command{
		Use:   "add <url>",
		Short: "Add a site to monitor (writes config.yaml + reloads via the daemon)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				id, err := runSitesAdd(ctx, client, args[0], name, minInterval, maxInterval, speed)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "added site %d: %s\n", id, args[0])
				return err
			})
		},
	}
	add.Flags().StringVar(&name, "name", "", "human-readable site name")
	add.Flags().StringVar(&minInterval, "min-interval", "", "minimum recheck interval (e.g. 10m)")
	add.Flags().StringVar(&maxInterval, "max-interval", "", "maximum recheck interval (e.g. 24h)")
	add.Flags().IntVar(&speed, "speed", 0, "crawl speed scale percent")
	return add
}

func newSitesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List monitored sites (read-only)",
		RunE: func(c *cobra.Command, args []string) error {
			return withStore(c, func(ctx context.Context, db *store.DB) error {
				sites, err := db.ListSites(ctx)
				if err != nil {
					return err
				}
				for _, s := range sites {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "%d\t%s\t%s\tenabled=%v\n", s.ID, s.Name, s.BaseURL, s.Enabled); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newSitesShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <url|id>",
		Short: "Show a site (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := loadConfig(c)
			if err != nil {
				return err
			}
			db, err := store.Open(c.Context(), databasePath(cfg))
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			site, err := db.GetSiteByBaseURL(c.Context(), args[0])
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(c.OutOrStdout(), "id=%d name=%s base_url=%s enabled=%v min=%ds max=%ds\n",
				site.ID, site.Name, site.BaseURL, site.Enabled, site.MinInterval, site.MaxInterval); err != nil {
				return err
			}
			monitored, cerr := db.CountSiteURLs(c.Context(), site.ID)
			if cerr != nil {
				monitored = 0
			}
			pageCap := siteConfigCap(cfg, site.BaseURL)
			_, err = fmt.Fprintln(c.OutOrStdout(), sitePagesLine(monitored, pageCap))
			return err
		},
	}
}

func newSitesRemoveCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove or disable a site (purge deletes history per §3.4 S1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withControlClient(c, func(ctx context.Context, client *control.Client) error {
				if err := runSitesRemove(ctx, client, args[0], purge); err != nil {
					return err
				}
				_, err := fmt.Fprintf(c.OutOrStdout(), "removed site %s (purge=%v)\n", args[0], purge)
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete history as well")
	return cmd
}
