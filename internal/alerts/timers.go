package alerts

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// IncidentLister lists open incidents for the auto-close sweep. *store.DB
// satisfies this via ListOpenIncidents.
type IncidentLister interface {
	ListOpenIncidents(ctx context.Context) ([]model.Alert, error)
}

// AutoCloseStale closes open incidents whose LastUpdatedAt is older than
// Options.IncidentAutoClose (default 24h). Runs on a timer (RegisterTimers).
func (p *Pipeline) AutoCloseStale(ctx context.Context, lister IncidentLister) error {
	now := p.opts.Now()
	open, err := lister.ListOpenIncidents(ctx)
	if err != nil {
		return err
	}
	for _, inc := range open {
		if now.Sub(inc.LastUpdatedAt) >= p.opts.IncidentAutoClose {
			if err := p.store.CloseIncident(ctx, inc.ID, now, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// RegisterTimers wires the incident auto-close sweep (and a digest tick) onto a
// gocron scheduler. The sweep job runs every 15 minutes and closes incidents
// idle longer than Options.IncidentAutoClose (default 24h). The digest tick fires
// every digestInterval (wires alerting.digest.schedule; a non-positive value
// falls back to 1h). The caller owns the scheduler's lifecycle (Start/Shutdown).
//
// Both jobs use gocron.WithStartImmediately so they ALSO run once at scheduler
// start, then follow their interval — without it a DurationJob defers its first
// run to now()+interval, leaving a cold window after every restart where no stale
// sweep runs (up to 15m) and the digest buffer is not drained (up to 1h) (F57).
//
// The auto-close task runs under the supplied ctx (so a daemon shutdown cancels
// the sweep mid-flight rather than leaking a background context); a non-nil
// AutoCloseStale error is logged via log when log != nil. The digest tick wraps
// the supplied digest func; a nil digest registers no digest job.
func (p *Pipeline) RegisterTimers(ctx context.Context, log *slog.Logger, s gocron.Scheduler, lister IncidentLister, digestInterval time.Duration, digest func()) error {
	if _, err := s.NewJob(
		gocron.DurationJob(15*time.Minute),
		gocron.NewTask(func() {
			if err := p.AutoCloseStale(ctx, lister); err != nil && log != nil {
				log.Error("auto-close sweep failed", obs.KeyComponent, "alerts", obs.KeyError, err.Error())
			}
		}),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	); err != nil {
		return err
	}
	if digest != nil {
		if digestInterval <= 0 {
			digestInterval = time.Hour
		}
		if _, err := s.NewJob(
			gocron.DurationJob(digestInterval),
			gocron.NewTask(digest),
			gocron.WithStartAt(gocron.WithStartImmediately()),
		); err != nil {
			return err
		}
	}
	return nil
}
