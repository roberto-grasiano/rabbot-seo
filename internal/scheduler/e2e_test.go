package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// e2eDeps adapts the real store + engine + pipeline to ProcessorDeps for the
// test. Per the M2 seam contract there is no LatestSnapshot: the prior snapshot
// is passed into ProcessFetch explicitly (captured before the new one is saved).
type e2eDeps struct {
	store    *store.DB
	engine   *rules.Engine
	pipeline *alerts.Pipeline
}

func (d *e2eDeps) RecordChanges(ctx context.Context, c []model.Change) error {
	return d.store.RecordChanges(ctx, c)
}

func (d *e2eDeps) ApplyRules(ctx context.Context, urlID int64, imp float64, newSnap, old model.Snapshot, ch []model.Change, truncated bool) ([]NewFinding, error) {
	// Snapshot the rule_ids already open for this URL BEFORE applying, so we can
	// identify which findings the engine NEWLY opens (an already-open finding has
	// already alerted on the crawl it opened — re-bridging it would double-alert).
	urlIDp := urlID
	before, err := d.store.ListIssues(ctx, store.IssueFilter{URLID: &urlIDp, OpenOnly: true})
	if err != nil {
		return nil, err
	}
	wasOpen := make(map[string]bool, len(before))
	for _, iss := range before {
		wasOpen[iss.RuleID] = true
	}
	if err := d.engine.Apply(ctx, rules.EvalContext{URLID: urlID, Importance: imp, New: newSnap, Old: old, Changes: ch, Truncated: truncated}); err != nil {
		return nil, err
	}
	after, err := d.store.ListIssues(ctx, store.IssueFilter{URLID: &urlIDp, OpenOnly: true})
	if err != nil {
		return nil, err
	}
	var newly []NewFinding
	for _, iss := range after {
		if wasOpen[iss.RuleID] {
			continue // already open before this crawl
		}
		field, ok := BridgeFieldForRule(iss.RuleID)
		if !ok {
			field = iss.RuleID // unmapped rule: bridge under its own id
		}
		newly = append(newly, NewFinding{Field: field, Severity: iss.Severity, Detail: iss.Detail})
	}
	return newly, nil
}
func (d *e2eDeps) HandleFetchClass(ctx context.Context, ac alerts.AccessContext, seo []alerts.Event) (bool, error) {
	return d.pipeline.HandleFetchClass(ctx, ac, seo)
}
func (d *e2eDeps) IngestEvent(ctx context.Context, e alerts.Event) error {
	return d.pipeline.Ingest(ctx, e)
}
func (d *e2eDeps) ResolveEvent(ctx context.Context, e alerts.Event) error {
	return d.pipeline.Resolve(ctx, e)
}
func (d *e2eDeps) RecordHealthScore(ctx context.Context, siteID, urlID int64) error {
	return d.store.RecordHealthScores(ctx, siteID, urlID, time.Now())
}

