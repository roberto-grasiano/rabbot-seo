package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// sitemapURLCap bounds the URL list persisted in a sitemap snapshot's
// ParsedEntries. The default MaxPages budget (2000) bounds typical sites; this cap
// protects unbounded (max_pages_per_site: 0) sites from writing an unbounded JSON
// blob into file_snapshots. Beyond the cap the snapshot stores urls_capped:true and
// diffing falls back to the set hash alone (no added/dropped samples).
const sitemapURLCap = 20000

// sitemapDropSampleLimit caps how many dropped paths a sitemap_xml warning names.
const sitemapDropSampleLimit = 3

// changeTypeSitemapCoverageDrift is the site-level change_type emitted when the
// reconciled coverage drift grows vs the prior snapshot's block. It is NOT produced
// by diff.CompareFile (which only knows the set hash + status); RefreshSitemap
// raises it directly. severityForField classifies it warning.
const changeTypeSitemapCoverageDrift = "sitemap_coverage_drift"

// sitemapCoverageBlock is the coverage object persisted inside a FileKindSitemap
// snapshot's ParsedEntries JSON. Its JSON keys are read back by
// store.SitemapCoverage (sitemapParsedEntries) and by the drift gate's comparison
// against the prior snapshot — keep the snake_case tags identical on both sides.
type sitemapCoverageBlock struct {
	SitemappedUncrawled  int `json:"sitemapped_uncrawled"`
	SitemappedUnadmitted int `json:"sitemapped_unadmitted"`
	CrawledNotInSitemap  int `json:"crawled_not_in_sitemap"`
}

// sitemapDoc is the versioned JSON document stored in a sitemap snapshot's
// ParsedEntries: {"v":1,count,truncated,incomplete,urls,urls_capped,coverage{...}}.
//
// In v1 "truncated" and "urls_capped" carry the same value: both report that
// the stored urls list was cut at sitemapURLCap. Go consumers must read
// URLsCapped (sitemapSetDiffSummary does); Truncated exists for schema parity
// with the spec and is reserved to diverge if doc-level truncation ever gets
// its own signal distinct from list capping.
type sitemapDoc struct {
	V          int                  `json:"v"`
	Count      int                  `json:"count"`
	Truncated  bool                 `json:"truncated"`
	Incomplete bool                 `json:"incomplete"`
	URLs       []string             `json:"urls"`
	URLsCapped bool                 `json:"urls_capped"`
	Coverage   sitemapCoverageBlock `json:"coverage"`
}

