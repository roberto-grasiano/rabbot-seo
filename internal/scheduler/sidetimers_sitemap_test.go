package scheduler

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// fakeURLStore is the URLStore test double for RefreshSitemap. It records the
// reconcile calls (loc set + additiveOnly flag) and returns canned live coverage
// counts so the snapshot's coverage block and the drift gate can be exercised
// without a real DB.
type fakeURLStore struct {
	reconcileCalls []reconcileCall
	reconcileErr   error

	// counts is returned by SitemapLiveCounts; inSitemapTotal feeds the
	// sitemapped_unadmitted derivation (len(uniqLocs) - inSitemapTotal) and is
	// folded into counts.InSitemapTotal on return.
	counts         SitemapLiveCounts
	inSitemapTotal int
	countsErr      error
}

type reconcileCall struct {
	locs         []string
	additiveOnly bool
}

func (f *fakeURLStore) ReconcileSitemapMembership(_ context.Context, _ int64, locs []string, additiveOnly bool) error {
	f.reconcileCalls = append(f.reconcileCalls, reconcileCall{locs: append([]string(nil), locs...), additiveOnly: additiveOnly})
	return f.reconcileErr
}

func (f *fakeURLStore) SitemapLiveCounts(_ context.Context, _ int64) (SitemapLiveCounts, error) {
	c := f.counts
	c.InSitemapTotal = f.inSitemapTotal
	return c, f.countsErr
}

// canonicalSetHash mirrors RefreshSitemap's canonical hashing so tests can assert
// the persisted snapshot hash without duplicating the implementation's internals.
func canonicalSetHash(locs []string) string {
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(locs))
	for _, l := range locs {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		uniq = append(uniq, l)
	}
	// sort
	for i := 1; i < len(uniq); i++ {
		for j := i; j > 0 && uniq[j-1] > uniq[j]; j-- {
			uniq[j-1], uniq[j] = uniq[j], uniq[j-1]
		}
	}
	return extract.ContentSHA256(strings.Join(uniq, "\n"))
}

func entries(locs ...string) []SitemapEntry {
	out := make([]SitemapEntry, 0, len(locs))
	for _, l := range locs {
		out = append(out, SitemapEntry{Loc: l, Priority: 0.5})
	}
	return out
}

func newSitemapTimer(fs *fakeFileStore, us *fakeURLStore, col SitemapCollection, ing *fakeIngestor) *SideTimers {
	st := &SideTimers{
		FileStore: fs,
		URLStore:  us,
		Sitemaps:  fakeCollector{col: col},
		Now:       func() time.Time { return time.Unix(1000, 0).UTC() },
	}
	if ing != nil {
		st.Alerts = ing
	}
	return st
}

func parsedSitemapDoc(t *testing.T, raw string) sitemapDoc {
	t.Helper()
	var d sitemapDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("snapshot ParsedEntries is not valid sitemapDoc JSON: %v\n%s", err, raw)
	}
	return d
}

// Acceptance criterion 1: the first RefreshSitemap pass persists a FileKindSitemap
// snapshot (canonical set hash + coverage block) and ingests ZERO events (baseline).
func TestRefreshSitemapFirstPassBaseline(t *testing.T) {
	fs := &fakeFileStore{}
	us := &fakeURLStore{counts: SitemapLiveCounts{SitemappedUncrawled: 2}, inSitemapTotal: 2}
	ing := &fakeIngestor{}
	col := SitemapCollection{
		Entries:    entries("https://ex.com/a", "https://ex.com/b"),
		SeedURL:    "https://ex.com/sitemap.xml",
		SeedStatus: 200,
	}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("first pass must persist a snapshot, saved %d", len(fs.saved))
	}
	snap := fs.saved[0]
	if snap.Kind != model.FileKindSitemap {
		t.Errorf("kind = %q, want sitemap", snap.Kind)
	}
	if snap.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", snap.HTTPStatus)
	}
	if want := canonicalSetHash([]string{"https://ex.com/a", "https://ex.com/b"}); snap.ContentSHA256 != want {
		t.Errorf("ContentSHA256 = %q, want canonical set hash %q", snap.ContentSHA256, want)
	}
	doc := parsedSitemapDoc(t, snap.ParsedEntries)
	if doc.V != 1 {
		t.Errorf("doc.V = %d, want 1", doc.V)
	}
	if doc.Count != 2 {
		t.Errorf("doc.Count = %d, want 2", doc.Count)
	}
	if doc.Coverage.SitemappedUncrawled != 2 {
		t.Errorf("coverage.sitemapped_uncrawled = %d, want 2", doc.Coverage.SitemappedUncrawled)
	}
	if len(ing.events) != 0 {
		t.Fatalf("baseline pass must ingest ZERO events, got %d", len(ing.events))
	}
}

