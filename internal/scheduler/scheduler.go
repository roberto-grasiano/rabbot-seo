package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// DueStore is the subset of store.Store the scheduler uses for the due-queue.
type DueStore interface {
	PopDueURLs(ctx context.Context, now time.Time, batch int) ([]model.URL, error)
}

// CrawlFn crawls one URL. The daemon supplies Crawler.CrawlOne.
type CrawlFn func(ctx context.Context, u model.URL, minInterval, maxInterval int64, contentSelector string) CrawlResult

// Scheduler pops due URLs (importance DESC, next_check_at ASC) and dispatches
// them through CrawlFunc. Per-URL politeness is enforced inside CrawlFunc by the
// frontier, so this loop may dispatch concurrently.
type Scheduler struct {
	DueStore    DueStore
	CrawlFunc   CrawlFn
	Batch       int
	MinInterval int64
	MaxInterval int64
	MaxParallel int
	SelectorFor func(u model.URL) string
	Now         func() time.Time

	// Log, when set, records per-crawl errors (fetch + M2 diff/rules/alert
	// failures surfaced on CrawlResult.Err). Nil disables logging (tests).
	Log *slog.Logger

	// inFlight tracks crawls currently executing. atomic.Int64 makes Scheduler
	// non-copyable; it is always used as *Scheduler, which is fine.
	inFlight atomic.Int64
}

// Tick processes one due batch.
func (s *Scheduler) Tick(ctx context.Context) error {
	batch := s.Batch
	if batch <= 0 {
		batch = 50
	}
	// next_check_at is serialized as UTC wall-clock text and compared lexically
	// as TEXT by SQLite (PopDueURLs' WHERE next_check_at <= ?), so the query arg
	// must also be a UTC wall-clock or due URLs mis-order on non-UTC hosts. Derive
	// the default clock in UTC; an injected s.Now is expected to already be UTC.
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	due, err := s.DueStore.PopDueURLs(ctx, now, batch)
	if err != nil {
		return err
	}

	maxPar := s.MaxParallel
	if maxPar <= 0 {
		maxPar = 8
	}
	sem := make(chan struct{}, maxPar)
	var wg sync.WaitGroup
	for _, u := range due {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(u model.URL) {
			defer wg.Done()
			defer func() { <-sem }()
			s.inFlight.Add(1)
			defer s.inFlight.Add(-1)
			// CrawlFunc runs the goquery/readability/JSON-LD/hydration chain, none of
			// which recovers internally. A panic on one malformed page would unwind
			// this goroutine and, with no recover, crash the whole daemon — taking
			// down monitoring for every OTHER site. Contain it per-URL: recover, log
			// (url_id + the recovered value + a stack), and return so the next URL in
			// this batch still processes. This defer is innermost-but-one (the sem/
			// inFlight releases above it still run on the unwind), so a panic never
			// leaks an in-flight count or a semaphore slot.
			defer func() {
				if r := recover(); r != nil && s.Log != nil {
					s.Log.Error("crawl panicked", obs.KeyComponent, "scheduler",
						obs.KeyURLID, u.ID, obs.KeyError, fmt.Sprintf("%v", r),
						"stack", string(debug.Stack()))
				}
			}()
			sel := ""
			if s.SelectorFor != nil {
				sel = s.SelectorFor(u)
			}
			res := s.CrawlFunc(ctx, u, s.MinInterval, s.MaxInterval, sel)
			// Surface per-crawl failures (fetch + M2 diff/rules/alert errors). The
			// crawl already advanced the schedule, so this is observability only,
			// not control flow. Suppressed during shutdown (ctx cancelled).
			if res.Err != nil && s.Log != nil && ctx.Err() == nil {
				s.Log.Error("crawl failed", obs.KeyComponent, "scheduler",
					obs.KeyURLID, res.URLID, obs.KeyError, res.Err.Error())
			}
		}(u)
	}
	wg.Wait()
	return nil
}

// QueueDepth reports the number of crawls currently in flight (dispatched and
// executing inside CrawlFunc). It is safe to call concurrently with Tick.
func (s *Scheduler) QueueDepth() int { return int(s.inFlight.Load()) }

// Run drives Tick on an interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				// Non-fatal: a tick error (e.g. PopDueURLs) must not stop the loop,
				// but it must not be swallowed either. Log it and keep looping. When
				// s.Log is nil the error is intentionally dropped — only tests run
				// without a logger; the daemon (runDaemon) always sets s.Log.
				if s.Log != nil {
					s.Log.Error("scheduler tick failed", obs.KeyComponent, "scheduler",
						obs.KeyError, err.Error())
				}
			}
		}
	}
}