// RefreshSitemap runs one sitemap collection pass for a site and turns it into the
// watch: it collects (admitting new URLs in the same pass), reconciles
// urls.in_sitemap against the collected set, dedup-persists a FileKindSitemap
// snapshot, and ingests alert events for accessibility regressions, set changes,
// and coverage drift. It mirrors RefreshRobots' shape end to end.
//
// Honest partial-failure semantics: when the collection is Incomplete (a child
// sitemap failed / a doc was truncated mid-BFS), the reconcile is additive-only
// (never flips in_sitemap 1→0) and drop reporting in the set-diff is suppressed, so
// a partial read can never masquerade as a mass URL drop.
//
// The first snapshot is an alert-silent baseline (CompareFile emits nothing, and
// the drift gate needs a prior coverage block) — the PR #57 first-crawl-guard
// philosophy, applied to the sitemap entity.
func (s *SideTimers) RefreshSitemap(ctx context.Context, site model.Site) error {
	col, err := s.Sitemaps.CollectAndSeed(ctx, site)
	if err != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, err)
	}

	// Canonical set: sort-unique the locs, join with "\n", hash. This is the
	// snapshot's ContentSHA256 and the set-change signal.
	locs := sortUniqueLocs(col.Entries)
	hash := extract.ContentSHA256(strings.Join(locs, "\n"))

	// Reconcile urls.in_sitemap against the collected set. additiveOnly on an
	// incomplete read so a truncated collection never flips flags off.
	if rerr := s.URLStore.ReconcileSitemapMembership(ctx, site.ID, locs, col.Incomplete); rerr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, rerr)
	}

	// Live coverage counts (post-reconcile) for the snapshot block + drift gate.
	counts, cerr := s.URLStore.SitemapLiveCounts(ctx, site.ID)
	if cerr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, cerr)
	}
	// sitemapped_unadmitted = declared locs that never made it into the inventory
	// (page-cap exhaustion, same-host/SSRF rejects). |declared| − |in inventory|,
	// floored at zero (the reconcile upserts presence, but admission can be capped).
	unadmitted := len(locs) - counts.InSitemapTotal
	if unadmitted < 0 {
		unadmitted = 0
	}
	cov := sitemapCoverageBlock{
		SitemappedUncrawled:  counts.SitemappedUncrawled,
		SitemappedUnadmitted: unadmitted,
		CrawledNotInSitemap:  counts.CrawledNotInSitemap,
	}

	capped := len(locs) > sitemapURLCap
	storedURLs := locs
	if capped {
		storedURLs = locs[:sitemapURLCap]
	}
	doc := sitemapDoc{
		V:          1,
		Count:      len(locs),
		Truncated:  capped, // the stored list was truncated to the cap
		Incomplete: col.Incomplete,
		URLs:       storedURLs,
		URLsCapped: capped,
		Coverage:   cov,
	}
	parsed, merr := json.Marshal(doc)
	if merr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, merr)
	}

	prev, ok, lerr := s.FileStore.LatestFileSnapshot(ctx, site.ID, model.FileKindSitemap)
	if lerr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, lerr)
	}

	// snapHash is the ContentSHA256 the snapshot is persisted with — the diff
	// baseline of record. On an INCOMPLETE pass the collected loc set is partial,
	// so its hash must NOT become the baseline: persisting the partial hash makes
	// the next COMPLETE pass diff full-vs-partial and emit a phantom drop+recovery
	// pair (spec lines 47-48: an incomplete collection must never masquerade as a
	// mass URL drop). Carry forward the prior snapshot's hash so the next complete
	// pass diffs complete-vs-complete. The snapshot still persists for state (count,
	// coverage, incomplete flag) — only the set-diff baseline is held steady.
	snapHash := hash
	if col.Incomplete {
		if !ok {
			// First-ever pass AND incomplete: there is no prior hash to carry
			// forward, so skip the write entirely — persisting the partial hash
			// would make it the baseline and the next COMPLETE pass would fire a
			// phantom drop. The additive-only reconcile above already admitted
			// what was collected; the next complete pass becomes the true,
			// alert-silent baseline.
			return nil
		}
		snapHash = prev.ContentSHA256
	}

	// Compute the drift signal against the prior snapshot's coverage block BEFORE
	// the dedup short-circuit, because a coverage change with an unchanged set/status
	// must still write a new row (the snapshot is the drift state of record).
	driftGrew, prevCov := coverageDriftGrew(ok, prev, cov)

	// Dedup: skip the write when the set hash, seed status, AND drift-gated coverage
	// all match the prior snapshot. A pure coverage *decrease* (driftGrew=false but
	// counts changed) still updates state silently — persist so the next pass diffs
	// against current truth.
	coverageUnchanged := ok && prevCov == cov
	if ok && prev.ContentSHA256 == snapHash && prev.HTTPStatus == col.SeedStatus && coverageUnchanged {
		return nil
	}

	newSnap := model.FileSnapshot{
		SiteID:        site.ID,
		Kind:          model.FileKindSitemap,
		FetchedAt:     s.now(),
		ContentSHA256: snapHash,
		ParsedEntries: string(parsed),
		HTTPStatus:    col.SeedStatus,
	}
	id, serr := s.FileStore.SaveFileSnapshot(ctx, newSnap)
	if serr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, serr)
	}
	newSnap.ID = id

	// The first snapshot is a baseline: CompareFile emits nothing and the drift gate
	// has no prior block — alert-silent. Also silent when alerting is disabled.
	if !ok || s.Alerts == nil {
		return nil
	}

	var prevDoc sitemapDoc
	_ = json.Unmarshal([]byte(prev.ParsedEntries), &prevDoc) // best-effort; empty on garbage

	var ingestErr error
	ingest := func(ev alerts.Event) {
		if ierr := s.Alerts.Ingest(ctx, ev); ierr != nil {
			ingestErr = errors.Join(ingestErr, ierr)
		}
	}

	for _, fc := range diff.CompareFile(newSnap, prev, s.now()) {
		// Suppress the sitemap_xml SET-change event on an incomplete pass: a partial
		// read cannot establish that the set actually changed (spec lines 47-48). The
		// carried-forward baseline (snapHash above) already keeps CompareFile silent
		// here in the steady case; this guard is the explicit, load-bearing contract.
		// sitemap_xml_STATUS (accessibility regression) must still fire so a 200→404
		// break is visible.
		if fc.Field == "sitemap_xml" && col.Incomplete {
			continue
		}
		ev := alerts.Event{
			SiteID:     fc.SiteID,
			Site:       site.BaseURL,
			ChangeType: fc.Field,
			Severity:   severityForField(fc.Field),
			Before:     fc.OldValue,
			After:      fc.NewValue,
		}
		// sitemap_xml carries raw set hashes from CompareFile; swap them for a
		// human-useful Before/After set-diff summary (counts + added/dropped samples).
		if fc.Field == "sitemap_xml" {
			ev.Before, ev.After = sitemapSetDiffSummary(prevDoc, doc, col.Incomplete)
		}
		ingest(ev)
	}

	// Coverage-drift is a watch-only signal (not in CompareFile): emit a warning
	// only when drift GREW vs the prior block. Decreases update state silently.
	// Suppressed on an incomplete pass: an additive-only reconcile can grow
	// sitemapped_uncrawled purely from newly-admitted-but-uncrawled URLs, which is
	// not a genuine coverage regression — a partial read must not masquerade as a
	// real signal (spec lines 47-48).
	if driftGrew && !col.Incomplete {
		ingest(alerts.Event{
			SiteID:     site.ID,
			Site:       site.BaseURL,
			ChangeType: changeTypeSitemapCoverageDrift,
			Severity:   severityForField(changeTypeSitemapCoverageDrift),
			Before:     coverageSummary(prevCov),
			After:      coverageSummary(cov),
		})
	}

	if ingestErr != nil {
		return fmt.Errorf("refresh sitemap for site %d: %w", site.ID, ingestErr)
	}
	return nil
}

