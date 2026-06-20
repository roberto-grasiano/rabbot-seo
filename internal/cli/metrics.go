package cli

import (
	"context"
	"net"
	"os"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// isLoopbackAddr reports whether a host:port metrics address binds only to the
// loopback interface. An unparseable addr, an empty host (binds all interfaces),
// or a non-loopback IP is treated as non-loopback so the daemon errs toward
// WARNING about exposure rather than staying silent. A hostname that is not an IP
// literal (e.g. "localhost") is conservatively treated as non-loopback for the
// warning since it could resolve anywhere — the listener still binds; this only
// governs the advisory log line.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// metricsSampleInterval is the cadence at which the daemon refreshes the
// DB-backed metrics gauges (rabbot_due_urls, rabbot_db_size_bytes). These read
// the database, so they are sampled OFF the scrape path on a slow timer — the
// scrape handler (promhttp over the registry) never touches the DB. ~30s keeps
// the gauges fresh enough for a dashboard without adding meaningful load.
const metricsSampleInterval = 30 * time.Second

// dueCounter is the single store method the sampler needs (the same call the
// Status hook uses). Narrowing to an interface keeps the sampler unit-testable
// and documents that the sampler is read-only against the store.
type dueCounter interface {
	CountDueURLs(ctx context.Context, now time.Time) (int, error)
}

// dbSizeBytes returns the on-disk size of the SQLite database: the main file
// plus its WAL sidecar (the daemon runs in WAL mode, where recent writes live in
// -wal until a checkpoint). A missing file contributes 0 (the -wal may not exist
// between checkpoints, and the main file may be momentarily absent during a
// VACUUM); the gauge therefore tracks the live footprint without erroring.
func dbSizeBytes(dbPath string) int64 {
	var total int64
	for _, p := range []string{dbPath, dbPath + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// sampleMetricsOnce refreshes the DB-backed gauges a single time. It reads the
// recheck backlog (CountDueURLs — the same call the Status hook makes) and the
// on-disk DB size. Errors are swallowed: a transient query failure must not stop
// the sampler, and the gauges simply keep their prior value until the next tick.
// A nil *Metrics no-ops on every Set, so this is safe when metrics are off.
func sampleMetricsOnce(ctx context.Context, m *obs.Metrics, due dueCounter, dbPath string, now time.Time) {
	if n, err := due.CountDueURLs(ctx, now); err == nil {
		m.SetDueURLs(n)
	}
	m.SetDBSizeBytes(dbSizeBytes(dbPath))
}

// runMetricsSampler refreshes the DB-backed gauges on every tick from tickCh
// until ctx is cancelled. It is the loop body the daemon runs in a
// pipelineWG-joined goroutine; the ticker is injected so tests can drive it
// deterministically. It does ONE refresh immediately so the gauges are populated
// before the first scrape, then refreshes per tick.
func runMetricsSampler(ctx context.Context, m *obs.Metrics, due dueCounter, dbPath string, tickCh <-chan time.Time) {
	sampleMetricsOnce(ctx, m, due, dbPath, time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case <-tickCh:
			sampleMetricsOnce(ctx, m, due, dbPath, time.Now().UTC())
		}
	}
}

// ensure *store.DB satisfies the narrow dueCounter the sampler depends on.
var _ dueCounter = (*store.DB)(nil)
