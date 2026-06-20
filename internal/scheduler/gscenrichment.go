package scheduler

import (
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// GSC W2 signal 3: search_performance_shift. The pure enrichment computation lives
// in internal/store (store.SearchPerformanceShift / store.SearchShift) so the READ
// layer — store.BuildReport and the MCP summarize_changes handler — can call it
// without an import cycle (internal/store cannot import internal/scheduler, which
// already imports internal/store). This file keeps a thin forwarding seam so the
// scheduler's own synthetic-behaviour tests keep driving the same correlation by its
// package-local name, and so the "signal 3 is an enrichment, never a standalone
// Ingest" invariant stays asserted next to signals 1 & 2.
//
// The enrichment is NOT a standalone alert and NOT an Ingest into the pipeline: it is
// computed at the report/MCP read layer, where change history is reviewed after the
// fact, because the correlation needs enough FINALIZED post-change search data, which
// by the dataState=final lag does not exist at the instant a change alert fires. A
// standalone raw-traffic / impression / ranking-drop threshold is a HARD non-goal
// (seasonality, SERP volatility, data lag = noise).

// SearchShift is the additive search_performance_shift enrichment. It is an alias of
// store.SearchShift (the canonical type now lives in the store, near the
// search_metrics keyspace it reads): the scheduler refers to it by this local name.
type SearchShift = store.SearchShift

// SearchPerformanceShift forwards to store.SearchPerformanceShift (the canonical
// implementation). It correlates a single change at changeDate against the page's
// per-(query,date) search metrics and returns an enrichment ONLY when there is enough
// finalized post-change data to be meaningful; see the store implementation for the
// full gating contract.
func SearchPerformanceShift(metrics []model.SearchMetric, changeDate string, now time.Time) (SearchShift, bool) {
	return store.SearchPerformanceShift(metrics, changeDate, now)
}
