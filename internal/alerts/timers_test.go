package alerts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

type listableIncidentStore struct {
	*fakeIncidentStore
	openList []model.Alert
}

func (l *listableIncidentStore) ListOpenIncidents(ctx context.Context) ([]model.Alert, error) {
	return l.openList, nil
}

func TestAutoCloseSweep(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := base.Add(25 * time.Hour) // 25h later
	fake := newFakeIncidentStore()
	stale := model.Alert{ID: 7, Fingerprint: "fp", Status: model.AlertOpen, LastUpdatedAt: base}
	fake.openByFP["fp"] = stale
	ls := &listableIncidentStore{fakeIncidentStore: fake, openList: []model.Alert{stale}}

	p := newPipeline(ls, &capturingDispatcher{},
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
	)

	if err := p.AutoCloseStale(context.Background(), ls); err != nil {
		t.Fatalf("AutoCloseStale: %v", err)
	}
	if len(fake.closed) != 1 || fake.closed[0] != 7 {
		t.Errorf("stale incident (>24h) should auto-close, closed=%v", fake.closed)
	}
}

func TestAutoCloseSkipsRecent(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := base.Add(1 * time.Hour)
	fake := newFakeIncidentStore()
	recent := model.Alert{ID: 8, Fingerprint: "fp2", Status: model.AlertOpen, LastUpdatedAt: base}
	ls := &listableIncidentStore{fakeIncidentStore: fake, openList: []model.Alert{recent}}
	p := newPipeline(ls, &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
	)
	if err := p.AutoCloseStale(context.Background(), ls); err != nil {
		t.Fatalf("AutoCloseStale: %v", err)
	}
	if len(fake.closed) != 0 {
		t.Errorf("recent incident must not auto-close, closed=%v", fake.closed)
	}
}

// ctxKey is a private context key so the auto-close task can prove which ctx it
// received (the one passed to RegisterTimers, not context.Background()).
type ctxKey struct{}

// signalingLister records the context it was invoked with and signals a channel,
// optionally returning an error to exercise the error-logging branch.
type signalingLister struct {
	gotCtx chan context.Context
	err    error
}

func (s *signalingLister) ListOpenIncidents(ctx context.Context) ([]model.Alert, error) {
	s.gotCtx <- ctx
	return nil, s.err
}

func newTestScheduler(t *testing.T) gocron.Scheduler {
	t.Helper()
	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	return s
}

// TestRegisterTimersDigestJobOptional asserts the digest job is registered only
// when a non-nil digest func is supplied.
func TestRegisterTimersDigestJobOptional(t *testing.T) {
	p := newPipeline(newFakeIncidentStore(), &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}))
	ls := &signalingLister{gotCtx: make(chan context.Context, 1)}

	sNil := newTestScheduler(t)
	if err := p.RegisterTimers(context.Background(), nil, sNil, ls, time.Hour, nil); err != nil {
		t.Fatalf("RegisterTimers(nil digest): %v", err)
	}
	if got := len(sNil.Jobs()); got != 1 {
		t.Fatalf("nil digest: want 1 job (auto-close only), got %d", got)
	}

	sWith := newTestScheduler(t)
	if err := p.RegisterTimers(context.Background(), nil, sWith, ls, time.Hour, func() {}); err != nil {
		t.Fatalf("RegisterTimers(with digest): %v", err)
	}
	if got := len(sWith.Jobs()); got != 2 {
		t.Fatalf("with digest: want 2 jobs (auto-close + digest), got %d", got)
	}
}

