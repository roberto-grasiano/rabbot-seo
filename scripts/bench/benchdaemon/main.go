//go:build benchdaemon

// Command benchdaemon is the BENCH-ONLY daemon launcher for
// scripts/bench/capacity.sh. It is built ONLY with `-tags benchdaemon` and is
// never shipped (goreleaser builds only ./cmd/rabbot). It runs the REAL rabbot
// daemon with the crawl SSRF guard relaxed so the capacity harness can dial its
// loopback corpus — see internal/cli/run_benchdaemon.go and docs/PERFORMANCE.md.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/roberto-grasiano/rabbot-seo/internal/cli"
)

func main() {
	// If the capacity harness asked for a pidfile, record THIS process's real PID
	// before the (blocking) daemon run. systemd-run --scope may either exec-replace
	// the command (then the harness's $! is us) or fork+monitor it (then $! is the
	// waiter and we are its child) — behavior varies by systemd version. Writing our
	// own getpid() here lets the harness sample the right /proc/<pid> regardless.
	// Best-effort: a write failure must not abort the bench run. 0o600 keeps gosec happy.
	if pidPath := os.Getenv("RABBOT_BENCH_PIDFILE"); pidPath != "" {
		// G703 path-traversal is N/A: this is build-tagged dev tooling (never shipped),
		// and the path is supplied by the bench harness operator, not an attacker.
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600) //nolint:gosec // operator-supplied pidfile path, dev-only
	}
	// Any args (e.g. a "run" subcommand, for parity with how capacity.sh invokes
	// the stock binary) are ignored: this binary's sole job is the daemon loop.
	// Config + data dir resolve from XDG_CONFIG_HOME exactly as `rabbot run` does.
	if err := cli.RunDaemonBenchLoopback(cli.BuildInfo{Version: "bench"}); err != nil {
		fmt.Fprintln(os.Stderr, "benchdaemon:", err)
		os.Exit(1)
	}
}
