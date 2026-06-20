//go:build benchdaemon

// This file is compiled ONLY under `-tags benchdaemon`, used solely by the
// offline capacity harness (scripts/bench/capacity.sh) to drive the REAL daemon
// against a loopback corpus. It is excluded from every normal and shipped build:
// `go build ./cmd/rabbot`, `make build`, `make test`, `go vet ./...`, and
// goreleaser all run WITHOUT this tag, so RunDaemonBenchLoopback — and the
// AllowPrivate:true crawl path it enables — is PHYSICALLY ABSENT from the release
// binary (not dead code; not present at all).
//
// Why it must exist: the shipped binary deliberately exposes no flag or env to
// relax the crawl SSRF guard (internal/fetcher/ssrf.go), so a stock `rabbot run`
// cannot dial the harness's 127.0.0.1 corpus — every fetch would classify as
// unreachable and the capacity numbers would be meaningless. This tagged build
// flips the same AllowPrivate switch the in-package end-to-end daemon tests use
// (see daemon_e2e_test.go), and nothing else. docs/PERFORMANCE.md documents the
// harness.

package cli

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// RunDaemonBenchLoopback runs the daemon exactly as `rabbot run`'s RunE does —
// identical config-dir / data-dir / control-token resolution from the resolved
// XDG config dir — differing ONLY in AllowPrivate:true, which relaxes the crawl
// fetchers' SSRF guard so the daemon may dial the harness's loopback corpus.
// Bench-only (see the file header); unreachable from the shipped binary.
func RunDaemonBenchLoopback(bi BuildInfo) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfgDir, err := config.ResolveConfigDir()
	if err != nil {
		return err
	}
	cfgPath := config.ConfigFilePath(cfgDir)
	c, err := config.Load(cfgPath, nil)
	if err != nil {
		return err
	}
	dataDir, err := config.ResolveDataDir(c.DataDir)
	if err != nil {
		return err
	}
	token, err := control.LoadOrCreateToken(filepath.Join(cfgDir, "control.token"))
	if err != nil {
		return err
	}
	return runDaemon(ctx, os.Stdout, daemonOptions{
		ConfigPath:          cfgPath,
		DataDir:             dataDir,
		ControlToken:        token,
		ControlPort:         c.Control.Port,
		Version:             bi.Version,
		LogLevel:            c.Log.Level,
		TickInterval:        time.Second,
		EgressCheckEndpoint: c.Crawler.EgressCheckEndpoint,
		EgressCheckEnabled:  c.Crawler.EgressCheckEnabled,
		MetricsAddr:         c.Metrics.Addr,
		AllowPrivate:        true, // BENCH-ONLY: admit the loopback corpus (see file header)
	})
}
