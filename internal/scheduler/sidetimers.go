package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// FileStore is the subset of store.Store needed for robots/sitemap file snapshots.
type FileStore interface {
	SaveFileSnapshot(ctx context.Context, fs model.FileSnapshot) (int64, error)
	LatestFileSnapshot(ctx context.Context, siteID int64, kind model.FileSnapshotKind) (model.FileSnapshot, bool, error)
}

// EventIngestor is the optional alerts sink for site-level (robots.txt) change
// events. It mirrors alerts.Pipeline.Ingest. A nil SideTimers.Alerts leaves the
// M1-only / test paths' behavior unchanged (snapshots persist, no alerts fire).
type EventIngestor interface {
	Ingest(ctx context.Context, e alerts.Event) error
}

// SitemapCollection is the result of one bounded sitemap collection pass: the
// post-budget URL set, the primary seed's identity and HTTP status, whether the
// pass was complete, and how many new URLs the same pass admitted into the
// inventory. It is the input to SideTimers.RefreshSitemap (the sitemap watch)
// and is produced by a SitemapCollector — the discovery package implements it.
//
// It lives here (not in discovery) because discovery imports scheduler (for
// ParseSitemap/ApplyBudget), so scheduler can never import discovery: the seam
// type has to sit on the imported side.
type SitemapCollection struct {
	Entries    []SitemapEntry // post-budget collected set
	SeedURL    string         // robots Sitemap: directive, else <base>/sitemap.xml
	SeedStatus int            // HTTP status of the primary seed doc; 0 = network error
	Incomplete bool           // any doc fetch failed / truncated mid-BFS
	Admitted   int            // new URLs upserted by this same pass
}

// SitemapCollector runs one bounded collection pass over a site's declared
// sitemaps, returning the collected set (for snapshotting/diffing) while the
// same pass admits newly-discovered URLs into the inventory. RefreshSitemap
// depends on this interface; discovery.Discoverer is the production impl.
type SitemapCollector interface {
	CollectAndSeed(ctx context.Context, site model.Site) (SitemapCollection, error)
}

// URLStore is the subset of store.DB the sitemap watch needs to reconcile
// urls.in_sitemap against the freshly collected loc set and to read the live
// coverage counts (post-reconcile) for the snapshot's coverage block + drift gate.
// A nil SideTimers.URLStore disables RefreshSitemap reconciliation/coverage (the
// robots-only / M1 paths leave it nil); RefreshSitemap requires it.
type URLStore interface {
	ReconcileSitemapMembership(ctx context.Context, siteID int64, locs []string, additiveOnly bool) error
	SitemapLiveCounts(ctx context.Context, siteID int64) (SitemapLiveCounts, error)
}

// SitemapLiveCounts are the urls-derived coverage counts computed live (after a
// reconcile) from the current inventory: sitemapped-but-uncrawled, crawled-but-
// absent-from-sitemap, and the total number of rows flagged in_sitemap=1 (used to
// derive sitemapped_unadmitted = |declared locs| − InSitemapTotal). It mirrors the
// store's read model but is computed at refresh time, before the new snapshot is
// persisted, so the first pass (no prior snapshot) still gets real counts.
type SitemapLiveCounts struct {
	SitemappedUncrawled int
	CrawledNotInSitemap int
	InSitemapTotal      int
}

// SideTimers drives the fixed-cadence robots.txt 5-min recheck (and sitemap
// refresh) that persist file_snapshots. Schedule these via gocron in the daemon.
type SideTimers struct {
	FileStore FileStore
	Robots    *frontier.RobotsCache
	// Sitemaps runs one bounded collection pass per RefreshSitemap (collect +
	// admit), returning the collected set/seed-status/completeness. Required by
	// RefreshSitemap; the production impl is discovery.Discoverer.
	Sitemaps SitemapCollector
	// URLStore reconciles urls.in_sitemap and reads live coverage counts for the
	// sitemap watch. Required by RefreshSitemap.
	URLStore URLStore
	// Alerts, when non-nil, receives a site-level alerts.Event for each detected
	// robots.txt change so the robots side-timer feeds the SAME incident pipeline
	// as the per-URL change stream (process.go). Nil = no alerting (M1-only paths,
	// disabled alerting, existing tests): snapshots still persist, no event fires.
	Alerts EventIngestor
	Now    func() time.Time
}

func (s *SideTimers) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// RefreshRobots fetches a site's robots.txt and persists a file_snapshot — but
// only when it actually changed. robots.txt is near-static and the side-timer
// fires every 5 minutes per site, so an unconditional INSERT would grow
// file_snapshots by ~288 redundant rows/site/day. We dedup against the latest
// stored snapshot by content hash (and HTTP status), saving only on a real change.
func (s *SideTimers) RefreshRobots(ctx context.Context, siteID int64, baseURL string) error {
	raw, status, err := s.Robots.Raw(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("refresh robots for site %d: %w", siteID, err)
	}
	hash := extract.ContentSHA256(string(raw))

	prev, ok, lerr := s.FileStore.LatestFileSnapshot(ctx, siteID, model.FileKindRobots)
	if lerr != nil {
		return fmt.Errorf("refresh robots for site %d: %w", siteID, lerr)
	}
	if ok && prev.ContentSHA256 == hash && prev.HTTPStatus == status {
		return nil // unchanged: skip the redundant write
	}

	newSnap := model.FileSnapshot{
		SiteID:        siteID,
		Kind:          model.FileKindRobots,
		FetchedAt:     s.now(),
		ContentSHA256: hash,
		ParsedEntries: string(raw),
		HTTPStatus:    status,
	}
	id, serr := s.FileStore.SaveFileSnapshot(ctx, newSnap)
	if serr != nil {
		return fmt.Errorf("refresh robots for site %d: %w", siteID, serr)
	}

	// Diff against the prior snapshot and feed the alerts pipeline (robots half of
	// #8): a deploy that ships "Disallow: /" must raise a Slack alert, just like a
	// per-URL change. Only when a prior snapshot existed (ok) — the first snapshot
	// is a baseline (CompareFile emits nothing for it anyway). Best-effort-complete
	// like process.go's ingest loop: an Ingest failure is joined and surfaced, never
	// allowed to drop the remaining events or unwind the already-saved snapshot.
	if !ok || s.Alerts == nil {
		return nil
	}
	newSnap.ID = id
	var ingestErr error
	for _, fc := range diff.CompareFile(newSnap, prev, s.now()) {
		// Site-level event: no URL. Site label mirrors the per-URL change stream,
		// which uses site.BaseURL (process.go) — here that is baseURL verbatim.
		ev := alerts.Event{
			SiteID:     fc.SiteID,
			Site:       baseURL,
			ChangeType: fc.Field,
			Severity:   severityForField(fc.Field),
			Before:     fc.OldValue,
			After:      fc.NewValue,
		}
		if ierr := s.Alerts.Ingest(ctx, ev); ierr != nil {
			ingestErr = errors.Join(ingestErr, ierr)
		}
	}
	if ingestErr != nil {
		return fmt.Errorf("refresh robots for site %d: %w", siteID, ingestErr)
	}
	return nil
}
