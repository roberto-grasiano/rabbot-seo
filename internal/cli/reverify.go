package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// reverifyFn is the injectable verifier seam behind the re-verify loop. It
// mirrors verify.Verify exactly; production wires verify.Verify and
// tests inject a stub so the living-state transitions are asserted with no live
// network. The verifier DERIVES the expected token from opts.Key (it never trusts
// a caller-supplied token), so the loop threads the per-instance key through, not
// the stored token.
type reverifyFn func(ctx context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error)

// reverifyStore is the narrow store seam the re-verify loop needs. *store.DB
// satisfies it; tests inject a wrapper that can fault a single per-site write so
// the best-effort (log + continue) pass is exercised without a live DB fault.
type reverifyStore interface {
	ListSites(ctx context.Context) ([]model.Site, error)
	GetVerification(ctx context.Context, siteID int64) (verify.ProofRecord, error)
	SaveVerification(ctx context.Context, siteID int64, rec verify.ProofRecord) error
}

// reverifyAll re-checks the living verification state of every enabled site,
// DEMOTING a site whose proof has vanished but never auto-promoting one. It is
// the testable core of the daemon re-verify loop (spec D5/§E "living state"):
//
//   - StateAttested: a TERMINAL human decision — never re-checked, never
//     auto-promoted (the stub is never called for it).
//   - StateVerified with a method: re-run the verifier, which DERIVES the expected
//     token from the per-instance key and re-checks the surface. On a CLEAN miss
//     (the Outcome record is StateThrottled, no error) the verified->throttled flip
//     is persisted — the proof disappeared, so the living state demotes. On a
//     transport/DNS ERROR (inconclusive) the record is NOT overwritten, so a
//     transient network blip cannot flap a verified site to throttled.
//   - StateVerified with NO method: un-recheckable (there is no surface to re-derive
//     against), so leaving it verified would run it at full speed forever.
//     Demote-on-doubt (spec D5) DEMOTES it to throttled rather than silently keeping
//     it verified.
//   - A never-attempted record (empty method at StateThrottled, e.g. a bare
//     throttled site) is skipped — there is nothing to re-check; a throttled site
//     is lifted only by an explicit `rabbot verify`.
//
// The loop only ever DEMOTES, and every per-site write is BEST-EFFORT: a
// SaveVerification failure is logged and skipped (matching every other per-site
// op in the daemon's side-timer loops) so one bad write does not suppress the
// living-state demotions for the rest of the fleet in that pass. It honors ctx
// cancellation between sites, and every DB call takes ctx (the caller — the
// robots side-timer goroutine — is joined by pipelineWG, so cancel stops it and
// the DB is drained before Close).
//
// It returns the number of sites whose proof actually DEMOTED this pass (a clean
// token-loss flip or an un-recheckable-verified demotion whose write LANDED). The
// caller gates the destructive reconcileAfterReverify on demoted > 0 (PR31 #2):
// a pass that changes nothing must NOT reset every homepage's adaptive schedule.
// A demotion whose best-effort save fails is NOT counted — its proof did not
// change, so reconcile would not help it.
func reverifyAll(ctx context.Context, db reverifyStore, vf reverifyFn, key []byte, now time.Time, logger *slog.Logger) (int, error) {
	sites, err := db.ListSites(ctx)
	if err != nil {
		return 0, err
	}
	demotedCount := 0
	for _, s := range sites {
		if ctx.Err() != nil {
			return demotedCount, ctx.Err()
		}
		if !s.Enabled {
			continue
		}
		rec, gerr := db.GetVerification(ctx, s.ID)
		if gerr != nil {
			continue // best-effort: a read glitch skips this site, never demotes it
		}
		switch rec.State {
		case verify.StateAttested:
			// Terminal: a deliberate human skip is never re-checked or auto-promoted.
			continue
		case verify.StateVerified:
			if rec.Method == "" {
				// Un-recheckable verified record (no method to re-derive against):
				// demote-on-doubt rather than keep it at full speed forever. Persist a
				// throttled record (best-effort).
				demoted := verify.ProofRecord{
					SiteID:           s.ID,
					State:            verify.StateThrottled,
					LastReverifiedAt: now,
				}
				if serr := db.SaveVerification(ctx, s.ID, demoted); serr != nil {
					if ctx.Err() == nil {
						logger.Debug("re-verify: demote un-recheckable verified record failed",
							obs.KeyComponent, "supervisor", obs.KeySite, s.BaseURL, obs.KeyError, serr.Error())
					}
					continue // save failed: proof unchanged, do NOT count as demoted
				}
				demotedCount++
				continue
			}
			out, verr := vf(ctx, verify.Request{
				SiteID: s.ID,
				Host:   hostFromURL(s.BaseURL),
				Method: rec.Method,
			}, verify.Options{Now: now, Key: key})
			if verr != nil {
				continue // inconclusive (transport/DNS) — do NOT demote
			}
			if out.Record.State != verify.StateVerified {
				// Clean token-loss: persist the verified->throttled demotion
				// (best-effort — one bad write must not abort the rest of the pass).
				if serr := db.SaveVerification(ctx, s.ID, out.Record); serr != nil {
					if ctx.Err() == nil {
						logger.Debug("re-verify: persist demotion failed",
							obs.KeyComponent, "supervisor", obs.KeySite, s.BaseURL, obs.KeyError, serr.Error())
					}
					continue // save failed: proof unchanged, do NOT count as demoted
				}
				demotedCount++
				continue
			}
			// Still verified: advance the living-state clock so LastReverifiedAt
			// reflects this pass rather than forever showing the original verify
			// time. Saving out.Record raw would clobber VerifiedAt to now and lose the
			// first-verification time, so preserve the EXISTING record and bump
			// only LastReverifiedAt (Method/Token/VerifiedAt unchanged; State stays
			// verified). Best-effort, matching the demotion write above.
			updated := rec
			updated.State = verify.StateVerified
			updated.LastReverifiedAt = now
			if serr := db.SaveVerification(ctx, s.ID, updated); serr != nil && ctx.Err() == nil {
				logger.Debug("re-verify: advance LastReverifiedAt failed",
					obs.KeyComponent, "supervisor", obs.KeySite, s.BaseURL, obs.KeyError, serr.Error())
			}
		default:
			// StateThrottled / never-attempted: lifted only by an explicit verify.
			continue
		}
	}
	return demotedCount, nil
}
