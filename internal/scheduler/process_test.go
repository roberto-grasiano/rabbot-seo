package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

type fakeProcDeps struct {
	recorded         []model.Change
	applied          []int64 // urlIDs the rules engine was applied to
	appliedTruncated []bool  // truncated flag passed to ApplyRules, parallel to applied
	access           []alerts.AccessContext
	ingested         []alerts.Event
	resolved         []alerts.Event
	resolveErr       error            // when set, ResolveEvent returns it
	ingestErrOn      map[string]error // ChangeType -> error returned by IngestEvent (still recorded)
	// newFindings is returned verbatim by ApplyRules — the set of findings the rules
	// engine NEWLY opened this crawl, which ProcessFetch bridges into the alert path.
	newFindings []NewFinding
	// applyErr, when set, is returned by ApplyRules so the test can assert that a
	// rules-pass failure suppresses the RecordHealthScore trigger (A6 criterion 4).
	applyErr error
	// recordedHealth records every (siteID, urlID) pair RecordHealthScore is called
	// with, so a test can assert it fires exactly once after a successful rules pass.
	recordedHealth [][2]int64
}

func (f *fakeProcDeps) RecordChanges(ctx context.Context, changes []model.Change) error {
	f.recorded = append(f.recorded, changes...)
	return nil
}
func (f *fakeProcDeps) ApplyRules(ctx context.Context, urlID int64, importance float64, newSnap, oldSnap model.Snapshot, changes []model.Change, truncated bool) ([]NewFinding, error) {
	f.applied = append(f.applied, urlID)
	f.appliedTruncated = append(f.appliedTruncated, truncated)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return f.newFindings, nil
}
func (f *fakeProcDeps) RecordHealthScore(ctx context.Context, siteID, urlID int64) error {
	f.recordedHealth = append(f.recordedHealth, [2]int64{siteID, urlID})
	return nil
}
func (f *fakeProcDeps) HandleFetchClass(ctx context.Context, ac alerts.AccessContext, seo []alerts.Event) (bool, error) {
	f.access = append(f.access, ac)
	return ac.FetchClass != model.FetchOK, nil
}
func (f *fakeProcDeps) IngestEvent(ctx context.Context, e alerts.Event) error {
	f.ingested = append(f.ingested, e)
	if f.ingestErrOn != nil {
		if err, ok := f.ingestErrOn[e.ChangeType]; ok {
			return err
		}
	}
	return nil
}
func (f *fakeProcDeps) ResolveEvent(ctx context.Context, e alerts.Event) error {
	f.resolved = append(f.resolved, e)
	return f.resolveErr
}

func TestProcessFetchOK(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", ContentSHA256: "a", Indexable: true, HTTPStatus: 200, Canonical: "c", MetaRobots: "index", Headings: "h1", MetaDescription: "d"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0, LastFetchClass: model.FetchOK}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "New", ContentSHA256: "b", ContentSimhash: 0xFFFF, Indexable: true, HTTPStatus: 200, Canonical: "c", MetaRobots: "index", Headings: "h1", MetaDescription: "d"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	if len(deps.recorded) == 0 {
		t.Error("expected title change recorded")
	}
	if len(deps.applied) != 1 || deps.applied[0] != 5 {
		t.Errorf("rules engine must be applied to url 5, got %v", deps.applied)
	}
	if len(deps.ingested) == 0 {
		t.Error("expected the title change to be ingested as an alert event")
	}
}

// TestProcessFetch_RecordsHealthScore covers A6 criterion 4: after a successful
// rules pass ProcessFetch triggers RecordHealthScore(site.ID, u.ID) exactly once
// (the per-recheck compute-trigger seam), and when ApplyRules errors it does NOT —
// the health score must not be recomputed from a half-applied rules pass.
func TestProcessFetch_RecordsHealthScore(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", ContentSHA256: "a", Indexable: true, HTTPStatus: 200, Canonical: "c", MetaRobots: "index", Headings: "h1", MetaDescription: "d"}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "New", ContentSHA256: "b", ContentSimhash: 0xFFFF, Indexable: true, HTTPStatus: 200, Canonical: "c", MetaRobots: "index", Headings: "h1", MetaDescription: "d"}
	u := model.URL{ID: 5, SiteID: 7, URL: "https://ex.com/p", Importance: 1.0, LastFetchClass: model.FetchOK}
	site := model.Site{ID: 7, BaseURL: "https://ex.com", Name: "Ex"}

	t.Run("success records once", func(t *testing.T) {
		deps := &fakeProcDeps{}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if len(deps.recordedHealth) != 1 {
			t.Fatalf("expected RecordHealthScore once, got %d calls: %v", len(deps.recordedHealth), deps.recordedHealth)
		}
		if deps.recordedHealth[0] != [2]int64{7, 5} {
			t.Errorf("RecordHealthScore must be called with (site.ID=7, u.ID=5), got %v", deps.recordedHealth[0])
		}
	})

	t.Run("ApplyRules error suppresses the record", func(t *testing.T) {
		wantErr := errors.New("apply boom")
		deps := &fakeProcDeps{applyErr: wantErr}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		_, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false)
		if !errors.Is(err, wantErr) {
			t.Fatalf("ProcessFetch must surface the ApplyRules error, got %v", err)
		}
		if len(deps.recordedHealth) != 0 {
			t.Errorf("RecordHealthScore must NOT be called when ApplyRules errors, got %v", deps.recordedHealth)
		}
	})

	t.Run("non-ok fetch records nothing", func(t *testing.T) {
		deps := &fakeProcDeps{}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		// A blocked fetch is handled by the access gate and returns before the rules pass.
		if _, err := p.ProcessFetch(context.Background(), site, u, model.Snapshot{ID: 2, URLID: 5}, old, model.FetchHardBlock, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if len(deps.recordedHealth) != 0 {
			t.Errorf("a non-ok fetch must not record a health score, got %v", deps.recordedHealth)
		}
	})
}