// TestRegisterTimersAutoCloseUsesPassedCtx drives the scheduler and asserts the
// auto-close task ran with the context passed to RegisterTimers (not Background).
func TestRegisterTimersAutoCloseUsesPassedCtx(t *testing.T) {
	p := newPipeline(newFakeIncidentStore(), &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}))
	ls := &signalingLister{gotCtx: make(chan context.Context, 1)}

	s := newTestScheduler(t)
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	if err := p.RegisterTimers(ctx, nil, s, ls, time.Hour, nil); err != nil {
		t.Fatalf("RegisterTimers: %v", err)
	}
	jobs := s.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	s.Start()
	if err := jobs[0].RunNow(); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	select {
	case got := <-ls.gotCtx:
		if got.Value(ctxKey{}) != "marker" {
			t.Errorf("auto-close task used a context without the marker; want the ctx passed to RegisterTimers")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("auto-close task did not run within 3s")
	}
}

// TestRegisterTimersRunsAtBoot reproduces F57: a DurationJob with no start
// option defers its first run to now()+interval, leaving a cold window after every
// restart where no stale-incident sweep runs and the digest buffer is not drained.
// Both jobs must run at boot (WithStartImmediately): right after Start(), and
// WITHOUT anyone calling RunNow(), the auto-close sweep invokes the lister and the
// digest func fires. With the deferred first-run bug, neither runs for an interval.
func TestRegisterTimersRunsAtBoot(t *testing.T) {
	p := newPipeline(newFakeIncidentStore(), &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}))
	ls := &signalingLister{gotCtx: make(chan context.Context, 16)}
	digestFired := make(chan struct{}, 16)

	s := newTestScheduler(t)
	if err := p.RegisterTimers(context.Background(), nil, s, ls, time.Hour, func() {
		digestFired <- struct{}{}
	}); err != nil {
		t.Fatalf("RegisterTimers: %v", err)
	}
	if got := len(s.Jobs()); got != 2 {
		t.Fatalf("want 2 jobs (auto-close + digest), got %d", got)
	}
	s.Start() // no RunNow(): the at-boot run must fire on its own.

	select {
	case <-ls.gotCtx:
	case <-time.After(3 * time.Second):
		t.Fatal("auto-close sweep did not run at boot (deferred to start+15m)")
	}
	select {
	case <-digestFired:
	case <-time.After(3 * time.Second):
		t.Fatal("digest tick did not run at boot (deferred to start+interval)")
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer (the scheduler runs jobs on its own
// goroutine while the test reads the buffer).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRegisterTimersAutoCloseLogsError asserts a non-nil AutoCloseStale error is
// logged via the supplied logger, and that a nil logger does not panic.
func TestRegisterTimersAutoCloseLogsError(t *testing.T) {
	wantErr := errors.New("list boom")

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	p := newPipeline(newFakeIncidentStore(), &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}))
	ls := &signalingLister{gotCtx: make(chan context.Context, 1), err: wantErr}
	s := newTestScheduler(t)
	if err := p.RegisterTimers(context.Background(), logger, s, ls, time.Hour, nil); err != nil {
		t.Fatalf("RegisterTimers: %v", err)
	}
	s.Start()
	if err := s.Jobs()[0].RunNow(); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	<-ls.gotCtx // task ran; logging happens synchronously after ListOpenIncidents returns the error

	// Poll the buffer briefly: the log write happens just after the lister returns.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "list boom") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "list boom") {
		t.Errorf("auto-close error was not logged; buffer=%q", buf.String())
	}

	// nil logger: must not panic.
	pNil := newPipeline(newFakeIncidentStore(), &capturingDispatcher{},
		WithCaps(Caps{IncidentAutoClose: 24 * time.Hour}))
	lsNil := &signalingLister{gotCtx: make(chan context.Context, 1), err: wantErr}
	sNil := newTestScheduler(t)
	if err := pNil.RegisterTimers(context.Background(), nil, sNil, lsNil, time.Hour, nil); err != nil {
		t.Fatalf("RegisterTimers(nil logger): %v", err)
	}
	sNil.Start()
	if err := sNil.Jobs()[0].RunNow(); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	select {
	case <-lsNil.gotCtx:
	case <-time.After(3 * time.Second):
		t.Fatal("nil-logger auto-close task did not run within 3s")
	}
}