func TestE2ENoindexRegressionFiresAndResolves(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Seed real parent rows — PRAGMA foreign_keys=ON means snapshots/issues/alerts
	// must reference an existing site + url.
	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://ex.com", Name: "Ex", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://ex.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: urlID, SiteID: siteID, URL: "https://ex.com/p", Importance: 1.0}

	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	dispatcher := notify.NewDispatcher(registry)
	pipeline := alerts.NewPipeline(st, dispatcher,
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// 1) Baseline: ok + indexable. No prior snapshot, so pass the zero Snapshot.
	good := model.Snapshot{
		URLID: urlID, Title: "T", Canonical: "https://ex.com/p", MetaRobots: "index,follow",
		HTTPStatus: 200, Indexable: true, IndexabilityReason: "indexable", Headings: `{"h1":["x"]}`,
		MetaDescription: "d", ContentSHA256: "v1", ContentSimhash: 0x01, FetchedAt: now,
	}
	id1, err := st.SaveSnapshot(ctx, good)
	if err != nil {
		t.Fatalf("SaveSnapshot baseline: %v", err)
	}
	good.ID = id1
	if _, err := proc.ProcessFetch(ctx, site, u, good, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch baseline: %v", err)
	}

	// 2) Regression: noindex shipped (indexable flips false). Prior = good.
	bad := good
	bad.MetaRobots = "noindex,follow"
	bad.Indexable = false
	bad.IndexabilityReason = "meta noindex"
	bad.ContentSHA256 = "v2"
	bad.ContentSimhash = 0x03
	id2, err := st.SaveSnapshot(ctx, bad)
	if err != nil {
		t.Fatalf("SaveSnapshot bad: %v", err)
	}
	bad.ID = id2
	changed, err := proc.ProcessFetch(ctx, site, u, bad, good, model.FetchOK, "", false)
	if err != nil {
		t.Fatalf("ProcessFetch regression: %v", err)
	}
	if !changed {
		t.Errorf("noindex regression is a substantive change; ProcessFetch must report changed=true (F24)")
	}

	if atomic.LoadInt32(&slackHits) == 0 {
		t.Fatalf("expected a Slack alert for the noindex regression")
	}
	open, _ := st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	foundIdx := false
	for _, iss := range open {
		if iss.RuleID == "indexability_flip" && iss.Severity == model.SeverityCritical {
			foundIdx = true
		}
	}
	if !foundIdx {
		t.Errorf("expected an open critical indexability_flip issue, got %+v", open)
	}
	openInc, _ := st.ListOpenIncidents(ctx)
	if len(openInc) == 0 {
		t.Errorf("expected an open incident for the regression")
	}

	// Feature C (end-to-end through the REAL engine + pipeline): a noindex flip trips
	// indexable + meta_robots + indexability_reason all critical, but ProcessFetch must
	// collapse the triad to ONE canonical critical alert on `indexable`. Each distinct
	// change_type opens its own group incident (fingerprint = site|change_type|severity),
	// so assert exactly one critical incident and that meta_robots / indexability_reason
	// did NOT open standalone critical incidents.
	var critIndexable, critMetaRobots, critReason int
	for _, inc := range openInc {
		if inc.Severity != model.SeverityCritical {
			continue
		}
		switch inc.GroupKey {
		case "https://ex.com|indexable":
			critIndexable++
		case "https://ex.com|meta_robots":
			critMetaRobots++
		case "https://ex.com|indexability_reason":
			critReason++
		}
	}
	if critIndexable != 1 {
		t.Errorf("noindex triad must collapse to one canonical critical indexable incident, got %d (incidents %+v)", critIndexable, openInc)
	}
	if critMetaRobots != 0 {
		t.Errorf("meta_robots must NOT open a standalone critical incident when indexable also flipped, got %d", critMetaRobots)
	}
	if critReason != 0 {
		t.Errorf("indexability_reason must NOT open a standalone critical incident when indexable also flipped, got %d", critReason)
	}

	// 3) Fix: noindex removed. Prior = bad → issue closes, incident resolves.
	fixed := bad
	fixed.MetaRobots = "index,follow"
	fixed.Indexable = true
	fixed.IndexabilityReason = "indexable"
	fixed.ContentSHA256 = "v3"
	fixed.ContentSimhash = 0x05
	id3, err := st.SaveSnapshot(ctx, fixed)
	if err != nil {
		t.Fatalf("SaveSnapshot fixed: %v", err)
	}
	fixed.ID = id3
	if _, err := proc.ProcessFetch(ctx, site, u, fixed, bad, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch fix: %v", err)
	}

	openAfter, _ := st.ListIssues(ctx, store.IssueFilter{OpenOnly: true})
	for _, iss := range openAfter {
		if iss.RuleID == "indexability_flip" && iss.Status == model.IssueOpen {
			t.Errorf("indexability_flip issue should be closed after fix, still open: %+v", iss)
		}
	}
}

// TestE2EBrokenLinksSpikeBridgesToSlack covers Feature A end-to-end through the REAL
// engine + pipeline + Slack sink: a sharp internal-link-count drop opens a
// broken_links_spike issue, but internal_link_count is SKIPPED by the change-stream
// alert loop, so without the rule bridge the spike never reaches Slack. ProcessFetch
// must bridge the newly-opened finding into the alert path, producing a Slack delivery
// and an open incident keyed on internal_link_count.
func TestE2EBrokenLinksSpikeBridgesToSlack(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://ex.com", Name: "Ex", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://ex.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: urlID, SiteID: siteID, URL: "https://ex.com/p", Importance: 1.0}

	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// 1) Baseline: ok, 100 internal links, indexable. No prior snapshot.
	good := model.Snapshot{
		URLID: urlID, Title: "T", Canonical: "https://ex.com/p", MetaRobots: "index,follow",
		HTTPStatus: 200, Indexable: true, IndexabilityReason: "indexable", Headings: `{"h1":["x"]}`,
		MetaDescription: "d", InternalLinkCount: 100, ContentSHA256: "v1", ContentSimhash: 0x01, FetchedAt: now,
	}
	id1, err := st.SaveSnapshot(ctx, good)
	if err != nil {
		t.Fatalf("SaveSnapshot baseline: %v", err)
	}
	good.ID = id1
	if _, err := proc.ProcessFetch(ctx, site, u, good, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch baseline: %v", err)
	}
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("clean baseline must not alert; slackHits=%d", got)
	}

	// 2) Spike: internal links collapse 100 -> 40 (a 60% drop). Everything else stable,
	// so the ONLY substantive signal is broken_links_spike — which the change-stream
	// loop skips. The bridge is the only path to Slack.
	bad := good
	bad.InternalLinkCount = 40
	bad.ContentSHA256 = "v2"
	bad.ContentSimhash = 0x01 // unchanged hash flips, but keep simhash same so content is the only other field
	id2, err := st.SaveSnapshot(ctx, bad)
	if err != nil {
		t.Fatalf("SaveSnapshot bad: %v", err)
	}
	bad.ID = id2
	if _, err := proc.ProcessFetch(ctx, site, u, bad, good, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch spike: %v", err)
	}

	// The broken_links_spike issue must be open...
	open, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	foundSpike := false
	for _, iss := range open {
		if iss.RuleID == "broken_links_spike" {
			foundSpike = true
		}
	}
	if !foundSpike {
		t.Fatalf("expected an open broken_links_spike issue, got %+v", open)
	}
	// ...and it must have reached Slack via the bridge.
	if got := atomic.LoadInt32(&slackHits); got == 0 {
		t.Fatalf("a broken_links_spike must be bridged to a Slack alert (internal_link_count is skipped by the change-stream loop); slackHits=0")
	}
	// The bridged incident is keyed on internal_link_count (the field the spike maps to).
	openInc, _ := st.ListOpenIncidents(ctx)
	foundInc := false
	for _, inc := range openInc {
		if inc.GroupKey == "https://ex.com|internal_link_count" {
			foundInc = true
		}
	}
	if !foundInc {
		t.Errorf("expected an open incident keyed on internal_link_count, got %+v", openInc)
	}
}