// TestProcessFetch304OnlyAccessGate covers the 304/no-body ok fetch: newSnap is
// the zero snapshot (nothing persisted), so the processor must drive ONLY the
// access gate (to auto-close any operational incident on recovery) and must NOT
// run diff/rules — evaluating rules against a zero snapshot would open spurious
// issues (a healthy 304 reads as Indexable=false, HTTPStatus=0).
func TestProcessFetch304OnlyAccessGate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	// fc==ok with a zero newSnap (ID 0) is the 304 case.
	if _, err := p.ProcessFetch(context.Background(), site, u, model.Snapshot{}, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	if len(deps.access) != 1 || deps.access[0].FetchClass != model.FetchOK {
		t.Errorf("access gate must run once on a 304: %+v", deps.access)
	}
	if len(deps.recorded) != 0 || len(deps.applied) != 0 || len(deps.ingested) != 0 {
		t.Errorf("a 304 must run no diff/rules/ingest (recorded=%d applied=%d ingested=%d)",
			len(deps.recorded), len(deps.applied), len(deps.ingested))
	}
}

// TestProcessFetchResolveEventErrorSurfaces covers SC1: when a critical signal
// recovers (indexable flips false->true) the processor auto-resolves the open
// incident; if ResolveEvent fails, ProcessFetch must propagate that error rather
// than silently dropping it, so the crawl is recorded as failed.
func TestProcessFetchResolveEventErrorSurfaces(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	wantErr := errors.New("resolve boom")
	deps := &fakeProcDeps{resolveErr: wantErr}
	// oldSnap is non-indexable (ID!=0 so it's a real prior), newSnap is indexable: a recovery.
	oldSnap := model.Snapshot{ID: 1, URLID: 5, Indexable: false, HTTPStatus: 200}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Indexable: true, HTTPStatus: 200}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	_, err := p.ProcessFetch(context.Background(), site, u, newSnap, oldSnap, model.FetchOK, "", false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ProcessFetch must surface the ResolveEvent error, got %v", err)
	}
	if len(deps.resolved) == 0 {
		t.Error("expected ResolveEvent to be attempted on the indexability recovery")
	}
}

// TestProcessFetchStatusRecoveryOnly2xx guards F7: a dead URL (4xx/5xx) that now
// returns a 3xx redirect is NOT recovered — the original content is still gone, and
// a redirect is not a healthy status. resolveHealthyFields must only auto-close an
// http_status incident on a true 2xx success, never on a 3xx.
func TestProcessFetchStatusRecoveryOnly2xx(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// 404 -> 301: must NOT resolve the http_status incident.
	t.Run("4xx_to_3xx_does_not_resolve", func(t *testing.T) {
		deps := &fakeProcDeps{}
		oldSnap := model.Snapshot{ID: 1, URLID: 5, HTTPStatus: 404, Indexable: false}
		newSnap := model.Snapshot{ID: 2, URLID: 5, HTTPStatus: 301, Indexable: false}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, oldSnap, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		for _, e := range deps.resolved {
			if e.ChangeType == "http_status" {
				t.Errorf("a 404->301 must NOT auto-resolve the http_status incident (still broken)")
			}
		}
	})

	// 500 -> 200: real recovery, MUST resolve.
	t.Run("5xx_to_2xx_resolves", func(t *testing.T) {
		deps := &fakeProcDeps{}
		oldSnap := model.Snapshot{ID: 1, URLID: 5, HTTPStatus: 500, Indexable: false}
		newSnap := model.Snapshot{ID: 2, URLID: 5, HTTPStatus: 200, Indexable: false}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, oldSnap, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		found := false
		for _, e := range deps.resolved {
			if e.ChangeType == "http_status" {
				found = true
			}
		}
		if !found {
			t.Errorf("a 500->200 must auto-resolve the http_status incident")
		}
	})
}

// TestSeverityForIndexabilityReason guards F10: indexability_reason is emitted as
// a substantive change by diff.Compare and ingested as an alert event, but lacked a
// case in severityForField and fell through to SeverityInfo — under-routing a real
// deindexing-cause regression (which can change without the indexable bool flipping)
// and risking a silent drop under cap pressure. It must mirror indexable (critical).
func TestSeverityForIndexabilityReason(t *testing.T) {
	if got := severityForField("indexability_reason"); got != model.SeverityCritical {
		t.Errorf("severityForField(indexability_reason) = %q, want critical (mirrors indexable)", got)
	}
}

// TestSeverityForContent guards High#2: a main-content body change is a meaningful
// SEO signal (a rewritten/replaced page), but severityForField had no "content" case
// so it fell through to SeverityInfo — under-routing a real content change and risking
// a silent drop under cap pressure. It must route at the warning tier (alongside title
// / meta_description), not info.
func TestSeverityForContent(t *testing.T) {
	if got := severityForField("content"); got != model.SeverityWarning {
		t.Errorf("severityForField(content) = %q, want warning (mirrors title/meta_description)", got)
	}
}

// TestSeverityForRenderMode guards A8 acceptance #8: a render_mode FLIP is recorded
// as a substantive change-history event by diff.Compare, but it must route QUIET — at
// the info tier — NOT critical/warning. The needs_rendering RULE carries the warning
// alert; the field-flip is history-only. render_mode achieves Info by being absent from
// every non-default case in severityForField (it falls through to the info default).
// This test pins that routing so a future edit that mistakenly adds render_mode to a
// warning/critical case (turning the history event into a duplicate page) is caught.
func TestSeverityForRenderMode(t *testing.T) {
	if got := severityForField("render_mode"); got != model.SeverityInfo {
		t.Errorf("severityForField(render_mode) = %q, want info (quiet; the rule carries the alert)", got)
	}
}

