// Package supervisor owns the process lifecycle as a native OS service via
// kardianos/service, holds the root context, and runs the (empty in M0)
// scheduler loop. It implements service.Interface and service.Shutdowner.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kardianos/service"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// Daemon is the supervised long-running process. In M0 the loop is empty:
// a ticker that does nothing yet and exits cleanly on ctx cancellation.
type Daemon struct {
	Logger       *slog.Logger
	TickInterval time.Duration
	OnTick       func(ctx context.Context) // optional per-tick work (M1+)
	OnReload     func() error              // SIGHUP / control reload hook
	OnStart      func(ctx context.Context) error
	OnStop       func() error

	// mu guards ctx/cancel: Start writes them on the service-manager goroutine
	// while Stop/Shutdown read cancel, so a mutex gives them a happens-before
	// relationship independent of the service manager's call ordering.
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	// runLoop is the loop body Start spawns; it defaults to d.RunLoop and exists
	// as a seam so a test can drive the service-managed goroutine to a failing
	// return without standing up a real ticker loop. Production never sets it.
	runLoop func(context.Context) error
}

// service.Interface: Start is called by the OS service manager (non-blocking).
func (d *Daemon) Start(s service.Service) (err error) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(context.Background())
	ctx, cancel := d.ctx, d.cancel
	d.mu.Unlock()

	// Don't leak the root context (and its timer/goroutine resources) if start
	// fails: cancel it on any error return.
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	if d.OnStart != nil {
		if err = d.OnStart(ctx); err != nil {
			return err
		}
	}
	// NOTE: this kardianos/service entrypoint is not exercised by the current
	// binary — `rabbot run` calls runDaemon -> d.RunLoop(ctx) directly (blocking)
	// and drains its own goroutines. The RunLoop goroutine spawned here is
	// intentionally not joined by Stop (Stop signals via d.cancel and returns
	// promptly, as the service contract requires); wiring the service-managed path
	// with a join is deferred to M3 distribution.
	loop := d.runLoop
	if loop == nil {
		loop = d.RunLoop
	}
	go func() {
		// Don't discard the loop's error: on the service-managed path Stop only
		// signals via d.cancel and never inspects this goroutine's return, so a
		// real failure here would otherwise be invisible. A clean shutdown returns
		// nil (or, defensively, context.Canceled) — surface only genuine failures.
		if err := loop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if d.Logger != nil {
				d.Logger.Error("scheduler loop exited with error",
					obs.KeyComponent, "supervisor", obs.KeyError, err.Error())
			}
		}
	}()
	return nil
}

// service.Interface: Stop is called on service stop (must return promptly).
func (d *Daemon) Stop(s service.Service) error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if d.OnStop != nil {
		return d.OnStop()
	}
	return nil
}

// service.Shutdowner: Shutdown is called on system shutdown.
func (d *Daemon) Shutdown(s service.Service) error {
	return d.Stop(s)
}

// RunLoop runs the empty scheduler ticker until ctx is cancelled.
func (d *Daemon) RunLoop(ctx context.Context) error {
	interval := d.TickInterval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if d.Logger != nil {
				d.Logger.Info("scheduler loop stopping", obs.KeyComponent, "supervisor")
			}
			return nil
		case <-t.C:
			if d.OnTick != nil {
				d.OnTick(ctx)
			}
		}
	}
}

// Reload invokes the configured reload hook (SIGHUP / control /v1/reload).
func (d *Daemon) Reload() error {
	if d.OnReload == nil {
		return nil
	}
	return d.OnReload()
}
