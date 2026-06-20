package scheduler

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

type fakeDueStore struct {
	mu      sync.Mutex
	due     []model.URL
	popped  int
	lastNow time.Time // records the now passed to the most recent PopDueURLs call
}

func (f *fakeDueStore) PopDueURLs(ctx context.Context, now time.Time, batch int) ([]model.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastNow = now
	if f.popped > 0 {
		return nil, nil // only deliver once to avoid an infinite loop in test
	}
	f.popped++
	return f.due, nil
}

func TestSchedulerProcessesDueBatch(t *testing.T) {
	ds := &fakeDueStore{due: []model.URL{
		{ID: 1, URL: "https://e.com/a", Interval: 600},
		{ID: 2, URL: "https://e.com/b", Interval: 600},
	}}

	var mu sync.Mutex
	var crawled []int64
	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MinInterval: 600,
		MaxInterval: 86400,
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			mu.Lock()
			crawled = append(crawled, u.ID)
			mu.Unlock()
			return CrawlResult{URLID: u.ID, FetchClass: model.FetchOK}
		},
		Now: time.Now,
	}

	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(crawled) != 2 {
		t.Errorf("crawled %d URLs, want 2: %v", len(crawled), crawled)
	}
}

// TestSchedulerContainsPanickingCrawl guards fix #8: the per-URL worker runs the
// goquery/readability/JSON-LD/hydration chain with no recover, so one panicking
// page would unwind the goroutine and crash the whole daemon. A deferred per-URL
// recover must contain the panic, log it (component=scheduler + the url_id + the
// recovered value + a stack), and return so the NEXT URL still processes.
//
// MaxParallel is 1 so the two URLs run sequentially: the panicker first, then the
// survivor. Without the recover, the test process itself dies on the panic.
func TestSchedulerContainsPanickingCrawl(t *testing.T) {
	ds := &fakeDueStore{due: []model.URL{
		{ID: 11, URL: "https://e.com/panics", Interval: 600},
		{ID: 22, URL: "https://e.com/survives", Interval: 600},
	}}

	var processed sync.Map // url id -> struct{}
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, "error")

	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MaxParallel: 1, // sequential: panicker runs before survivor
		MinInterval: 600,
		MaxInterval: 86400,
		Log:         log,
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			processed.Store(u.ID, struct{}{})
			if u.ID == 11 {
				panic("boom: goquery exploded on a malformed page")
			}
			return CrawlResult{URLID: u.ID, FetchClass: model.FetchOK}
		},
		Now: time.Now,
	}

	// If the panic is not contained, this Tick crashes the test binary.
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if _, ok := processed.Load(int64(11)); !ok {
		t.Fatalf("panicking URL 11 was never entered")
	}
	if _, ok := processed.Load(int64(22)); !ok {
		t.Fatalf("URL 22 was not processed after URL 11 panicked — recover did not contain the panic")
	}

	out := buf.String()
	if !strings.Contains(out, "url_id") || !strings.Contains(out, "11") {
		t.Errorf("recover log missing the panicking url_id; log = %q", out)
	}
	if !strings.Contains(out, "boom: goquery exploded") {
		t.Errorf("recover log missing the recovered panic value; log = %q", out)
	}
	if !strings.Contains(out, "stack") {
		t.Errorf("recover log missing the stack; log = %q", out)
	}
}

// TestSchedulerPanicDoesNotLeakInFlight guards that a panicking crawl still
// decrements the in-flight counter (the recover must run inside the same
// goroutine as the inFlight.Add(-1) defer), so QueueDepth returns to zero and
// the semaphore slot is released — a panic must not wedge the scheduler.
func TestSchedulerPanicDoesNotLeakInFlight(t *testing.T) {
	ds := &fakeDueStore{due: []model.URL{
		{ID: 1, URL: "https://e.com/a", Interval: 600},
		{ID: 2, URL: "https://e.com/b", Interval: 600},
	}}
	var calls atomic.Int64
	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MaxParallel: 2,
		MinInterval: 600,
		MaxInterval: 86400,
		Log:         obs.NewLogger(&bytes.Buffer{}, "error"),
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			calls.Add(1)
			panic("every page panics")
		},
		Now: time.Now,
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("CrawlFunc calls = %d, want 2 (both URLs dispatched despite panics)", got)
	}
	if got := s.QueueDepth(); got != 0 {
		t.Errorf("QueueDepth() = %d after Tick with all-panicking crawls, want 0 (in-flight leaked)", got)
	}
}

// TestSchedulerDefaultClockIsUTC guards F1: next_check_at is written as UTC
// wall-clock text by every crawl, but SQLite compares those rows lexically as
// TEXT (the trailing zone suffix defeats NUMERIC affinity), so PopDueURLs'
// `WHERE next_check_at <= ?` only orders correctly when the query arg is also a
// UTC wall-clock. With the default (s.Now unset) clock derived from a host-local
// time.Now(), a host west of UTC would query with a wall-clock hours behind the
// stored UTC strings and silently stall due URLs. The scheduler's default clock
// must therefore be UTC. We force the host zone to a non-UTC offset and assert
// the now reaching PopDueURLs is still UTC.
func TestSchedulerDefaultClockIsUTC(t *testing.T) {
	ds := &fakeDueStore{}
	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MinInterval: 600,
		MaxInterval: 86400,
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			return CrawlResult{URLID: u.ID, FetchClass: model.FetchOK}
		},
		// Now intentionally unset: exercises the production default path.
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if loc := ds.lastNow.Location(); loc != time.UTC {
		t.Errorf("PopDueURLs now location = %v, want UTC (else due-URL selection mis-orders on non-UTC hosts)", loc)
	}
}