// TestProcessFetchPopulatesSegments guards A7 slice 3: when a SegmentsFor lookup
// is injected, every emitted alert Event (both the change-stream loop and the
// bridged rule-finding loop) carries the URL's segment names. The lookup is the
// in-memory registry seam — no DB read on the hot path.
func TestProcessFetchPopulatesSegments(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		// A bridged rule finding (no diff) so the bridged-finding path also runs.
		newFindings: []NewFinding{{Field: "http_status", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/blog/p", Importance: 1.0}
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "New", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}

	var gotSiteID int64
	var gotURL string
	seam := func(siteID int64, url string) []string {
		gotSiteID, gotURL = siteID, url
		return []string{"content", "featured"}
	}
	p := NewProcessor(deps, 4, func() time.Time { return now }, WithSegmentsFor(seam))
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	if gotSiteID != 1 || gotURL != "https://ex.com/blog/p" {
		t.Errorf("SegmentsFor called with (%d,%q), want (1,https://ex.com/blog/p)", gotSiteID, gotURL)
	}
	if len(deps.ingested) == 0 {
		t.Fatal("expected at least one ingested event")
	}
	for _, e := range deps.ingested {
		if len(e.Segments) != 2 || e.Segments[0] != "content" || e.Segments[1] != "featured" {
			t.Errorf("event %q segments = %v, want [content featured]", e.ChangeType, e.Segments)
		}
	}
}

// TestProcessFetchNilSegmentsForSafe confirms a Processor with no SegmentsFor
// seam (the default) emits events with nil Segments and never panics.
func TestProcessFetchNilSegmentsForSafe(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "New", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, e := range deps.ingested {
		if e.Segments != nil {
			t.Errorf("event %q should have nil segments without a SegmentsFor seam, got %v", e.ChangeType, e.Segments)
		}
	}
}

// TestProcessFetchIngestBestEffort guards F17: when a mid-loop IngestEvent fails on
// one change of a multi-change batch, the processor must still attempt the REMAINING
// events (the schedule advances and the new snapshot is already stored, so the next
// crawl re-diffs against it and would emit no change — an un-ingested event is never
// retried). It must also surface the failure (joined) so the crawl is recorded failed.
func TestProcessFetchIngestBestEffort(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	boom := errors.New("ingest boom")
	deps := &fakeProcDeps{ingestErrOn: map[string]error{"title": boom}}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// Multiple substantive fields differ -> multiple ingest events. Fail on "title"
	// but the others (meta_robots, meta_description) must still be attempted.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", MetaRobots: "index", MetaDescription: "old desc", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "New", MetaRobots: "noindex", MetaDescription: "new desc", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	_, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false)
	if !errors.Is(err, boom) {
		t.Fatalf("ProcessFetch must surface the failed-ingest error (joined), got %v", err)
	}
	// Every differing field must have been attempted, not just up to the failure.
	gotFields := map[string]bool{}
	for _, e := range deps.ingested {
		gotFields[e.ChangeType] = true
	}
	for _, want := range []string{"title", "meta_robots", "meta_description"} {
		if !gotFields[want] {
			t.Errorf("event %q was not ingested; the loop must not stop at the first failure (got %v)", want, gotFields)
		}
	}
}

// TestProcessFetchTruncatedSuppressesContentDiff guards the Med truncated-body
// finding: a body that exceeded the fetcher's size cap is a PREFIX of the real page,
// so its content hash is a shifted fragment. Diffing it against the prior full content
// would emit a spurious 'content' change (or hide a real one). When truncated=true the
// processor must NOT record or ingest a 'content' change, while still handling
// non-content fields (title/etc., which live in the recoverable <head> prefix).
func TestProcessFetchTruncatedSuppressesContentDiff(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// Content hash differs (prefix vs full) AND title differs. Only the content diff
	// must be suppressed by the truncated flag; the title change still flows through.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", ContentSHA256: "fullhash", ContentSimhash: 0xFF, Indexable: true, HTTPStatus: 200}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "New", ContentSHA256: "prefixhash", ContentSimhash: 0x0F, Indexable: true, HTTPStatus: 200}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", true); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, c := range deps.recorded {
		if c.Field == "content" {
			t.Errorf("a truncated body must NOT record a 'content' change (shifted prefix): %+v", c)
		}
	}
	for _, e := range deps.ingested {
		if e.ChangeType == "content" {
			t.Errorf("a truncated body must NOT ingest a 'content' alert event: %+v", e)
		}
	}
	// The non-content field (title) must still be handled — a truncated body still
	// carries a valid <head> prefix, so SEO head fields are honest.
	gotTitle := false
	for _, e := range deps.ingested {
		if e.ChangeType == "title" {
			gotTitle = true
		}
	}
	if !gotTitle {
		t.Errorf("non-content fields (title) must still be ingested on a truncated body; got %+v", deps.ingested)
	}
}