// sortUniqueLocs returns the deduplicated, lexically sorted locs of the collected
// entries — the canonical set used for hashing and reconciliation.
func sortUniqueLocs(entries []SitemapEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Loc == "" {
			continue
		}
		if _, ok := seen[e.Loc]; ok {
			continue
		}
		seen[e.Loc] = struct{}{}
		out = append(out, e.Loc)
	}
	sort.Strings(out)
	return out
}

// coverageDriftGrew reports whether the reconciled coverage drift increased vs the
// prior snapshot's coverage block. Drift "grows" when either bucket increased:
// (sitemapped_uncrawled + sitemapped_unadmitted) or crawled_not_in_sitemap. It also
// returns the prior block (zero when there is no prior snapshot). When there is no
// prior snapshot (baseline) it returns false — the first pass never alerts.
func coverageDriftGrew(hasPrev bool, prev model.FileSnapshot, cur sitemapCoverageBlock) (bool, sitemapCoverageBlock) {
	if !hasPrev {
		return false, sitemapCoverageBlock{}
	}
	var prevDoc sitemapDoc
	_ = json.Unmarshal([]byte(prev.ParsedEntries), &prevDoc)
	prevCov := prevDoc.Coverage
	grew := (cur.SitemappedUncrawled+cur.SitemappedUnadmitted) > (prevCov.SitemappedUncrawled+prevCov.SitemappedUnadmitted) ||
		cur.CrawledNotInSitemap > prevCov.CrawledNotInSitemap
	return grew, prevCov
}

// sitemapSetDiffSummary builds the human-useful Before/After for a sitemap_xml set
// change: Before "1450 urls", After "1462 urls (+15, -3; dropped: /a, /b, /c)".
// Drop reporting (the added/dropped tail) is suppressed when either side was an
// incomplete read or a URL-list-capped snapshot — a partial/capped list cannot
// distinguish a real drop from an unread URL.
func sitemapSetDiffSummary(prev, cur sitemapDoc, incomplete bool) (before, after string) {
	before = strconv.Itoa(prev.Count) + " urls"
	suppress := incomplete || prev.URLsCapped || cur.URLsCapped

	if suppress {
		after = strconv.Itoa(cur.Count) + " urls"
		return before, after
	}

	prevSet := make(map[string]struct{}, len(prev.URLs))
	for _, u := range prev.URLs {
		prevSet[u] = struct{}{}
	}
	curSet := make(map[string]struct{}, len(cur.URLs))
	for _, u := range cur.URLs {
		curSet[u] = struct{}{}
	}
	added := 0
	for u := range curSet {
		if _, ok := prevSet[u]; !ok {
			added++
		}
	}
	var dropped []string
	for _, u := range prev.URLs { // stable order: prev's order
		if _, ok := curSet[u]; !ok {
			dropped = append(dropped, u)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d urls (+%d, -%d", cur.Count, added, len(dropped))
	if len(dropped) > 0 {
		sample := dropped
		if len(sample) > sitemapDropSampleLimit {
			sample = sample[:sitemapDropSampleLimit]
		}
		b.WriteString("; dropped: ")
		b.WriteString(strings.Join(sample, ", "))
	}
	b.WriteString(")")
	return before, b.String()
}

// coverageSummary renders a coverage block as a compact one-liner for an alert
// body, e.g. "uncrawled=5 unadmitted=0 orphaned=3".
func coverageSummary(c sitemapCoverageBlock) string {
	return fmt.Sprintf("uncrawled=%d unadmitted=%d orphaned=%d",
		c.SitemappedUncrawled, c.SitemappedUnadmitted, c.CrawledNotInSitemap)
}