// Acceptance criterion 2: an unchanged set + status + drift-gated coverage on pass
// 2 writes NO new file_snapshots row.
func TestRefreshSitemapDedupsUnchanged(t *testing.T) {
	locs := []string{"https://ex.com/a", "https://ex.com/b"}
	hash := canonicalSetHash(locs)
	cov := `{"sitemapped_uncrawled":2,"sitemapped_unadmitted":0,"crawled_not_in_sitemap":0}`
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: hash, HTTPStatus: 200,
		ParsedEntries: `{"v":1,"count":2,"coverage":` + cov + `}`,
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	us := &fakeURLStore{counts: SitemapLiveCounts{SitemappedUncrawled: 2}, inSitemapTotal: 2}
	ing := &fakeIngestor{}
	col := SitemapCollection{Entries: entries(locs...), SeedStatus: 200}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	if len(fs.saved) != 0 {
		t.Fatalf("unchanged set/status/coverage must not write a new snapshot, saved %d", len(fs.saved))
	}
	if len(ing.events) != 0 {
		t.Fatalf("unchanged pass must not alert, ingested %d", len(ing.events))
	}
}

// Acceptance criterion 3: a set change (adds 2, drops 1) emits exactly one
// sitemap_xml warning whose Before/After carry counts + added/dropped sample paths.
func TestRefreshSitemapSetChangeWarning(t *testing.T) {
	priorLocs := []string{"https://ex.com/a", "https://ex.com/keep", "https://ex.com/drop"}
	priorHash := canonicalSetHash(priorLocs)
	priorDoc := sitemapDoc{
		V: 1, Count: len(priorLocs), URLs: priorLocs,
		Coverage: sitemapCoverageBlock{},
	}
	priorJSON, _ := json.Marshal(priorDoc)
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: priorHash, HTTPStatus: 200, ParsedEntries: string(priorJSON),
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	us := &fakeURLStore{}
	ing := &fakeIngestor{}
	// new set: keep a + keep, drop /drop, add /new1 /new2
	col := SitemapCollection{
		Entries:    entries("https://ex.com/a", "https://ex.com/keep", "https://ex.com/new1", "https://ex.com/new2"),
		SeedStatus: 200,
	}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	var setEvents []alerts.Event
	for _, e := range ing.events {
		if e.ChangeType == "sitemap_xml" {
			setEvents = append(setEvents, e)
		}
	}
	if len(setEvents) != 1 {
		t.Fatalf("want exactly one sitemap_xml warning, got %d (%+v)", len(setEvents), ing.events)
	}
	ev := setEvents[0]
	if ev.Severity != model.SeverityWarning {
		t.Errorf("severity = %q, want warning", ev.Severity)
	}
	// Before "3 urls", After "4 urls (+2, -1; dropped: ...)"
	if !strings.Contains(ev.Before, "3 urls") {
		t.Errorf("Before = %q, want to contain '3 urls'", ev.Before)
	}
	if !strings.Contains(ev.After, "4 urls") || !strings.Contains(ev.After, "+2") || !strings.Contains(ev.After, "-1") {
		t.Errorf("After = %q, want counts + (+2, -1)", ev.After)
	}
	if !strings.Contains(ev.After, "/drop") {
		t.Errorf("After = %q, want dropped sample path /drop", ev.After)
	}
}