// TestProcessFetchBridgesNewFinding guards Feature A: a rule finding newly opened
// this crawl (e.g. broken_links_spike on internal_link_count) does NOT correspond to
// any change-stream alert event (internal_link_count is skipped from the diff loop),
// so without bridging it never reaches Slack. ProcessFetch must emit an alerts.Event
// for each newly-opened critical/warning finding, with ChangeType = the finding's
// field and Severity = the finding's severity.
func TestProcessFetchBridgesNewFinding(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{Field: "internal_link_count", Severity: model.SeverityWarning}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A link-count drop: recorded as a change but the diff loop SKIPS internal_link_count,
	// so the only path to an alert is the rule bridge.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", InternalLinkCount: 100, Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", InternalLinkCount: 40, Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	found := false
	for _, e := range deps.ingested {
		if e.ChangeType == "internal_link_count" {
			found = true
			if e.Severity != model.SeverityWarning {
				t.Errorf("bridged finding severity = %q, want warning", e.Severity)
			}
			if e.URL != u.URL || e.SiteID != site.ID || e.Site != site.BaseURL {
				t.Errorf("bridged event must carry url/site: %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("a newly-opened broken_links_spike finding must be bridged to an alert event (internal_link_count); got %+v", deps.ingested)
	}
}

// TestProcessFetchBridgedFindingCarriesDetail guards the A3 NewFinding.Detail seam:
// a newly-opened finding bridged to Slack carries the issue's Detail JSON in
// Event.After, so the alert body can show the measured-px numbers (or any rule's
// detail). title_pixel_overflow is unmapped in the bridge, so it bridges under its
// own change_type with no change-stream event to dedup against.
func TestProcessFetchBridgedFindingCarriesDetail(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	const detail = `{"measured_px":906,"budget_px":580,"chars":48}`
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{
			Field: "title_pixel_overflow", Severity: model.SeverityWarning, Detail: detail,
		}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A prior snapshot exists (not a first crawl) so the warning bridges, and the
	// title field actually changed so the overflow push is legitimate.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Short", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "A Much Wider Title", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	found := false
	for _, e := range deps.ingested {
		if e.ChangeType == "title_pixel_overflow" {
			found = true
			if e.After != detail {
				t.Errorf("bridged Event.After = %q, want the issue detail %q", e.After, detail)
			}
			if e.Severity != model.SeverityWarning {
				t.Errorf("bridged severity = %q, want warning", e.Severity)
			}
		}
	}
	if !found {
		t.Errorf("expected a bridged title_pixel_overflow event carrying detail; got %+v", deps.ingested)
	}
}

// TestProcessFetchBridgesFirstCrawlBroken guards Feature A for the no-diff case: a
// page broken on its FIRST crawl (oldSnap is the zero snapshot, so diff.Compare emits
// nothing) still opens an issue via ApplyRules. With no diff there is no change-stream
// event, so the rule bridge is the only path to Slack.
func TestProcessFetchBridgesFirstCrawlBroken(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{Field: "http_status", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// First crawl: no prior snapshot (zero oldSnap). A 500 opens status_regression.
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 500, Indexable: false, ContentSHA256: "a"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	found := false
	for _, e := range deps.ingested {
		if e.ChangeType == "http_status" && e.Severity == model.SeverityCritical {
			found = true
		}
	}
	if !found {
		t.Errorf("a first-crawl-broken finding (no diff) must be bridged to a critical alert; got %+v", deps.ingested)
	}
}

// TestProcessFetchBridgeDedupNoDoubleAlert guards Feature A's dedup invariant: a field
// that trips BOTH a change-stream event AND a rule finding (same change_type) must fire
// EXACTLY ONCE. The change-stream loop already ingested it, so the bridge must skip it.
func TestProcessFetchBridgeDedupNoDoubleAlert(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// indexable is emitted by the change-stream loop (the flip is in the diff) AND
	// reported as a newly-opened finding — it must be ingested once, not twice.
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{Field: "indexable", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// indexable flips true->false: a diff field AND a rule finding on the same key.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, IndexabilityReason: "indexable", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: false, IndexabilityReason: "meta noindex", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	count := 0
	for _, e := range deps.ingested {
		if e.ChangeType == "indexable" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("indexable must be ingested EXACTLY once (change-stream + bridge dedup), got %d: %+v", count, deps.ingested)
	}
}

// TestProcessFetchCollapsesNoindexTriad guards Feature C: a page going noindex flips
// three diff fields in one crawl (meta_robots, indexable, indexability_reason), each
// mapped Critical. Without collapsing, that is THREE Slack alerts for one root cause.
// ProcessFetch must emit ONLY the canonical `indexable` event and SKIP the standalone
// meta_robots and indexability_reason events for this crawl.
func TestProcessFetchCollapsesNoindexTriad(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// noindex shipped: indexable flips, meta_robots changes, indexability_reason changes.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", MetaRobots: "index,follow", Indexable: true, IndexabilityReason: "indexable", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", MetaRobots: "noindex,follow", Indexable: false, IndexabilityReason: "meta noindex", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	var idx, robots, reason int
	for _, e := range deps.ingested {
		switch e.ChangeType {
		case "indexable":
			idx++
		case "meta_robots":
			robots++
		case "indexability_reason":
			reason++
		}
	}
	if idx != 1 {
		t.Errorf("the noindex triad must collapse to exactly one canonical indexable alert, got %d", idx)
	}
	if robots != 0 {
		t.Errorf("meta_robots must be suppressed when indexable also flipped, got %d events", robots)
	}
	if reason != 0 {
		t.Errorf("indexability_reason must be suppressed when indexable also flipped, got %d events", reason)
	}
	// The canonical indexable alert should fold the reason into its body so the
	// root cause is not lost.
	for _, e := range deps.ingested {
		if e.ChangeType == "indexable" && e.After == "" {
			t.Errorf("the canonical indexable alert should carry context (reason folded in); got empty After")
		}
	}
}

// TestProcessFetchMetaRobotsAloneStillAlerts guards Feature C's precision: a meta_robots
// change that flips the noindex verdict but WITHOUT an indexable flip in this crawl must
// STILL alert on meta_robots — the triad collapse only applies when indexable is in the
// change set. The page is already non-indexable from another cause (an X-Robots noindex),
// so adding meta noindex flips the meta_robots verdict (so fix #2's benign-value gate lets
// it through) without producing an `indexable` diff (so there is no triad to collapse).
func TestProcessFetchMetaRobotsAloneStillAlerts(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// meta_robots gains noindex (verdict flips) but indexable stays false both crawls
	// (already out via X-Robots), so there is no `indexable` change to collapse against.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", MetaRobots: "index", XRobotsTag: "noindex", Indexable: false, IndexabilityReason: "x-robots noindex", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", MetaRobots: "noindex", XRobotsTag: "noindex", Indexable: false, IndexabilityReason: "x-robots noindex", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	robots := false
	for _, e := range deps.ingested {
		if e.ChangeType == "meta_robots" {
			robots = true
		}
	}
	if !robots {
		t.Errorf("a meta_robots verdict flip WITHOUT an indexable flip must still alert on meta_robots; got %+v", deps.ingested)
	}
}

// TestProcessFetchOverflowPushGate guards A3's anti-stampede push gate: an
// overflow finding (title_pixel_overflow / meta_description_pixel_overflow,
// unmapped so it bridges under its own change_type) must reach Slack ONLY when its
// SOURCE field changed this crawl. Without the gate, the first recheck after an
// upgrade would newly-open issues for every pre-existing long title and page the
// whole fleet. The issue still opens (engine's job); only the push is gated.
//
// Covers acceptance criterion 8: (a) title changed INTO overflow → two ingested
// types {title, title_pixel_overflow}; (b) pre-existing overflow, no title change
// (the upgrade case) → no overflow event; (c) first crawl → no overflow event.
func TestProcessFetchOverflowPushGate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	ingestedTitleOverflow := func(deps *fakeProcDeps) bool {
		for _, e := range deps.ingested {
			if e.ChangeType == "title_pixel_overflow" {
				return true
			}
		}
		return false
	}

	// (a) The title CHANGED this crawl and the new title overflows. Both facts page:
	// the title-change alert AND the overflow alert (two distinct change_types).
	t.Run("title_changed_into_overflow_pushes_both", func(t *testing.T) {
		deps := &fakeProcDeps{
			newFindings: []NewFinding{{
				Field: "title_pixel_overflow", Severity: model.SeverityWarning,
				Detail: `{"measured_px":906,"budget_px":580,"chars":48}`,
			}},
		}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "Short", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "A Brand New Much Wider Overflowing Title", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		got := map[string]bool{}
		for _, e := range deps.ingested {
			got[e.ChangeType] = true
		}
		if !got["title"] {
			t.Errorf("the title change itself must alert; got %v", got)
		}
		if !got["title_pixel_overflow"] {
			t.Errorf("a title changed INTO overflow must push the overflow alert too; got %v", got)
		}
	})

	// (b) The upgrade case: a pre-existing long title that did NOT change this crawl.
	// The overflow finding is newly opened (Rabbot just gained the rule), but pushing
	// it would stampede Slack across the fleet. It must NOT bridge — the issue stays
	// visible on pull/agent surfaces, but no page.
	t.Run("preexisting_overflow_no_title_change_does_not_push", func(t *testing.T) {
		deps := &fakeProcDeps{
			newFindings: []NewFinding{{
				Field: "title_pixel_overflow", Severity: model.SeverityWarning,
				Detail: `{"measured_px":906,"budget_px":580,"chars":48}`,
			}},
		}
		// Same (already-overflowing) title on both crawls: the title field did NOT change.
		// Something else changed (meta_description) so the crawl still runs the bridge loop.
		const longTitle = "A Brand New Much Wider Overflowing Title"
		old := model.Snapshot{ID: 1, URLID: 5, Title: longTitle, MetaDescription: "old", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: longTitle, MetaDescription: "new", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if ingestedTitleOverflow(deps) {
			t.Errorf("a pre-existing overflow with no title change must NOT push (anti-stampede); got %+v", deps.ingested)
		}
	})

	// (c) First crawl with an overflowing title: no prior baseline. The firstCrawl
	// guard already suppresses non-http_status findings, and the push gate (no title
	// CHANGE on a first crawl) reinforces it — either way, no overflow event.
	t.Run("first_crawl_overflow_does_not_push", func(t *testing.T) {
		deps := &fakeProcDeps{
			newFindings: []NewFinding{{
				Field: "title_pixel_overflow", Severity: model.SeverityWarning,
				Detail: `{"measured_px":906,"budget_px":580,"chars":48}`,
			}},
		}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "A Brand New Much Wider Overflowing Title", Indexable: true, HTTPStatus: 200, ContentSHA256: "a"}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, model.Snapshot{}, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if ingestedTitleOverflow(deps) {
			t.Errorf("a first-crawl overflow must NOT push; got %+v", deps.ingested)
		}
	})

	// The description rule shares the gate: a meta_description edited into overflow
	// pushes; an unchanged pre-existing one does not. One case is enough to pin that
	// the gate's source-field map covers meta_description_pixel_overflow too.
	t.Run("description_changed_into_overflow_pushes", func(t *testing.T) {
		deps := &fakeProcDeps{
			newFindings: []NewFinding{{
				Field: "meta_description_pixel_overflow", Severity: model.SeverityWarning,
				Detail: `{"measured_px":1100,"budget_px":920,"chars":200}`,
			}},
		}
		old := model.Snapshot{ID: 1, URLID: 5, MetaDescription: "short", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, MetaDescription: "a brand new much wider overflowing description", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		found := false
		for _, e := range deps.ingested {
			if e.ChangeType == "meta_description_pixel_overflow" {
				found = true
			}
		}
		if !found {
			t.Errorf("a meta_description edited INTO overflow must push the overflow alert; got %+v", deps.ingested)
		}
	})
}

// TestProcessFetchRedirectChainNoStandaloneAlert guards A5 scope item 5 (acceptance
// criterion 6): the opaque standalone redirect_chain alert is retired. A
// redirect_chain diff is still recorded as history (substantive change row, pinned in
// diff_test.go) but the scheduler's ingest loop must SKIP it — exactly like
// internal_link_count — so chain churn that neither grows nor loops no longer pages.
// Parsed growth/loop alerting is owned by the redirect_chain_growth / redirect_loop
// rules, bridged via Feature A.
func TestProcessFetchRedirectChainNoStandaloneAlert(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// The redirect chain churns (a→b becomes a→c) but neither grows nor loops, and
	// nothing else changes. Pre-retirement this emitted a noisy opaque redirect_chain
	// warning; now it must emit NO alert event at all.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b"]`, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/c"]`, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	// History is still recorded (the diff row), but it must NOT be ingested as an alert.
	recordedChain := false
	for _, c := range deps.recorded {
		if c.Field == "redirect_chain" {
			recordedChain = true
		}
	}
	if !recordedChain {
		t.Errorf("redirect_chain history must still be recorded as a change row; got %+v", deps.recorded)
	}
	for _, e := range deps.ingested {
		if e.ChangeType == "redirect_chain" {
			t.Errorf("the standalone redirect_chain alert is retired; it must NOT be ingested (got %+v)", e)
		}
	}
}

// TestProcessFetchRedirectLoopRecovery guards A5 scope item 6 (acceptance criterion
// 6): when a redirect chain that LOOPED on the prior crawl is CLEAN on this one,
// resolveHealthyFields must emit a redirect_loop resolve so the critical incident
// closes on recovery rather than waiting for the 24h auto-close sweep — mirroring
// indexable / http_status. It must NOT resolve when the old chain did not loop (no
// open incident to close) — that would be a spurious resolve.
func TestProcessFetchRedirectLoopRecovery(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	resolvedRedirectLoop := func(deps *fakeProcDeps) (found bool, sev model.Severity) {
		for _, e := range deps.resolved {
			if e.ChangeType == "redirect_loop" {
				return true, e.Severity
			}
		}
		return false, ""
	}

	// Old chain loops (A->B->A), new chain is clean (A->B): the loop recovered.
	t.Run("old_loops_new_clean_resolves", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b","https://ex.com/a"]`, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b"]`, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		found, sev := resolvedRedirectLoop(deps)
		if !found {
			t.Fatalf("a recovered redirect loop (old loops, new clean) must emit a redirect_loop resolve; got %+v", deps.resolved)
		}
		if sev != model.SeverityCritical {
			t.Errorf("redirect_loop resolve severity = %q, want critical", sev)
		}
	})

	// Old chain was already clean: nothing to recover, no spurious resolve.
	t.Run("old_clean_new_clean_no_resolve", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b"]`, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/c"]`, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if found, _ := resolvedRedirectLoop(deps); found {
			t.Errorf("no prior loop means no redirect_loop incident to resolve; got a spurious resolve %+v", deps.resolved)
		}
	})

	// Still looping (old loops, new also loops): the incident stays open, no resolve.
	t.Run("old_loops_new_still_loops_no_resolve", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b","https://ex.com/a"]`, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/c","https://ex.com/a"]`, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if found, _ := resolvedRedirectLoop(deps); found {
			t.Errorf("a chain that still loops must NOT resolve the redirect_loop incident; got %+v", deps.resolved)
		}
	})
}

// TestProcessFetchInfoFindingNeverPushes pins A5 acceptance criterion 6's info-tier
// clause: a newly-opened INFO finding (image_alt_missing is the first info-tier rule,
// landing on a seam built for it) must NEVER reach IngestEvent — info findings open an
// issue (queryable via `rabbot issues`) but never page Slack. The bridge loop's severity
// guard (only critical/warning bridge) is the seam; this test would catch a regression
// that let info findings stampede the push path. A warning finding in the SAME batch must
// still bridge, proving the guard filters by severity rather than dropping the whole batch.
func TestProcessFetchInfoFindingNeverPushes(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		newFindings: []NewFinding{
			{Field: "missing_alt_count", Severity: model.SeverityInfo, Detail: `{"images":10,"missing":3}`},
			{Field: "external_link_count", Severity: model.SeverityWarning, Detail: `{"old":5,"new":60}`},
		},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A real prior snapshot (not a first crawl) so the warning is eligible to bridge.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	var infoCount, warnCount int
	for _, e := range deps.ingested {
		switch e.ChangeType {
		case "missing_alt_count":
			infoCount++
		case "external_link_count":
			warnCount++
		}
	}
	if infoCount != 0 {
		t.Errorf("an info-tier finding (image_alt_missing -> missing_alt_count) must NEVER be ingested; got %d events", infoCount)
	}
	if warnCount != 1 {
		t.Errorf("a warning finding in the same batch must still bridge exactly once; got %d", warnCount)
	}
}

// TestProcessFetchRedirectChainGrowthBridges pins the one A5 rule whose ONLY path
// to Slack is the rule bridge: redirect_chain_growth maps to the `redirect_chain`
// change_type (rules_bridge.go), which the change-stream ingest loop deliberately
// SKIPS (process.go: redirect_chain is retired as a standalone alert). So a growth
// finding can reach Slack ONLY by bridging. This test proves two things at once:
//  1. the redirect_chain skip-list does NOT block the bridged growth alert (the skip
//     happens before ingestedTypes is set, so the bridge is not deduped away); and
//  2. dedup keeps it to EXACTLY one event under change_type "redirect_chain".
//
// The crawl pair also diffs redirect_chain (a→b grows to a→b→c) so the skip path is
// genuinely exercised — a regression that started ingesting (or that set
// ingestedTypes for) the skipped redirect_chain would either double-fire or swallow
// the bridged growth alert, and this test would catch both.
func TestProcessFetchRedirectChainGrowthBridges(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		newFindings: []NewFinding{
			{Field: "redirect_chain", Severity: model.SeverityWarning, Detail: `{"old":1,"new":2}`},
		},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A real prior snapshot (not a first crawl) whose redirect_chain also diffs and
	// grows but does NOT loop — exactly the redirect_chain_growth scenario.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b"]`, ContentSHA256: "a", ContentSimhash: 0x01}
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b","https://ex.com/c"]`, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	var chainEvents []alerts.Event
	for _, e := range deps.ingested {
		if e.ChangeType == "redirect_chain" {
			chainEvents = append(chainEvents, e)
		}
	}
	if len(chainEvents) != 1 {
		t.Fatalf("a newly-opened redirect_chain_growth finding must bridge EXACTLY one redirect_chain event (the skip-list must not block or double it); got %d, ingested=%+v", len(chainEvents), deps.ingested)
	}
	// The single event MUST be the bridged growth finding, not a resurrected standalone
	// redirect_chain alert: the bridge carries the finding's Detail in After, whereas the
	// retired change-stream alert carried the raw chain JSON in Before/After. Pinning
	// After == the finding Detail (and Before == "") proves the alert came through the
	// rule bridge and that the change-stream skip held (a regression that re-ingested the
	// standalone alert would set Before to the old chain JSON and fail here).
	got := chainEvents[0]
	if got.After != `{"old":1,"new":2}` || got.Before != "" {
		t.Errorf("the redirect_chain event must be the BRIDGED growth finding (After=Detail, Before=\"\"); got Before=%q After=%q", got.Before, got.After)
	}
	if got.Severity != model.SeverityWarning {
		t.Errorf("bridged redirect_chain_growth must be warning; got %q", got.Severity)
	}
}

// TestProcessFetchFirstCrawlRedirectLoopSilent documents the deliberate first-crawl
// silence for redirect_loop (A5 design "first-crawl findings other than http_status
// never push"). redirectLoopRule has no Old.ID guard, so a site whose VERY FIRST
// crawl observes a within-cap A→B→A loop opens a CRITICAL redirect_loop issue — but
// the first-crawl bridge guard (only http_status pages on a first crawl) suppresses
// the page. Net: the critical loop is tracked in `rabbot issues` yet never reaches
// Slack on a first crawl. This is intentional (a loop with no baseline is
// indistinguishable from a site that simply redirects that way), but it is subtle —
// "critical => always pages" would be wrong here. This test pins the silence so a
// reviewer (or a future change to the first-crawl exception list) sees it explicitly.
func TestProcessFetchFirstCrawlRedirectLoopSilent(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		// The engine would open this critical finding on a first crawl (no Old.ID guard
		// on redirect_loop); the bridge is what decides whether it pages.
		newFindings: []NewFinding{
			{Field: "redirect_loop", Severity: model.SeverityCritical, Detail: `{"repeated":"https://ex.com/a","depth":2}`},
		},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// First crawl: oldSnap is the zero snapshot (ID 0). The new chain loops A→B→A.
	new := model.Snapshot{ID: 2, URLID: 5, Title: "T", Indexable: true, HTTPStatus: 200, RedirectChain: `["https://ex.com/a","https://ex.com/b","https://ex.com/a"]`, ContentSHA256: "a"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, new, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, e := range deps.ingested {
		if e.ChangeType == "redirect_loop" {
			t.Errorf("a first-crawl redirect_loop must NOT page (first-crawl guard suppresses all but http_status); got %+v", e)
		}
	}
}

func TestProcessFetchHardBlockSuppresses(t *testing.T) {
	now := time.Now()
	deps := &fakeProcDeps{}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	site := model.Site{ID: 1, BaseURL: "https://ex.com"}

	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, model.Snapshot{}, model.Snapshot{}, model.FetchHardBlock, "cloudflare", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	if len(deps.access) != 1 || deps.access[0].FetchClass != model.FetchHardBlock {
		t.Errorf("access gate not invoked with hard_block: %+v", deps.access)
	}
	if len(deps.recorded) != 0 || len(deps.applied) != 0 {
		t.Errorf("SEO diff/rules must be suppressed on hard_block (recorded=%d applied=%d)", len(deps.recorded), len(deps.applied))
	}
}

// TestSeverityForHreflang guards fix #1: hreflang is a bare set-change detector with
// NO validity check — #16 already killed its critical RULE tier (hreflang_invalid
// emits WARNING). The change-stream severity must agree, so severityForField("hreflang")
// routes WARNING (alongside title / meta_description / schema_types), not critical.
// Routing it critical made an hreflang change page CRITICAL while the rule stayed
// WARNING, and the bridge dedup then swallowed the hreflang_invalid finding.
func TestSeverityForHreflang(t *testing.T) {
	if got := severityForField("hreflang"); got != model.SeverityWarning {
		t.Errorf("severityForField(hreflang) = %q, want warning (bare set-change; rule emits warning)", got)
	}
}

// TestProcessFetchBenignRobotsValueChangeNoCriticalAlert guards fix #2: a benign
// meta_robots / x_robots_tag VALUE change (adding max-image-preview / max-snippet /
// nofollow — NOT a deindex) must NOT page critical via the change stream, because the
// noindex verdict did not change (metaRobotsNoindexRule correctly stays silent). The
// standalone change-stream event fires ONLY when robotsmeta.IsNoindex flipped. A real
// index -> noindex flip still alerts.
func TestProcessFetchBenignRobotsValueChangeNoCriticalAlert(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	ingestedField := func(deps *fakeProcDeps, field string) bool {
		for _, e := range deps.ingested {
			if e.ChangeType == field {
				return true
			}
		}
		return false
	}

	// (a) meta_robots benign value change: index,follow -> index,follow,max-image-preview:large.
	// IsNoindex stays false on both sides — the verdict did not change, so NO meta_robots event.
	t.Run("meta_robots_benign_value_change_suppressed", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", MetaRobots: "index,follow", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", MetaRobots: "index,follow,max-image-preview:large", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if ingestedField(deps, "meta_robots") {
			t.Errorf("a benign meta_robots value change (verdict unchanged) must NOT page critical; got %+v", deps.ingested)
		}
	})

	// (b) x_robots_tag benign value change: same gate applies to the header field.
	t.Run("x_robots_benign_value_change_suppressed", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", XRobotsTag: "index", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", XRobotsTag: "index, max-snippet:-1", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if ingestedField(deps, "x_robots_tag") {
			t.Errorf("a benign x_robots_tag value change (verdict unchanged) must NOT page critical; got %+v", deps.ingested)
		}
	})

	// (c) a genuine deindex (index -> noindex) flips the verdict and MUST still alert.
	// Indexable stays true here (no triad collapse) to isolate the meta_robots event.
	t.Run("meta_robots_real_noindex_still_alerts", func(t *testing.T) {
		deps := &fakeProcDeps{}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", MetaRobots: "index", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", MetaRobots: "noindex", Indexable: true, HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		if !ingestedField(deps, "meta_robots") {
			t.Errorf("a real index->noindex (verdict flipped) must still alert on meta_robots; got %+v", deps.ingested)
		}
	})
}

