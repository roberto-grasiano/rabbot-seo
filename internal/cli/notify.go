package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func newNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "notify", Short: "Notifier operations"}
	test := &cobra.Command{
		Use:   "test <notifier>",
		Short: "Send a sample alert through a named notifier",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withControlClient(cmd, func(ctx context.Context, client *control.Client) error {
				if err := client.NotifyTest(ctx, args[0]); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "sample alert sent via %q\n", args[0])
				return err
			})
		},
	}
	cmd.AddCommand(test)
	return cmd
}