// Acceptance criterion 4: seed status 200->404 emits one sitemap_xml_status
// critical event ("200"->"404"); 404->200 next pass emits again (recovery visible).
//
// Uses the REAL collector shape for a broken seed: a 404/5xx/network-error seed
// returns Entries=nil + Incomplete=true (discovery.go marks any non-OK seed
// incomplete). The break pass and the recovery pass must each emit exactly one
// sitemap_xml_STATUS event and ZERO sitemap_xml SET-change events — a status
// regression must not drag along a phantom "N urls -> 0 urls" mass-drop warning.
func TestRefreshSitemapStatusRegressionCritical(t *testing.T) {
	locs := []string{"https://ex.com/a"}
	priorHash := canonicalSetHash(locs)
	priorDoc := sitemapDoc{V: 1, Count: 1, URLs: locs}
	priorJSON, _ := json.Marshal(priorDoc)
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: priorHash, HTTPStatus: 200, ParsedEntries: string(priorJSON),
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	us := &fakeURLStore{}
	ing := &fakeIngestor{}
	// Real 404 seed shape: no entries, Incomplete=true, status 404.
	col := SitemapCollection{Entries: nil, SeedStatus: 404, Incomplete: true}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	var statusEvents, setEvents []alerts.Event
	for _, e := range ing.events {
		switch e.ChangeType {
		case "sitemap_xml_status":
			statusEvents = append(statusEvents, e)
		case "sitemap_xml":
			setEvents = append(setEvents, e)
		}
	}
	if len(statusEvents) != 1 {
		t.Fatalf("want one sitemap_xml_status event, got %d (%+v)", len(statusEvents), ing.events)
	}
	if len(setEvents) != 0 {
		t.Fatalf("a 404 break (incomplete) must emit ZERO sitemap_xml set-change events, got %d (%+v)", len(setEvents), setEvents)
	}
	ev := statusEvents[0]
	if ev.Severity != model.SeverityCritical {
		t.Errorf("severity = %q, want critical", ev.Severity)
	}
	if ev.Before != "200" || ev.After != "404" {
		t.Errorf("status change = %q->%q, want 200->404", ev.Before, ev.After)
	}

	// recovery: 404 -> 200 emits the status event again, and STILL no phantom
	// set-change (the prior snapshot's baseline hash was carried from the last
	// complete set, so the recovered complete set diffs clean).
	if len(fs.saved) != 1 {
		t.Fatalf("status change must persist a new snapshot, saved %d", len(fs.saved))
	}
	fs2 := &fakeFileStore{preload: []model.FileSnapshot{fs.saved[0]}}
	ing2 := &fakeIngestor{}
	st2 := newSitemapTimer(fs2, us, SitemapCollection{Entries: entries(locs...), SeedStatus: 200}, ing2)
	if err := st2.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap recovery: %v", err)
	}
	var rec, recSet []alerts.Event
	for _, e := range ing2.events {
		switch e.ChangeType {
		case "sitemap_xml_status":
			rec = append(rec, e)
		case "sitemap_xml":
			recSet = append(recSet, e)
		}
	}
	if len(rec) != 1 || rec[0].Before != "404" || rec[0].After != "200" {
		t.Fatalf("recovery must emit sitemap_xml_status 404->200, got %+v", ing2.events)
	}
	if len(recSet) != 0 {
		t.Fatalf("404->200 recovery must emit ZERO phantom sitemap_xml set-change events, got %d (%+v)", len(recSet), recSet)
	}
}

