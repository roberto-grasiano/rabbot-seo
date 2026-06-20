package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// blockingDueStore delivers a fixed batch of URLs exactly once, then nil.
type blockingDueStore struct {
	mu     sync.Mutex
	due    []model.URL
	popped bool
}

func (b *blockingDueStore) PopDueURLs(ctx context.Context, now time.Time, batch int) ([]model.URL, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.popped {
		return nil, nil
	}
	b.popped = true
	return b.due, nil
}

// TestQueueDepthReflectsInFlight verifies QueueDepth counts dispatched crawls
// that are currently executing and returns to zero once Tick completes.
func TestQueueDepthReflectsInFlight(t *testing.T) {
	ds := &blockingDueStore{due: []model.URL{
		{ID: 1, URL: "https://e.com/a", Interval: 600},
		{ID: 2, URL: "https://e.com/b", Interval: 600},
	}}

	entered := make(chan struct{}) // signalled once per crawl when it starts
	release := make(chan struct{}) // closed to let crawls finish

	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MaxParallel: 8, // both URLs dispatch concurrently
		MinInterval: 600,
		MaxInterval: 86400,
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			entered <- struct{}{}
			<-release
			return CrawlResult{URLID: u.ID, FetchClass: model.FetchOK}
		},
		Now: time.Now,
	}

	tickDone := make(chan error, 1)
	go func() {
		tickDone <- s.Tick(context.Background())
	}()

	// Wait until both crawls have entered CrawlFunc (and thus been counted).
	<-entered
	<-entered

	if got := s.QueueDepth(); got != 2 {
		t.Errorf("QueueDepth() = %d while 2 crawls in flight, want 2", got)
	}

	// Unblock both crawls and let Tick return.
	close(release)

	if err := <-tickDone; err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if got := s.QueueDepth(); got != 0 {
		t.Errorf("QueueDepth() = %d after Tick returned, want 0", got)
	}
}