// TestE2ERedirectLoopFiresAndResolves covers A5's redirect_loop path end-to-end
// through the REAL engine + pipeline + Slack sink. A within-cap loop (A->B->A that
// ultimately resolved 200) opens a critical redirect_loop issue. redirect_loop is
// unmapped in the bridge, so it bridges under its own change_type — there is no
// redirect_chain alert (that opaque event is retired), so the rule bridge is the only
// path to Slack. On recovery (clean chain), resolveHealthyFields closes the incident
// immediately rather than waiting for the auto-close sweep.
func TestE2ERedirectLoopFiresAndResolves(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://ex.com", Name: "Ex", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://ex.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: urlID, SiteID: siteID, URL: "https://ex.com/p", Importance: 1.0}

	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// 1) Baseline: a clean single-hop chain, indexable. No prior snapshot.
	good := model.Snapshot{
		URLID: urlID, Title: "T", Canonical: "https://ex.com/p", MetaRobots: "index,follow",
		HTTPStatus: 200, Indexable: true, IndexabilityReason: "indexable", Headings: `{"h1":["x"]}`,
		MetaDescription: "d", RedirectChain: `["https://ex.com/a","https://ex.com/p"]`,
		ContentSHA256: "v1", ContentSimhash: 0x01, FetchedAt: now,
	}
	id1, err := st.SaveSnapshot(ctx, good)
	if err != nil {
		t.Fatalf("SaveSnapshot baseline: %v", err)
	}
	good.ID = id1
	if _, err := proc.ProcessFetch(ctx, site, u, good, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch baseline: %v", err)
	}
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("clean baseline must not alert; slackHits=%d", got)
	}

	// 2) Loop appears: the chain revisits /a (A->B->A) within the redirect cap. The
	// opaque redirect_chain alert is retired, so the ONLY path to Slack is the
	// redirect_loop rule, bridged under its own change_type at critical.
	loop := good
	loop.RedirectChain = `["https://ex.com/a","https://ex.com/b","https://ex.com/a"]`
	loop.ContentSHA256 = "v2"
	loop.ContentSimhash = 0x01
	id2, err := st.SaveSnapshot(ctx, loop)
	if err != nil {
		t.Fatalf("SaveSnapshot loop: %v", err)
	}
	loop.ID = id2
	if _, err := proc.ProcessFetch(ctx, site, u, loop, good, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch loop: %v", err)
	}

	// The redirect_loop issue must be open at critical.
	open, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	foundLoop := false
	for _, iss := range open {
		if iss.RuleID == "redirect_loop" && iss.Severity == model.SeverityCritical {
			foundLoop = true
		}
	}
	if !foundLoop {
		t.Fatalf("expected an open critical redirect_loop issue, got %+v", open)
	}
	// ...and it must have reached Slack via the bridge (no redirect_chain alert exists).
	if got := atomic.LoadInt32(&slackHits); got == 0 {
		t.Fatalf("a redirect_loop must be bridged to a Slack alert (the opaque redirect_chain alert is retired); slackHits=0")
	}
	// The bridged incident is keyed on redirect_loop (the rule bridges under its own id),
	// NOT redirect_chain.
	openInc, _ := st.ListOpenIncidents(ctx)
	foundInc := false
	for _, inc := range openInc {
		if inc.GroupKey == "https://ex.com|redirect_loop" {
			foundInc = true
		}
		if inc.GroupKey == "https://ex.com|redirect_chain" {
			t.Errorf("the opaque redirect_chain alert is retired; no redirect_chain incident should open, got %+v", inc)
		}
	}
	if !foundInc {
		t.Fatalf("expected an open incident keyed on redirect_loop, got %+v", openInc)
	}

	// 3) Recovery: the chain is clean again. The issue closes (engine) and the incident
	// resolves immediately via resolveHealthyFields (old loops && new clean).
	fixed := loop
	fixed.RedirectChain = `["https://ex.com/a","https://ex.com/p"]`
	fixed.ContentSHA256 = "v3"
	fixed.ContentSimhash = 0x01
	id3, err := st.SaveSnapshot(ctx, fixed)
	if err != nil {
		t.Fatalf("SaveSnapshot fixed: %v", err)
	}
	fixed.ID = id3
	if _, err := proc.ProcessFetch(ctx, site, u, fixed, loop, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch fix: %v", err)
	}

	openAfter, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	for _, iss := range openAfter {
		if iss.RuleID == "redirect_loop" && iss.Status == model.IssueOpen {
			t.Errorf("redirect_loop issue should be closed after the chain clears, still open: %+v", iss)
		}
	}
	openIncAfter, _ := st.ListOpenIncidents(ctx)
	for _, inc := range openIncAfter {
		if inc.GroupKey == "https://ex.com|redirect_loop" {
			t.Errorf("the redirect_loop incident should resolve on recovery (resolveHealthyFields), still open: %+v", inc)
		}
	}
}

