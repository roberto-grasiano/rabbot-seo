package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// graphSweepChunk is the chunk size for the BFS depth write-back (urls.graph_depth)
// — the DeleteStaleSnapshots chunked-write precedent. It keeps each write
// transaction short on the single writer; not a config knob.
const graphSweepChunk = 5000

// registerGraphSweep adds the periodic A9 click-depth BFS sweep to s when graph is
// enabled (g non-nil), returning whether it registered. For each enabled site the
// sweep runs a depth-capped recursive-CTE BFS from the site root, writes
// urls.graph_depth (chunked), derives click_depth_regression from the depth
// transitions, and authoritatively reconciles orphans. The sweep can be slow on a
// large site, so it uses SingletonMode(LimitModeReschedule): a run still in flight
// when the next tick fires SKIPS the overlapping invocation (memory lead 7769 — the
// existing sweeps lacked this; the graph BFS is the one most likely to overrun its
// interval). WithStartImmediately mirrors the retention sweep so a freshly (re)
// started daemon populates depths without waiting a full interval. The task runs
// under ctx so daemon shutdown cancels it mid-flight.
func registerGraphSweep(ctx context.Context, logger *slog.Logger, s gocron.Scheduler, db *store.DB, g *linkgraph.Grapher, interval time.Duration) (bool, error) {
	if g == nil {
		return false, nil
	}
	_, err := s.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(func() {
			sites, lerr := db.ListSites(ctx)
			if lerr != nil {
				if ctx.Err() == nil {
					logger.Error("graph sweep: list sites failed", obs.KeyComponent, "linkgraph", obs.KeyError, lerr.Error())
				}
				return
			}
			for _, site := range sites {
				if !site.Enabled {
					continue
				}
				if serr := g.Sweep(ctx, site.ID, graphSweepChunk); serr != nil && ctx.Err() == nil {
					logger.Error("graph sweep failed", obs.KeyComponent, "linkgraph",
						"site", site.BaseURL, obs.KeyError, serr.Error())
				}
			}
		}),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	)
	if err != nil {
		return false, err
	}
	return true, nil
}
