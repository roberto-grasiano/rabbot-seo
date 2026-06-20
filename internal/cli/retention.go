package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// buildRetentionPolicy resolves config into the store-layer policy. Chunk is an
// internal constant (not a config knob); 5000 keeps each Layer 2 batch's write
// lock short on the single writer.
func buildRetentionPolicy(cfg *config.Config) store.RetentionPolicy {
	// Clamp to the same floors Validate() enforces. The daemon run path does NOT call
	// cfg.Validate(), so an unvalidated config could carry keep=0; clamp here as
	// defense in depth so Layer 1 always keeps a baseline and Layer 2's latest-row
	// protection (rn<=keep) can never delete the diff baseline.
	return store.RetentionPolicy{
		RawHTMLKeep:       max(1, cfg.Retention.RawHTMLKeep),
		SnapshotMaxAge:    cfg.RetentionSnapshotMaxAge(),
		FileSnapshotsKeep: max(2, cfg.Retention.FileSnapshotsKeep),
		Chunk:             5000,
	}
}

// registerRetentionSweep adds the periodic retention job to s when retention is
// enabled, returning whether it registered. The job runs once immediately at start
// (WithStartImmediately) so a freshly (re)started daemon prunes without waiting a
// full interval, then on each SweepInterval. The task runs under ctx so daemon
// shutdown cancels it mid-flight.
func registerRetentionSweep(ctx context.Context, logger *slog.Logger, s gocron.Scheduler, db *store.DB, cfg *config.Config) (bool, error) {
	if !cfg.Retention.Enabled {
		return false, nil
	}
	policy := buildRetentionPolicy(cfg)
	_, err := s.NewJob(
		gocron.DurationJob(cfg.RetentionSweepInterval()),
		gocron.NewTask(func() {
			res, rerr := db.ApplyRetention(ctx, policy, time.Now().UTC())
			if rerr != nil {
				logger.Error("retention sweep failed", obs.KeyComponent, "retention", obs.KeyError, rerr.Error())
				return
			}
			logger.Info("retention swept",
				obs.KeyComponent, "retention",
				"raw_html_nulled", res.RawHTMLNulled,
				"snapshots_deleted", res.SnapshotsDeleted,
				"file_snapshots_trimmed", res.FileSnapshotsTrimmed,
			)
		}),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	)
	if err != nil {
		return false, err
	}
	return true, nil
}