// Acceptance criterion 5: an incomplete collection reconciles additive-only (never
// flips in_sitemap off), persists incomplete:true, and emits NO drop alert (drop
// reporting suppressed). Additions are still admitted (reconcile still called).
func TestRefreshSitemapIncompleteNoDropAlert(t *testing.T) {
	priorLocs := []string{"https://ex.com/a", "https://ex.com/b", "https://ex.com/c"}
	priorHash := canonicalSetHash(priorLocs)
	priorDoc := sitemapDoc{V: 1, Count: len(priorLocs), URLs: priorLocs}
	priorJSON, _ := json.Marshal(priorDoc)
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: priorHash, HTTPStatus: 200, ParsedEntries: string(priorJSON),
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	us := &fakeURLStore{}
	ing := &fakeIngestor{}
	// partial read returns only /a (looks like a mass drop of b,c) — but Incomplete.
	col := SitemapCollection{Entries: entries("https://ex.com/a"), SeedStatus: 200, Incomplete: true}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	// reconcile must be additive-only.
	if len(us.reconcileCalls) != 1 {
		t.Fatalf("reconcile called %d times, want 1", len(us.reconcileCalls))
	}
	if !us.reconcileCalls[0].additiveOnly {
		t.Errorf("incomplete collection must reconcile additiveOnly=true (no 1->0 flips)")
	}
	// snapshot, if written, carries incomplete:true; and no drop reporting in any
	// sitemap_xml event.
	for _, s := range fs.saved {
		d := parsedSitemapDoc(t, s.ParsedEntries)
		if !d.Incomplete {
			t.Errorf("incomplete pass must persist incomplete:true")
		}
	}
	// Spec lines 47-48: an incomplete collection must never masquerade as a mass
	// URL drop. The narrow "no dropped: sample" check is insufficient — the count
	// delta ("3 urls"->"1 urls") is itself the masquerade. Assert ZERO sitemap_xml
	// set-change events fire on the incomplete pass (encode the spec guarantee, not
	// the implementation detail).
	for _, e := range ing.events {
		if e.ChangeType == "sitemap_xml" {
			t.Errorf("incomplete collection must emit NO sitemap_xml set-change event, got Before=%q After=%q", e.Before, e.After)
		}
	}
}

// TestRefreshSitemapDriftSilentOnIncomplete closes criterion 5's gap symmetric to
// the drop-alert suppression: on an incomplete pass, an additive-only reconcile can
// grow sitemapped_uncrawled purely from newly-admitted-but-uncrawled URLs. That
// growth must NOT fire a sitemap_coverage_drift warning — a partial read must never
// masquerade as a real coverage-regression signal (spec lines 47-48).
func TestRefreshSitemapDriftSilentOnIncomplete(t *testing.T) {
	locs := []string{"https://ex.com/a"}
	hash := canonicalSetHash(locs)
	// Prior COMPLETE snapshot recorded uncrawled=0.
	priorDoc := sitemapDoc{V: 1, Count: 1, URLs: locs, Coverage: sitemapCoverageBlock{SitemappedUncrawled: 0}}
	pj, _ := json.Marshal(priorDoc)
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: hash, HTTPStatus: 200, ParsedEntries: string(pj),
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	// Incomplete pass: live counts report uncrawled=3 (grow vs the prior block 0) —
	// the exact condition that fired a spurious drift warning before the guard.
	us := &fakeURLStore{counts: SitemapLiveCounts{SitemappedUncrawled: 3}, inSitemapTotal: 1}
	ing := &fakeIngestor{}
	col := SitemapCollection{Entries: entries(locs...), SeedStatus: 200, Incomplete: true}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	for _, e := range ing.events {
		if e.ChangeType == "sitemap_coverage_drift" {
			t.Errorf("incomplete pass must emit NO sitemap_coverage_drift, got Before=%q After=%q", e.Before, e.After)
		}
	}
}