// TestProcessFetchStatusFanOutCollapse guards fix #3: a 2xx -> 404/5xx flips
// http_status AND (typically) indexable and canonical in one crawl, which without
// collapsing fans out into 2-3 separate CRITICAL alerts for one root cause (the page
// is gone). When http_status is in the change set and the new status is non-2xx,
// ProcessFetch must emit ONLY the http_status critical and fold away the standalone
// indexable + canonical change-stream events AND the bridged indexability_flip /
// canonical_changed findings.
func TestProcessFetchStatusFanOutCollapse(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	t.Run("2xx_to_404_yields_exactly_one_critical", func(t *testing.T) {
		// The rules engine also opens the transition findings for this crawl; they must
		// be folded by the bridge, not re-introduced as separate alerts.
		deps := &fakeProcDeps{
			newFindings: []NewFinding{
				{Field: "http_status", Severity: model.SeverityCritical},
				{Field: "indexable", Severity: model.SeverityCritical},
				{Field: "canonical", Severity: model.SeverityCritical},
			},
		}
		// 200 (indexable, self-canonical) -> 404 (not indexable, canonical blanked).
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, Canonical: "https://ex.com/p", IndexabilityReason: "indexable", ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 404, Indexable: false, Canonical: "", IndexabilityReason: "http 404", ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		var nCritical int
		counts := map[string]int{}
		for _, e := range deps.ingested {
			counts[e.ChangeType]++
			if e.Severity == model.SeverityCritical {
				nCritical++
			}
		}
		if nCritical != 1 {
			t.Errorf("a 2xx->404 must collapse to EXACTLY one critical incident, got %d: %+v", nCritical, deps.ingested)
		}
		if counts["http_status"] != 1 {
			t.Errorf("the one surviving critical must be http_status, got %v", counts)
		}
		if counts["indexable"] != 0 {
			t.Errorf("indexable must be folded into the http_status alert on a non-2xx status, got %d", counts["indexable"])
		}
		if counts["canonical"] != 0 {
			t.Errorf("canonical must be folded into the http_status alert on a non-2xx status, got %d", counts["canonical"])
		}
	})

	// An indexable flip WITHOUT a status regression (status stays 2xx) must NOT be
	// folded — it still pages on its own. Guards the precision of fix #3.
	t.Run("indexable_flip_without_status_regression_still_pages", func(t *testing.T) {
		deps := &fakeProcDeps{}
		// 200 -> 200 but meta noindex shipped: indexable flips, http_status does NOT change.
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, MetaRobots: "index", IndexabilityReason: "indexable", ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: false, MetaRobots: "noindex", IndexabilityReason: "meta noindex", ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		idx := 0
		for _, e := range deps.ingested {
			if e.ChangeType == "indexable" {
				idx++
			}
		}
		if idx != 1 {
			t.Errorf("an indexable flip with NO status regression must still page once on indexable, got %d: %+v", idx, deps.ingested)
		}
	})

	// A 3xx redirect (200->301) must NOT collapse: it is not an error-tier regression, so a
	// canonical/indexability change on that crawl must still surface. Guards fix #2 (the
	// collapse predicate is >=400, not >=300 — a 301 would otherwise swallow them).
	t.Run("2xx_to_301_redirect_does_not_collapse", func(t *testing.T) {
		deps := &fakeProcDeps{
			newFindings: []NewFinding{
				{Field: "indexable", Severity: model.SeverityCritical},
				{Field: "canonical", Severity: model.SeverityCritical},
			},
		}
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, Canonical: "https://ex.com/p", IndexabilityReason: "indexable", ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 301, Indexable: false, Canonical: "https://ex.com/other", IndexabilityReason: "redirect", ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		counts := map[string]int{}
		for _, e := range deps.ingested {
			counts[e.ChangeType]++
		}
		if counts["indexable"] != 1 || counts["canonical"] != 1 {
			t.Errorf("a 200->301 must NOT collapse canonical/indexability (3xx is not error-tier), got %+v", deps.ingested)
		}
	})
}