// TestE2ETitlePixelOverflowFiresAndResolves covers A3 acceptance criterion 11 end-to-end
// through the REAL store + engine + pipeline + Slack sink: editing a fitting title into an
// overflowing one fires BOTH the title-change alert and the title_pixel_overflow alert
// (two distinct facts — it changed AND it no longer fits — neither deduped away because
// the overflow rule is unmapped and bridges under its own change_type), and the bridged
// overflow alert carries the measured-px Detail JSON. Reverting to a fitting title fires
// ONLY the title-change alert (no new overflow alert) and closes the overflow issue.
//
// A per-phase advancing clock steps past the dedup window between crawls so each phase's
// notifications are independently countable — that is what lets us assert "both fire once"
// on the edit and "only the title-change fires" on the revert (a frozen clock would dedup
// the second same-change_type `title` notification and muddy the count).
func TestE2ETitlePixelOverflowFiresAndResolves(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Advancing clock: each ProcessFetch phase is +1h, well past the 5-minute dedup
	// window, so same-change_type notifications across phases are not deduped together.
	var nowTick atomic.Int64
	clock := func() time.Time { return base.Add(time.Duration(nowTick.Load()) * time.Hour) }

	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://ex.com", Name: "Ex", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://ex.com/p", FirstSeen: base, NextCheckAt: base, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: urlID, SiteID: siteID, URL: "https://ex.com/p", Importance: 1.0}

	notifier := notify.NewSlackNotifier("slack-warning", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-warning": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-warning"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// A short title that FITS the 580px desktop budget, and a wide one (48×'W' ≈ 906px)
	// that OVERFLOWS it. serpwidth is the single owner of these numbers; the rule tests
	// pin the exact widths. Both pages are otherwise clean (indexable, canonical, etc.)
	// so the ONLY signals are the title change and the overflow.
	const fittingTitle = "Home"
	overflowingTitle := strings.Repeat("W", 48)

	mkSnap := func(title, sha string, simhash uint64) model.Snapshot {
		return model.Snapshot{
			URLID: urlID, Title: title, Canonical: "https://ex.com/p", MetaRobots: "index,follow",
			HTTPStatus: 200, Indexable: true, IndexabilityReason: "indexable", Headings: `{"h1":["x"]}`,
			MetaDescription: "d", ContentSHA256: sha, ContentSimhash: simhash, FetchedAt: clock(),
		}
	}

	// 1) Baseline: a fitting title. No prior snapshot, no alert, no overflow issue.
	nowTick.Store(0)
	good := mkSnap(fittingTitle, "v1", 0x01)
	id1, err := st.SaveSnapshot(ctx, good)
	if err != nil {
		t.Fatalf("SaveSnapshot baseline: %v", err)
	}
	good.ID = id1
	if _, err := proc.ProcessFetch(ctx, site, u, good, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch baseline: %v", err)
	}
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("a fitting baseline title must not alert; slackHits=%d", got)
	}
	openBase, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	for _, iss := range openBase {
		if iss.RuleID == "title_pixel_overflow" {
			t.Fatalf("a fitting title must not open a title_pixel_overflow issue, got %+v", iss)
		}
	}

	// 2) Edit the title into overflow. TWO distinct facts must page: the title CHANGED,
	// and it no longer FITS. The overflow rule is unmapped, so it bridges under its own
	// change_type and is NOT deduped against the `title` change-stream event.
	nowTick.Store(1)
	before2 := atomic.LoadInt32(&slackHits)
	bad := mkSnap(overflowingTitle, "v2", 0x03)
	bad.FetchedAt = clock()
	id2, err := st.SaveSnapshot(ctx, bad)
	if err != nil {
		t.Fatalf("SaveSnapshot overflow: %v", err)
	}
	bad.ID = id2
	if _, err := proc.ProcessFetch(ctx, site, u, bad, good, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch overflow: %v", err)
	}
	// Exactly TWO new Slack notifications this phase: the title change and the overflow.
	if delta := atomic.LoadInt32(&slackHits) - before2; delta != 2 {
		t.Fatalf("editing a fitting title into overflow must fire exactly two alerts (title changed + no-longer-fits); got %d new Slack hits", delta)
	}
	// The overflow issue is open at warning and carries the measured-px Detail JSON.
	openBad, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	var overflowIssue *model.Issue
	for i := range openBad {
		if openBad[i].RuleID == "title_pixel_overflow" {
			overflowIssue = &openBad[i]
		}
	}
	if overflowIssue == nil {
		t.Fatalf("expected an open title_pixel_overflow issue after the edit, got %+v", openBad)
	}
	if overflowIssue.Severity != model.SeverityWarning {
		t.Errorf("title_pixel_overflow severity = %q, want warning", overflowIssue.Severity)
	}
	// Detail must carry the measured px / budget px / char count behind the finding.
	var d struct {
		MeasuredPx int `json:"measured_px"`
		BudgetPx   int `json:"budget_px"`
		Chars      int `json:"chars"`
	}
	if err := json.Unmarshal([]byte(overflowIssue.Detail), &d); err != nil {
		t.Fatalf("title_pixel_overflow Detail %q does not carry measured-px JSON: %v", overflowIssue.Detail, err)
	}
	if d.BudgetPx != rules.TitleBudgetPx {
		t.Errorf("Detail budget_px = %d, want %d (TitleBudgetPx)", d.BudgetPx, rules.TitleBudgetPx)
	}
	if d.MeasuredPx <= d.BudgetPx {
		t.Errorf("Detail measured_px = %d must exceed budget_px = %d for an overflow", d.MeasuredPx, d.BudgetPx)
	}
	if d.Chars != len([]rune(overflowingTitle)) {
		t.Errorf("Detail chars = %d, want %d", d.Chars, len([]rune(overflowingTitle)))
	}
	// Two distinct incidents opened: site|title (the change) and site|title_pixel_overflow
	// (the overflow). The overflow MUST surface under its own change_type, not be folded
	// into `title`.
	openInc, _ := st.ListOpenIncidents(ctx)
	var incTitle, incOverflow int
	for _, inc := range openInc {
		switch inc.GroupKey {
		case "https://ex.com|title":
			incTitle++
		case "https://ex.com|title_pixel_overflow":
			incOverflow++
		}
	}
	if incTitle != 1 {
		t.Errorf("expected one title-change incident, got %d (incidents %+v)", incTitle, openInc)
	}
	if incOverflow != 1 {
		t.Errorf("expected one title_pixel_overflow incident (its own change_type), got %d (incidents %+v)", incOverflow, openInc)
	}

	// 3) Revert to a fitting title. ONLY the title-change alert fires this phase (the
	// title changed back) — NO new overflow alert — and the overflow issue closes.
	nowTick.Store(2)
	before3 := atomic.LoadInt32(&slackHits)
	overflowIncBefore3 := incOverflow
	fixed := mkSnap(fittingTitle, "v3", 0x05)
	fixed.FetchedAt = clock()
	id3, err := st.SaveSnapshot(ctx, fixed)
	if err != nil {
		t.Fatalf("SaveSnapshot revert: %v", err)
	}
	fixed.ID = id3
	if _, err := proc.ProcessFetch(ctx, site, u, fixed, bad, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch revert: %v", err)
	}
	// Exactly ONE new Slack notification this phase: the title change back. The overflow
	// rule now PASSES (the title fits), so it opens no new finding and pages nothing.
	if delta := atomic.LoadInt32(&slackHits) - before3; delta != 1 {
		t.Fatalf("reverting to a fitting title must fire exactly one alert (the title change); got %d new Slack hits", delta)
	}
	// The overflow issue must be closed (the title fits again).
	openFixed, _ := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	for _, iss := range openFixed {
		if iss.RuleID == "title_pixel_overflow" && iss.Status == model.IssueOpen {
			t.Errorf("title_pixel_overflow must close once the title fits again, still open: %+v", iss)
		}
	}
	// No SECOND overflow incident was opened on the revert (it fits, so it cannot fire).
	openIncFixed, _ := st.ListOpenIncidents(ctx)
	overflowIncAfter := 0
	for _, inc := range openIncFixed {
		if inc.GroupKey == "https://ex.com|title_pixel_overflow" {
			overflowIncAfter++
		}
	}
	if overflowIncAfter > overflowIncBefore3 {
		t.Errorf("the revert must not open a new title_pixel_overflow incident; before=%d after=%d", overflowIncBefore3, overflowIncAfter)
	}
}