// Acceptance criterion 7: the drift gate. Prior coverage {uncrawled:0} -> current
// {uncrawled:5} emits one sitemap_coverage_drift warning; a later decrease to 2
// emits nothing.
func TestRefreshSitemapCoverageDriftGate(t *testing.T) {
	locs := []string{"https://ex.com/a"}
	hash := canonicalSetHash(locs)
	mkPrior := func(uncrawled int) model.FileSnapshot {
		doc := sitemapDoc{V: 1, Count: 1, URLs: locs, Coverage: sitemapCoverageBlock{SitemappedUncrawled: uncrawled}}
		b, _ := json.Marshal(doc)
		return model.FileSnapshot{
			ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
			ContentSHA256: hash, HTTPStatus: 200, ParsedEntries: string(b),
		}
	}

	// increase 0 -> 5 emits one drift warning.
	fs := &fakeFileStore{preload: []model.FileSnapshot{mkPrior(0)}}
	us := &fakeURLStore{counts: SitemapLiveCounts{SitemappedUncrawled: 5}, inSitemapTotal: 1}
	ing := &fakeIngestor{}
	st := newSitemapTimer(fs, us, SitemapCollection{Entries: entries(locs...), SeedStatus: 200}, ing)
	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap increase: %v", err)
	}
	var drift []alerts.Event
	for _, e := range ing.events {
		if e.ChangeType == "sitemap_coverage_drift" {
			drift = append(drift, e)
		}
	}
	if len(drift) != 1 {
		t.Fatalf("coverage increase must emit one sitemap_coverage_drift, got %d (%+v)", len(drift), ing.events)
	}
	if drift[0].Severity != model.SeverityWarning {
		t.Errorf("drift severity = %q, want warning", drift[0].Severity)
	}

	// decrease 5 -> 2 emits nothing.
	fs2 := &fakeFileStore{preload: []model.FileSnapshot{mkPrior(5)}}
	us2 := &fakeURLStore{counts: SitemapLiveCounts{SitemappedUncrawled: 2}, inSitemapTotal: 1}
	ing2 := &fakeIngestor{}
	st2 := newSitemapTimer(fs2, us2, SitemapCollection{Entries: entries(locs...), SeedStatus: 200}, ing2)
	if err := st2.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap decrease: %v", err)
	}
	for _, e := range ing2.events {
		if e.ChangeType == "sitemap_coverage_drift" {
			t.Errorf("coverage decrease must be silent, got drift event %+v", e)
		}
	}
}

// TestRefreshSitemapTransientPartialNoPhantomDrift probes the deep-sweep finding:
// "unadmitted = len(locs) - InSitemapTotal on a partial read yields a spurious
// positive that the drift gate reads as growth, firing a phantom
// sitemap_coverage_drift." It drives RefreshSitemap with the InSitemapTotal that
// the REAL store produces after an additive-only reconcile of a partial set: the
// full prior set stays flagged, so InSitemapTotal (3) >= len(partial locs) (1) and
// the floored unadmitted is 0 — same as the complete-pass baseline (also 0). No
// drift must fire.
//
// The finding's reproduction set InSitemapTotal independently of the partial loc
// set (fake), which the real additive-only reconcile never does.
func TestRefreshSitemapTransientPartialNoPhantomDrift(t *testing.T) {
	site := model.Site{ID: 1, BaseURL: "https://ex.com"}
	partial := []string{"https://ex.com/a"}
	hash := canonicalSetHash(partial)

	// Prior snapshot: a complete pass over {a,b,c} that recorded unadmitted=0
	// (all three admitted). The drift gate compares against this block.
	priorDoc := sitemapDoc{
		V: 1, Count: 1, URLs: partial,
		Coverage: sitemapCoverageBlock{SitemappedUnadmitted: 0},
	}
	pj, _ := json.Marshal(priorDoc)
	prior := model.FileSnapshot{
		ID: 5, SiteID: 1, Kind: model.FileKindSitemap,
		ContentSHA256: hash, HTTPStatus: 200, ParsedEntries: string(pj),
	}
	fs := &fakeFileStore{preload: []model.FileSnapshot{prior}}

	// REAL store state for a transient partial read: additive-only reconcile keeps
	// the full prior set flagged, so in_sitemap total stays 3 while this pass's loc
	// set is only {a} (len 1).
	us := &fakeURLStore{counts: SitemapLiveCounts{}, inSitemapTotal: 3}
	ing := &fakeIngestor{}
	col := SitemapCollection{Entries: entries(partial...), SeedStatus: 200, Incomplete: true}
	st := newSitemapTimer(fs, us, col, ing)

	if err := st.RefreshSitemap(context.Background(), site); err != nil {
		t.Fatalf("RefreshSitemap: %v", err)
	}
	for _, e := range ing.events {
		if e.ChangeType == "sitemap_coverage_drift" {
			t.Errorf("FINDING CONFIRMED: transient partial read fired phantom sitemap_coverage_drift: before=%q after=%q",
				e.Before, e.After)
		}
	}
}