// TestProcessFetchXRobotsNoindexCollapse guards fix #4: a header-driven (X-Robots-Tag)
// noindex flip changes x_robots_tag AND indexable AND indexability_reason in one crawl.
// The noindex-triad collapse must fold x_robots_tag too (it previously folded only
// meta_robots + indexability_reason), so a header-driven deindex pages ONCE on the
// canonical indexable alert, not twice (indexable + x_robots_tag). An x_robots_tag
// change WITHOUT an indexable flip still alerts on its own.
func TestProcessFetchXRobotsNoindexCollapse(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	t.Run("header_noindex_flip_collapses_to_one", func(t *testing.T) {
		// The bridge also reports the x_robots transition under meta_robots' field? No:
		// a header-driven noindex opens indexability_flip (-> indexable) which the triad
		// already folds; pass it so the bridge fold is exercised too.
		deps := &fakeProcDeps{
			newFindings: []NewFinding{{Field: "indexable", Severity: model.SeverityCritical}},
		}
		// X-Robots-Tag: noindex added; indexable flips; reason changes. meta_robots stays empty.
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", XRobotsTag: "", Indexable: true, IndexabilityReason: "indexable", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", XRobotsTag: "noindex", Indexable: false, IndexabilityReason: "x-robots noindex", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}

		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		counts := map[string]int{}
		for _, e := range deps.ingested {
			counts[e.ChangeType]++
		}
		if counts["indexable"] != 1 {
			t.Errorf("a header-driven noindex flip must collapse to one canonical indexable alert, got %d: %+v", counts["indexable"], deps.ingested)
		}
		if counts["x_robots_tag"] != 0 {
			t.Errorf("x_robots_tag must be folded into the indexable alert when indexable also flipped, got %d events", counts["x_robots_tag"])
		}
	})

	// An x_robots_tag change to a real noindex WITHOUT an indexable flip (e.g. another
	// signal already kept the page out, or the page stays indexable) still alerts on its
	// own — the collapse only applies when indexable is in the change set.
	t.Run("x_robots_change_without_indexable_flip_still_alerts", func(t *testing.T) {
		deps := &fakeProcDeps{}
		// x_robots_tag flips index->noindex (verdict change, so fix #2 lets it through),
		// but indexable stays true (isolating the no-collapse path).
		old := model.Snapshot{ID: 1, URLID: 5, Title: "T", XRobotsTag: "index", Indexable: true, IndexabilityReason: "indexable", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		new := model.Snapshot{ID: 2, URLID: 5, Title: "T", XRobotsTag: "noindex", Indexable: true, IndexabilityReason: "indexable", HTTPStatus: 200, ContentSHA256: "a", ContentSimhash: 0x01}
		p := NewProcessor(deps, 4, func() time.Time { return now })
		if _, err := p.ProcessFetch(context.Background(), site, u, new, old, model.FetchOK, "", false); err != nil {
			t.Fatalf("ProcessFetch: %v", err)
		}
		got := false
		for _, e := range deps.ingested {
			if e.ChangeType == "x_robots_tag" {
				got = true
			}
		}
		if !got {
			t.Errorf("an x_robots_tag verdict change WITHOUT an indexable flip must still alert on x_robots_tag; got %+v", deps.ingested)
		}
	})
}