// TestE2ENoDeadSignalRemains is A5 acceptance criterion 9 — the falsifiability check
// behind the "we were storing the answer the whole time" pipeline audit. With all five
// dormant-signal rules registered, ONE synthetic crawl pair that moves every previously-
// dead field (ImageCount, MissingAltCount, ExternalLinkCount, RedirectChain) must produce
// at least one change row OR issue referencing EACH of image_count, missing_alt_count,
// external_link_count, and redirect_chain end-to-end through the real store + engine +
// pipeline. If any of those signals were still dead (extracted/stored but never diffed or
// ruled on), its field name would be absent from the union below and the test fails.
func TestE2ENoDeadSignalRemains(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// A real Slack sink so the pipeline path is exercised, but this test asserts on the
	// stored changes/issues (the audit surfaces), not on Slack-hit counts.
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackSrv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: "https://ex.com", Name: "Ex", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://ex.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: urlID, SiteID: siteID, URL: "https://ex.com/p", Importance: 1.0}

	notifier := notify.NewSlackNotifier("slack-all", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-all": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-all"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// 1) Baseline: a clean single-hop chain, 10 images all with alt (full coverage),
	// 5 external links. No prior snapshot.
	good := model.Snapshot{
		URLID: urlID, Title: "T", Canonical: "https://ex.com/p", MetaRobots: "index,follow",
		HTTPStatus: 200, Indexable: true, IndexabilityReason: "indexable", Headings: `{"h1":["x"]}`,
		MetaDescription: "d", RedirectChain: `["https://ex.com/a","https://ex.com/p"]`,
		ImageCount: 10, MissingAltCount: 0, ExternalLinkCount: 5,
		ContentSHA256: "v1", ContentSimhash: 0x01, FetchedAt: now,
	}
	id1, err := st.SaveSnapshot(ctx, good)
	if err != nil {
		t.Fatalf("SaveSnapshot baseline: %v", err)
	}
	good.ID = id1
	if _, err := proc.ProcessFetch(ctx, site, u, good, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch baseline: %v", err)
	}

	// 2) The crawl that lights up EVERY dormant signal at once:
	//   - ImageCount 10 -> 12              (cosmetic change row: image_count)
	//   - MissingAltCount 0 -> 6           (cosmetic change row + image_alt_regression + image_alt_missing)
	//   - ExternalLinkCount 5 -> 60        (cosmetic change row + external_link_spike: +55 and 12x)
	//   - RedirectChain gains a loop A->B->A (substantive change row + critical redirect_loop)
	bad := good
	bad.ImageCount = 12
	bad.MissingAltCount = 6
	bad.ExternalLinkCount = 60
	bad.RedirectChain = `["https://ex.com/a","https://ex.com/b","https://ex.com/a"]`
	bad.ContentSHA256 = "v2"
	bad.ContentSimhash = 0x01
	id2, err := st.SaveSnapshot(ctx, bad)
	if err != nil {
		t.Fatalf("SaveSnapshot bad: %v", err)
	}
	bad.ID = id2
	if _, err := proc.ProcessFetch(ctx, site, u, bad, good, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch bad: %v", err)
	}

	// Collect every field the pipeline now references, from BOTH audit surfaces:
	//   (a) recorded change rows (field name verbatim), and
	//   (b) open issues (rule_id -> bridged diff field, or the rule_id itself when unmapped).
	referenced := map[string]bool{}
	changes, err := st.GetURLHistory(ctx, urlID, time.Time{})
	if err != nil {
		t.Fatalf("GetURLHistory: %v", err)
	}
	for _, c := range changes {
		referenced[c.Field] = true
	}
	issues, err := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	for _, iss := range issues {
		if field, ok := BridgeFieldForRule(iss.RuleID); ok {
			referenced[field] = true
		} else {
			referenced[iss.RuleID] = true
		}
	}

	// No dead signal remains: each formerly-dormant field is now referenced by a change
	// row or an issue. (A failure here means a field flows extract -> store but nothing
	// downstream reads it — exactly the dead-data smell the audit hunts.)
	for _, field := range []string{"image_count", "missing_alt_count", "external_link_count", "redirect_chain"} {
		if !referenced[field] {
			t.Errorf("dead signal: %q is extracted/stored but produced no change row or issue end-to-end; referenced=%v", field, referenced)
		}
	}

	// Stronger: each formerly-dormant signal also produced a change ROW (the audit's
	// "history" surface), proving the diff layer was wired, not just the rules.
	changeFields := map[string]bool{}
	for _, c := range changes {
		changeFields[c.Field] = true
	}
	for _, field := range []string{"image_count", "missing_alt_count", "external_link_count", "redirect_chain"} {
		if !changeFields[field] {
			t.Errorf("expected a recorded change row for %q (diff layer wired), got change fields %v", field, changeFields)
		}
	}

	// And the rules fired: the spike/regression/loop signals must have opened issues, so
	// the formerly-dead data is now genuinely alertable, not merely logged as history.
	openRules := map[string]bool{}
	for _, iss := range issues {
		openRules[iss.RuleID] = true
	}
	for _, ruleID := range []string{"external_link_spike", "image_alt_regression", "image_alt_missing", "redirect_loop"} {
		if !openRules[ruleID] {
			t.Errorf("expected an open %q issue (the dormant signal is now ruled on), open rules=%v", ruleID, openRules)
		}
	}
}