// Acceptance criterion 13: an over-cap loc set stores urls_capped:true + the full
// count; a later set-hash change still emits a sitemap_xml warning, but with a
// hash-only (count-only) Before/After — no added/dropped samples (a capped list
// cannot distinguish a real drop from an unstored URL).
//
// The production cap (sitemapURLCap=20000) is left untouched; the test constructs
// just over the cap (cheap short strings) so the cap path is exercised honestly.
func TestRefreshSitemapURLListCap(t *testing.T) {
	overCap := sitemapURLCap + 1
	mkLocs := func(n int) []string {
		locs := make([]string, n)
		for i := range locs {
			locs[i] = "https://ex.com/p/" + strconv.Itoa(i)
		}
		return locs
	}

	// ── Pass 1: an over-cap set is the baseline (alert-silent), but the snapshot
	// must record urls_capped:true with the FULL count (not the truncated list len).
	locs1 := mkLocs(overCap)
	fs := &fakeFileStore{}
	us := &fakeURLStore{}
	ing := &fakeIngestor{}
	st := newSitemapTimer(fs, us, SitemapCollection{Entries: entries(locs1...), SeedStatus: 200}, ing)
	if err := st.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap pass 1: %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("over-cap baseline must persist one snapshot, saved %d", len(fs.saved))
	}
	doc := parsedSitemapDoc(t, fs.saved[0].ParsedEntries)
	if !doc.URLsCapped {
		t.Errorf("over-cap snapshot must set urls_capped:true")
	}
	if doc.Count != overCap {
		t.Errorf("doc.Count = %d, want full count %d (not the truncated list length)", doc.Count, overCap)
	}
	if len(doc.URLs) != sitemapURLCap {
		t.Errorf("stored URL list len = %d, want capped at %d", len(doc.URLs), sitemapURLCap)
	}
	if len(ing.events) != 0 {
		t.Fatalf("over-cap baseline must ingest ZERO events, got %d", len(ing.events))
	}

	// ── Pass 2: a set-hash change against the capped prior snapshot. The set still
	// changed (add one, drop one), so a sitemap_xml warning fires — but because both
	// sides are capped, the Before/After is count-only (no "dropped:" samples).
	prior := fs.saved[0]
	locs2 := append(mkLocs(overCap-1), "https://ex.com/p/NEW") // drop the last, add NEW
	fs2 := &fakeFileStore{preload: []model.FileSnapshot{prior}}
	ing2 := &fakeIngestor{}
	st2 := newSitemapTimer(fs2, us, SitemapCollection{Entries: entries(locs2...), SeedStatus: 200}, ing2)
	if err := st2.RefreshSitemap(context.Background(), model.Site{ID: 1, BaseURL: "https://ex.com"}); err != nil {
		t.Fatalf("RefreshSitemap pass 2: %v", err)
	}
	var setEvents []alerts.Event
	for _, e := range ing2.events {
		if e.ChangeType == "sitemap_xml" {
			setEvents = append(setEvents, e)
		}
	}
	if len(setEvents) != 1 {
		t.Fatalf("capped set change must still emit one sitemap_xml warning, got %d (%+v)", len(setEvents), ing2.events)
	}
	ev := setEvents[0]
	if !strings.Contains(ev.Before, "urls") || !strings.Contains(ev.After, "urls") {
		t.Errorf("capped Before/After must be count-only ('N urls'); got %q -> %q", ev.Before, ev.After)
	}
	if strings.Contains(ev.After, "dropped:") || strings.Contains(ev.After, "+") || strings.Contains(ev.After, "-") {
		t.Errorf("capped set change must suppress added/dropped samples; After = %q", ev.After)
	}
}

// Acceptance criterion 12: severityForField buckets the sitemap fields correctly.
func TestSeverityForSitemapFields(t *testing.T) {
	if got := severityForField("sitemap_xml_status"); got != model.SeverityCritical {
		t.Errorf("sitemap_xml_status = %q, want critical", got)
	}
	if got := severityForField("sitemap_xml"); got != model.SeverityWarning {
		t.Errorf("sitemap_xml = %q, want warning", got)
	}
	if got := severityForField("sitemap_coverage_drift"); got != model.SeverityWarning {
		t.Errorf("sitemap_coverage_drift = %q, want warning", got)
	}
}
