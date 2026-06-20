package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

func TestRunLoopExitsOnContextCancel(t *testing.T) {
	var ticks int64
	d := &Daemon{
		TickInterval: 5 * time.Millisecond,
		OnTick:       func(context.Context) { atomic.AddInt64(&ticks, 1) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.RunLoop(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLoop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunLoop did not exit within 1s of context cancel")
	}
	if atomic.LoadInt64(&ticks) == 0 {
		t.Error("expected at least one tick before cancel")
	}
}

func TestReloadHookInvoked(t *testing.T) {
	var reloaded int64
	d := &Daemon{
		TickInterval: time.Hour,
		OnReload:     func() error { atomic.AddInt64(&reloaded, 1); return nil },
	}
	if err := d.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if atomic.LoadInt64(&reloaded) != 1 {
		t.Errorf("reload count = %d, want 1", reloaded)
	}
}

func TestReloadNoHookIsNoop(t *testing.T) {
	d := &Daemon{}
	if err := d.Reload(); err != nil {
		t.Errorf("Reload with no hook should be nil, got %v", err)
	}
}

// TestStartCancelsContextOnOnStartError (F47) verifies Start does not leak the
// root context when OnStart fails: the context it created must be cancelled
// before Start returns the error.
func TestStartCancelsContextOnOnStartError(t *testing.T) {
	wantErr := errors.New("boom")
	var startCtx context.Context
	d := &Daemon{
		OnStart: func(ctx context.Context) error {
			startCtx = ctx
			return wantErr
		},
	}

	err := d.Start(nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start should return the OnStart error; got %v", err)
	}
	if startCtx == nil {
		t.Fatal("OnStart was not invoked")
	}
	select {
	case <-startCtx.Done():
		// good: context was cancelled, no leak.
	default:
		t.Fatal("Start leaked the root context: it was not cancelled after OnStart error")
	}
}

// syncBuffer is a goroutine-safe io.Writer: the service-managed RunLoop runs on
// its own goroutine, so the test reads the log buffer concurrently with that
// goroutine's write. Without the lock this races under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestStartLogsRunLoopError (F20.6) verifies the service-managed Start path no
// longer discards RunLoop's error: when the spawned RunLoop goroutine fails with
// a non-clean-shutdown error, the daemon logs it at error level so a
// service-managed failure is not invisible.
func TestStartLogsRunLoopError(t *testing.T) {
	sb := &syncBuffer{}
	wantErr := errors.New("runloop exploded")
	done := make(chan struct{})

	d := &Daemon{
		Logger: obs.NewLogger(sb, "info"),
		// runLoop is the injectable seam standing in for the real d.RunLoop so the
		// test can drive the goroutine to a failing return without a real loop.
		runLoop: func(context.Context) error {
			defer close(done)
			return wantErr
		},
	}

	if err := d.Start(nil); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// Wait for the spawned goroutine to finish (and log) before asserting.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunLoop goroutine did not run within 1s")
	}
	// The log write happens after runLoop returns; give the goroutine a moment to
	// emit it after our seam closed done, then poll the buffer.
	deadline := time.After(time.Second)
	for !strings.Contains(sb.String(), wantErr.Error()) {
		select {
		case <-deadline:
			t.Fatalf("RunLoop error %q was not logged; buffer=%q", wantErr, sb.String())
		case <-time.After(2 * time.Millisecond):
		}
	}

	// Parse the emitted JSON line and assert it is an error-level record carrying
	// the canonical component + error keys.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec[obs.KeyError] != wantErr.Error() {
			continue
		}
		found = true
		if rec["level"] != "ERROR" {
			t.Errorf("RunLoop error logged at level %v, want ERROR", rec["level"])
		}
		if rec[obs.KeyComponent] != "supervisor" {
			t.Errorf("RunLoop error logged with component %v, want supervisor", rec[obs.KeyComponent])
		}
	}
	if !found {
		t.Fatalf("no log record carried the RunLoop error; buffer=%q", sb.String())
	}
}

// TestStartDoesNotLogCleanShutdown (F20.6) verifies the error-logging in Start is
// scoped to real failures: a clean shutdown (RunLoop returns context.Canceled, or
// nil) must NOT be logged at error level, otherwise every normal Stop would emit a
// spurious error line.
func TestStartDoesNotLogCleanShutdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		ret  error
	}{
		{"nil", nil},
		{"context.Canceled", context.Canceled},
		{"wrapped context.Canceled", errors.Join(errors.New("loop done"), context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &syncBuffer{}
			done := make(chan struct{})
			d := &Daemon{
				Logger: obs.NewLogger(sb, "info"),
				runLoop: func(context.Context) error {
					defer close(done)
					return tc.ret
				},
			}
			if err := d.Start(nil); err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("RunLoop goroutine did not run within 1s")
			}
			// Allow any (erroneous) log emission to land before asserting absence.
			time.Sleep(20 * time.Millisecond)
			if got := sb.String(); strings.Contains(got, "ERROR") {
				t.Errorf("clean shutdown should not log at ERROR level; buffer=%q", got)
			}
		})
	}
}
