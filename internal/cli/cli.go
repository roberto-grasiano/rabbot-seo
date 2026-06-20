// Package cli implements the cobra command tree. Read-only commands query the
// store directly; mutating commands talk to the daemon via the control client.
package cli

import (
	"github.com/spf13/cobra"
)

// BuildInfo carries ldflags-injected version metadata.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// NewRootCmd builds the full rabbot command tree.
func NewRootCmd(bi BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:           "rabbot",
		Short:         "Real-time SEO monitoring gateway",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newVersionCmd(bi),
		newConfigCmd(),
		newInitCmd(bi),
		newServiceCmd(bi),
		newRunCmd(bi),
		newSitesCmd(),
		newCrawlCmd(),
		newHistoryCmd(),
		newReportCmd(),
		newSitemapCmd(),
		newInspectCmd(),
		newDoctorCmd(bi),
		newVerifyCmd(),
		newMCPCmd(bi),
		newPauseCmd(),
		newResumeCmd(),
		newStatusCmd(),
		newStopCmd(),
		newDBCmd(),
		newIssuesCmd(),
		newIssueCmd(),
		newSegmentsCmd(),
		newNotifyCmd(),
		newObservabilityCmd(),
		newLinksCmd(),
		newGraphCmd(),
		newGSCCmd(),
	)
	return root
}

// Execute builds and runs the root command, returning its error.
func Execute(bi BuildInfo) error {
	return NewRootCmd(bi).Execute()
}
