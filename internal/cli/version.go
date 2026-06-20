package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(bi BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rabbot %s (commit %s, built %s)\n",
				bi.Version, bi.Commit, bi.Date)
			return nil
		},
	}
}
