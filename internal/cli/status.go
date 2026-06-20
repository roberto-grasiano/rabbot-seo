package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func printStatus(w io.Writer, s control.StatusResponse) error {
	state := "RUNNING"
	if s.Paused {
		state = "PAUSED"
	}
	lines := []string{
		fmt.Sprintf("State:        %s\n", state),
		fmt.Sprintf("Version:      %s\n", s.Version),
		fmt.Sprintf("Uptime:       %s\n", s.Uptime),
		fmt.Sprintf("Sites:        %d\n", s.SiteCount),
		fmt.Sprintf("URLs:         %d\n", s.URLCount),
		fmt.Sprintf("Queue:        due=%d queue=%d\n", s.DueCount, s.QueueDepth),
		fmt.Sprintf("Last crawl:   %s\n", s.LastCrawlAt),
	}
	if s.CappedSites > 0 {
		lines = append(lines, fmt.Sprintf(
			"Capped sites: %d (raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>'; 0 = all)\n",
			s.CappedSites))
	}
	if len(s.EgressIP) > 0 {
		lines = append(lines, fmt.Sprintf("Egress IP:    %s\n", strings.Join(s.EgressIP, ", ")))
	}
	if s.MetricsAddr != "" {
		lines = append(lines, fmt.Sprintf("Metrics:      http://%s/metrics\n", s.MetricsAddr))
	}
	for _, line := range lines {
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}

// statusFetchFn is the seam through which `rabbot status` fetches the daemon
// status. Production points at (*control.Client).Status; tests replace it to drive
// the startup-race retry (a few ErrDaemonNotRunning then success) without a live
// daemon. It is scoped to the status command so the shared withControlClient
// preamble (used by every read command) keeps its plain one-shot semantics.
var statusFetchFn = func(ctx context.Context, client *control.Client) (control.StatusResponse, error) {
	return client.Status(ctx)
}

// statusStartupBudget and statusRetryInterval bound the startup-race retry (#87):
// right after `service start`, the daemon takes ~5-8s to bind the control port, so
// a `rabbot status` issued immediately would otherwise print "daemon not running"
// for a connection that is merely STARTING. We poll Status until the daemon answers
// or the budget elapses. They are package vars so tests can shrink them (poll a
// CONDITION, never a fixed wall sleep). 12s comfortably covers the observed window.
var (
	statusStartupBudget  = 12 * time.Second
	statusRetryInterval  = 300 * time.Millisecond
	statusStartingNotice = "waiting for daemon to start…"
)

// fetchStatusWithStartupRetry calls statusFetchFn, retrying ONLY while the daemon
// is not yet reachable (control.ErrDaemonNotRunning — connection refused during the
// bind window), up to statusStartupBudget. Any other error (e.g. an auth failure)
// returns immediately. On the first not-running result it prints a one-line
// "starting" notice to errOut so the operator can tell "starting" from "down". When
// the budget is exhausted it returns the last ErrDaemonNotRunning so the caller
// surfaces the genuine not-running message. It honors ctx for prompt cancellation
// and polls a condition (the fetch result) rather than sleeping a fixed wall.
func fetchStatusWithStartupRetry(ctx context.Context, client *control.Client, errOut io.Writer) (control.StatusResponse, error) {
	deadline := time.Now().Add(statusStartupBudget)
	noticed := false
	for {
		resp, err := statusFetchFn(ctx, client)
		if err == nil {
			return resp, nil
		}
		// Only the not-running (connection-refused) case is a startup race worth
		// retrying; anything else (auth, malformed response) is a real failure.
		if !errors.Is(err, control.ErrDaemonNotRunning) {
			return control.StatusResponse{}, err
		}
		if time.Now().After(deadline) {
			return control.StatusResponse{}, err
		}
		if !noticed {
			noticed = true
			_, _ = io.WriteString(errOut, statusStartingNotice+"\n")
		}
		// Sleep until the next poll, but wake promptly on cancellation. Polling a
		// timer (a condition re-check on each tick), never a single blind long sleep.
		t := time.NewTimer(statusRetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return control.StatusResponse{}, ctx.Err()
		case <-t.C:
		}
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status (GET /v1/status)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withControlClient(cmd, func(ctx context.Context, client *control.Client) error {
				resp, err := fetchStatusWithStartupRetry(ctx, client, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				return printStatus(cmd.OutOrStdout(), resp)
			})
		},
	}
}
