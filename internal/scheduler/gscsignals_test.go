package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// ── mocks ───────────────────────────────────────────────────────────────────

// fakeVerdictReader is the mocked GSC/store read side: per-URL Rabbot verdict
// (urls row + latest snapshot) and the latest GSC url_index_status row. It mirrors
// the store's not-found contract: a URL absent from urls returns store.ErrNotFound
// from GetURL; a never-crawled URL returns store.ErrNotFound from LatestSnapshot;
// an un-inspected URL returns ok=false from LatestURLIndexStatus.
type fakeVerdictReader struct {
	urls      map[string]model.URL            // canonical url -> urls row
	snaps     map[int64]model.Snapshot        // urlID -> latest snapshot
	idx       map[string]model.URLIndexStatus // canonical url -> latest index status (presence = ok)
	getURLErr error                           // when set, GetURL returns it (systemic read failure)
	snapErr   error                           // when set, LatestSnapshot returns it (systemic)
	idxErr    error                           // when set, LatestURLIndexStatus returns it (systemic)
}

func (f *fakeVerdictReader) GetURL(_ context.Context, _ int64, url string) (model.URL, error) {
	if f.getURLErr != nil {
		return model.URL{}, f.getURLErr
	}
	u, ok := f.urls[url]
	if !ok {
		return model.URL{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeVerdictReader) LatestSnapshot(_ context.Context, urlID int64) (model.Snapshot, error) {
	if f.snapErr != nil {
		return model.Snapshot{}, f.snapErr
	}
	s, ok := f.snaps[urlID]
	if !ok {
		return model.Snapshot{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeVerdictReader) LatestURLIndexStatus(_ context.Context, _ int64, url string) (model.URLIndexStatus, bool, error) {
	if f.idxErr != nil {
		return model.URLIndexStatus{}, false, f.idxErr
	}
	s, ok := f.idx[url]
	return s, ok, nil
}

// fixedCandidates is a static URLCandidateSource for the evaluator's URL set.
type fixedCandidates []string

func (c fixedCandidates) InspectionCandidates(_ context.Context, _ int64, limit int) ([]InspectCandidate, error) {
	out := make([]InspectCandidate, 0, len(c))
	for i, u := range c {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, InspectCandidate{URL: u, Importance: float64(len(c) - i)})
	}
	return out, nil
}

// recordingSink records Ingest/Resolve calls for assertion. It is the alerts seam the
// production *alerts.Pipeline satisfies (Ingest + Resolve + HasOpenMember).
//
// It models the incident lifecycle just enough for the daily-re-page anti-noise tests:
// an Ingest registers (groupFingerprint, URL) as an open member; a Resolve removes it;
// HasOpenMember reports membership. groupFingerprint here is site+change_type+severity
// (URL elided), mirroring the pipeline. This makes a SECOND identical Ingest across a
// tick observe HasOpenMember=true (so the evaluator skips it), while a resolve-then-
// recur clears membership so the next Ingest fires again — exactly the production
// fire-on-state-change semantics, with no live pipeline.
type recordingSink struct {
	ingested   []alerts.Event
	resolved   []alerts.Event
	ingestErr  error
	resolveErr error
	hasMemErr  error
	// openMembers models open-incident membership: fp(site|change_type|severity) ->
	// set of member URLs. nil until first Ingest.
	openMembers map[string]map[string]bool
}

// sinkFP is the recordingSink's incident identity (site+change_type+severity, URL
// elided) — the same grouping groupFingerprint uses in the pipeline.
func sinkFP(e alerts.Event) string {
	return e.Site + "|" + e.ChangeType + "|" + string(e.Severity)
}

func (r *recordingSink) Ingest(_ context.Context, e alerts.Event) error {
	if r.ingestErr != nil {
		return r.ingestErr
	}
	r.ingested = append(r.ingested, e)
	if r.openMembers == nil {
		r.openMembers = map[string]map[string]bool{}
	}
	fp := sinkFP(e)
	if r.openMembers[fp] == nil {
		r.openMembers[fp] = map[string]bool{}
	}
	if e.URL != "" {
		r.openMembers[fp][e.URL] = true
	}
	return nil
}

func (r *recordingSink) Resolve(_ context.Context, e alerts.Event) error {
	if r.resolveErr != nil {
		return r.resolveErr
	}
	r.resolved = append(r.resolved, e)
	if set := r.openMembers[sinkFP(e)]; set != nil {
		delete(set, e.URL)
	}
	return nil
}

func (r *recordingSink) HasOpenMember(_ context.Context, e alerts.Event) (bool, error) {
	if r.hasMemErr != nil {
		return false, r.hasMemErr
	}
	return r.openMembers[sinkFP(e)][e.URL], nil
}

func gscSite() model.Site { return model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"} }

// urlRow builds a urls row for a canonical URL.
func urlRow(id int64, u string) model.URL {
	return model.URL{ID: id, SiteID: 1, URL: u, StatusType: model.StatusPage}
}

// ── signal 1: index_status_discrepancy ──────────────────────────────────────

func TestEvaluate_Signal1_RabbotIndexableGoogleNot_Fires(t *testing.T) {
	const u = "https://ex.com/a"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(10, u)},
		snaps: map[int64]model.Snapshot{10: {ID: 1, URLID: 10, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}

	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 1 {
		t.Fatalf("want 1 ingested discrepancy, got %d (%+v)", len(sink.ingested), sink.ingested)
	}
	got := sink.ingested[0]
	if got.ChangeType != changeTypeIndexDiscrepancy {
		t.Errorf("change_type = %q, want %q", got.ChangeType, changeTypeIndexDiscrepancy)
	}
	if got.Severity != model.SeverityWarning {
		t.Errorf("severity = %q, want warning", got.Severity)
	}
	if got.URL != u {
		t.Errorf("URL = %q, want %q", got.URL, u)
	}
	if got.URLID != 10 {
		t.Errorf("URLID = %d, want 10 (resolved via GetURL)", got.URLID)
	}
	if got.DeepLink != u {
		t.Errorf("DeepLink = %q, want %q", got.DeepLink, u)
	}
	if got.SiteID != 1 || got.Site != "https://ex.com" {
		t.Errorf("site identity = (%d,%q), want (1,https://ex.com)", got.SiteID, got.Site)
	}
	if len(sink.resolved) != 0 {
		t.Errorf("a firing discrepancy must not also resolve (got %d)", len(sink.resolved))
	}
}

func TestEvaluate_Signal1_RabbotNoindexGoogleIndexed_Fires(t *testing.T) {
	const u = "https://ex.com/b"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(11, u)},
		snaps: map[int64]model.Snapshot{11: {ID: 2, URLID: 11, Indexable: false, IndexabilityReason: "meta noindex"}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 1 {
		t.Fatalf("want 1 inverse discrepancy, got %d", len(sink.ingested))
	}
	if sink.ingested[0].ChangeType != changeTypeIndexDiscrepancy {
		t.Errorf("change_type = %q", sink.ingested[0].ChangeType)
	}
}

func TestEvaluate_Signal1_Agreement_NoEvent_AndResolves(t *testing.T) {
	const u = "https://ex.com/ok"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(12, u)},
		snaps: map[int64]model.Snapshot{12: {ID: 3, URLID: 12, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 {
		t.Errorf("agreement must not ingest (got %d)", len(sink.ingested))
	}
	// Drives Resolve for the discrepancy change_type so a previously-open incident
	// for this URL auto-closes when the last member recovers.
	if len(sink.resolved) != 1 || sink.resolved[0].ChangeType != changeTypeIndexDiscrepancy {
		t.Fatalf("agreement must resolve the discrepancy change_type (got %+v)", sink.resolved)
	}
	if sink.resolved[0].URL != u {
		t.Errorf("resolve URL = %q, want %q (member tracking)", sink.resolved[0].URL, u)
	}
}

// THE no-false-positive guard: an un-inspected URL (LatestURLIndexStatus ok=false)
// must produce NO discrepancy and NO resolve — missing data is never a verdict.
func TestEvaluate_Signal1_NoIndexStatusRow_Skips(t *testing.T) {
	const u = "https://ex.com/uninspected"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(13, u)},
		snaps: map[int64]model.Snapshot{13: {ID: 4, URLID: 13, Indexable: true}},
		idx:   map[string]model.URLIndexStatus{}, // no row → ok=false
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 || len(sink.resolved) != 0 {
		t.Errorf("missing GSC data must skip entirely (ingested=%d resolved=%d)",
			len(sink.ingested), len(sink.resolved))
	}
}

// A URL Rabbot never admitted (GetURL → ErrNotFound) has no verdict to compare:
// signal 1 skips it. But signal 2 (pure GSC-internal) may still evaluate.
func TestEvaluate_Signal1_NoRabbotURL_SkipsSignal1(t *testing.T) {
	const u = "https://ex.com/google-only"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{}, // GetURL → ErrNotFound
		snaps: map[int64]model.Snapshot{},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeIndexDiscrepancy {
			t.Errorf("signal 1 must skip a URL with no Rabbot verdict; got %+v", e)
		}
	}
}

// A never-crawled URL (LatestSnapshot → ErrNotFound) has no Rabbot verdict either.
func TestEvaluate_Signal1_NoSnapshot_SkipsSignal1(t *testing.T) {
	const u = "https://ex.com/nosnap"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(14, u)},
		snaps: map[int64]model.Snapshot{}, // LatestSnapshot → ErrNotFound
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeIndexDiscrepancy {
			t.Errorf("signal 1 must skip a URL with no snapshot; got %+v", e)
		}
	}
}

// Rabbot DISALLOWED/fetch-error → expected-not-indexed: both sources agree the page
// is not indexable, so no discrepancy even though Google says "not indexed".
func TestEvaluate_Signal1_BothNotIndexable_NoFire(t *testing.T) {
	const u = "https://ex.com/blocked"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(15, u)},
		snaps: map[int64]model.Snapshot{15: {ID: 5, URLID: 15, Indexable: false, IndexabilityReason: "robots disallow"}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "FAIL", CoverageState: "Blocked by robots.txt", RobotsTxtState: "DISALLOWED"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 {
		t.Errorf("both-not-indexable must not fire a discrepancy (got %+v)", sink.ingested)
	}
}

// An unrecognized/ambiguous Google coverage state must NOT be force-read as
// not-indexed; without a confident classification, signal 1 stays quiet.
func TestEvaluate_Signal1_UnknownGoogleState_NoFire(t *testing.T) {
	const u = "https://ex.com/weird"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(16, u)},
		snaps: map[int64]model.Snapshot{16: {ID: 6, URLID: 16, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "VERDICT_UNSPECIFIED", CoverageState: "Some future state we don't model"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 {
		t.Errorf("unknown Google state must not fire (got %+v)", sink.ingested)
	}
}

// ── signal 1: staleness guard (fix #1) ───────────────────────────────────────

// STALENESS GUARD: a GSC inspection that PREDATES Rabbot's current snapshot is too
// stale to fire a discrepancy against — Google hasn't re-seen Rabbot's current state,
// so a "disagreement" is just lag, not a divergence. Rabbot=indexable, Google=not
// indexed, but inspectedAt < snap.FetchedAt → NO discrepancy (and NO resolve: staleness
// defers judgment, it does not assert agreement).
func TestEvaluate_Signal1_StaleInspectionPredatesSnapshot_Skips(t *testing.T) {
	const u = "https://ex.com/stale"
	snapAt := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	inspAt := snapAt.Add(-48 * time.Hour) // Google inspected 2 days BEFORE Rabbot's snapshot
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(30, u)},
		snaps: map[int64]model.Snapshot{30: {ID: 1, URLID: 30, Indexable: true, FetchedAt: snapAt}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed", InspectedAt: inspAt},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink,
		Now: func() time.Time { return snapAt.Add(time.Hour) }}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 {
		t.Errorf("a GSC inspection predating the snapshot must NOT fire a discrepancy (got %+v)", sink.ingested)
	}
	if len(sink.resolved) != 0 {
		t.Errorf("staleness defers judgment; it must not resolve either (got %+v)", sink.resolved)
	}
}

// The guard is SELECTIVE, not a blanket mute: a FRESH inspection (inspectedAt at/after
// the snapshot) with the same disagreement STILL fires. This proves the fix doesn't
// silence genuine discrepancies.
func TestEvaluate_Signal1_FreshInspection_StillFires(t *testing.T) {
	const u = "https://ex.com/fresh"
	snapAt := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	inspAt := snapAt.Add(2 * time.Hour) // Google inspected AFTER Rabbot's snapshot
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(31, u)},
		snaps: map[int64]model.Snapshot{31: {ID: 2, URLID: 31, Indexable: true, FetchedAt: snapAt}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed", InspectedAt: inspAt},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink,
		Now: func() time.Time { return inspAt.Add(time.Hour) }}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 1 || sink.ingested[0].ChangeType != changeTypeIndexDiscrepancy {
		t.Fatalf("a fresh inspection with a genuine disagreement must fire (got %+v)", sink.ingested)
	}
}

// The absolute maxAge ceiling: even when the inspection POSTDATES the snapshot, an
// inspection older than gscMaxInspectionAge (quota-bounded ancient ground truth) is too
// stale to fire — Google may simply not have re-inspected the page in a month.
func TestEvaluate_Signal1_InspectionBeyondMaxAge_Skips(t *testing.T) {
	const u = "https://ex.com/ancient"
	snapAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	inspAt := snapAt.Add(time.Hour)                       // inspection postdates the (very old) snapshot
	now := inspAt.Add(gscMaxInspectionAge + 24*time.Hour) // …but is now > maxAge old
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(32, u)},
		snaps: map[int64]model.Snapshot{32: {ID: 3, URLID: 32, Indexable: true, FetchedAt: snapAt}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed", InspectedAt: inspAt},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink, Now: func() time.Time { return now }}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 {
		t.Errorf("an inspection older than gscMaxInspectionAge must not fire (got %+v)", sink.ingested)
	}
}

// gscInspectionTooStale unit table: pin the exact predicate semantics, including the
// zero-timestamp no-op that keeps fixtures/legacy rows ungated.
func TestGSCInspectionTooStale(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		inspectedAt time.Time
		fetchedAt   time.Time
		now         time.Time
		want        bool
	}{
		{"predates-snapshot", base.Add(-time.Hour), base, base.Add(time.Minute), true},
		{"equals-snapshot-fresh", base, base, base.Add(time.Minute), false},
		{"after-snapshot-fresh", base.Add(time.Hour), base, base.Add(2 * time.Hour), false},
		{"beyond-maxage", base, base.Add(-time.Hour), base.Add(gscMaxInspectionAge + time.Hour), true},
		{"within-maxage", base, base.Add(-time.Hour), base.Add(gscMaxInspectionAge - time.Hour), false},
		{"zero-inspectedAt-never-stale", time.Time{}, base, base.Add(100 * 24 * time.Hour), false},
		{"zero-fetchedAt-skips-predate-but-not-maxage", base, time.Time{}, base.Add(time.Minute), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gscInspectionTooStale(c.inspectedAt, c.fetchedAt, c.now); got != c.want {
				t.Errorf("gscInspectionTooStale = %v, want %v", got, c.want)
			}
		})
	}
}

// ── signal 2: google_canonical_mismatch ─────────────────────────────────────

func TestEvaluate_Signal2_Mismatch_Fires(t *testing.T) {
	const u = "https://ex.com/p?utm=x"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{},
		snaps: map[int64]model.Snapshot{},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/p?utm=x", GoogleCanonical: "https://ex.com/p"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var got *alerts.Event
	for i := range sink.ingested {
		if sink.ingested[i].ChangeType == changeTypeCanonicalMismatch {
			got = &sink.ingested[i]
		}
	}
	if got == nil {
		t.Fatalf("want a canonical-mismatch event, got %+v", sink.ingested)
	}
	if got.Severity != model.SeverityWarning {
		t.Errorf("severity = %q, want warning", got.Severity)
	}
	if got.Before != "https://ex.com/p?utm=x" || got.After != "https://ex.com/p" {
		t.Errorf("before/after = (%q,%q), want declared/Google-chosen", got.Before, got.After)
	}
	if got.URL != u {
		t.Errorf("URL = %q, want %q", got.URL, u)
	}
}

func TestEvaluate_Signal2_Equal_NoFire_AndResolves(t *testing.T) {
	const u = "https://ex.com/same"
	rd := &fakeVerdictReader{
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/same", GoogleCanonical: "https://ex.com/same"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeCanonicalMismatch {
			t.Errorf("equal canonicals must not fire mismatch (got %+v)", e)
		}
	}
	var resolved bool
	for _, e := range sink.resolved {
		if e.ChangeType == changeTypeCanonicalMismatch {
			resolved = true
		}
	}
	if !resolved {
		t.Errorf("equal canonicals must resolve any open mismatch incident (got %+v)", sink.resolved)
	}
}

// Trailing-slash / equivalent forms must NOT false-positive: canonicalize both
// sides before comparing.
func TestEvaluate_Signal2_EquivalentForms_NoFire(t *testing.T) {
	const u = "https://ex.com/x"
	rd := &fakeVerdictReader{
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/x", GoogleCanonical: "https://EX.com/x"}, // host case only
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeCanonicalMismatch {
			t.Errorf("host-case-equivalent canonicals must not fire (got %+v)", e)
		}
	}
}

// CANONICAL-VARIANT FALSE-FIRE (fix #2): urlx.Normalize does NOT fold www↔apex, a
// trailing slash, or http↔https — so canonicalKey must layer those folds on top. Each
// of these declared-vs-Google-chosen pairs is the SAME page in a benign variant form
// and must NOT fire a google_canonical_mismatch. (The headline case
// `https://www.x.com/p` vs `https://x.com/p/` — www + trailing slash together — is the
// re-audit's exemplar.)
func TestEvaluate_Signal2_BenignVariants_NoFire(t *testing.T) {
	cases := []struct {
		name      string
		user      string
		googleCan string
	}{
		{"www-and-trailing-slash", "https://www.ex.com/p", "https://ex.com/p/"},
		{"www-only", "https://www.ex.com/p", "https://ex.com/p"},
		{"trailing-slash-only", "https://ex.com/p", "https://ex.com/p/"},
		{"scheme-http-vs-https", "http://ex.com/p", "https://ex.com/p"},
		{"all-three-combined", "http://www.ex.com/p", "https://ex.com/p/"},
		{"apex-vs-www-reversed", "https://ex.com/p/", "https://www.ex.com/p"},
		{"root-slash-vs-empty", "https://www.ex.com", "https://ex.com/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := c.user
			rd := &fakeVerdictReader{
				idx: map[string]model.URLIndexStatus{
					u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
						UserCanonical: c.user, GoogleCanonical: c.googleCan},
				},
			}
			sink := &recordingSink{}
			ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
			if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			for _, e := range sink.ingested {
				if e.ChangeType == changeTypeCanonicalMismatch {
					t.Errorf("benign variant (%s: %q vs %q) must NOT fire a mismatch (got %+v)",
						c.name, c.user, c.googleCan, e)
				}
			}
		})
	}
}

// The fold is SELECTIVE: a GENUINE cross-page difference (different path, or a
// query-string difference Google collapsed) STILL fires. This proves fix #2 did not
// over-fold the comparison into uselessness.
func TestEvaluate_Signal2_GenuineMismatch_StillFires(t *testing.T) {
	cases := []struct {
		name      string
		user      string
		googleCan string
	}{
		{"different-path", "https://ex.com/a", "https://ex.com/b"},
		{"query-stripped-by-google", "https://ex.com/p?utm=x", "https://ex.com/p"},
		{"www-but-different-path", "https://www.ex.com/a", "https://ex.com/b/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := c.user
			rd := &fakeVerdictReader{
				idx: map[string]model.URLIndexStatus{
					u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
						UserCanonical: c.user, GoogleCanonical: c.googleCan},
				},
			}
			sink := &recordingSink{}
			ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
			if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			var fired bool
			for _, e := range sink.ingested {
				if e.ChangeType == changeTypeCanonicalMismatch {
					fired = true
				}
			}
			if !fired {
				t.Errorf("genuine mismatch (%s: %q vs %q) must fire (got %+v)",
					c.name, c.user, c.googleCan, sink.ingested)
			}
		})
	}
}

// canonicalEquivalent unit table: pin the fold semantics directly (benign → equal,
// genuine → not equal), independent of the evaluator plumbing.
func TestCanonicalEquivalent(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://www.ex.com/p", "https://ex.com/p/", true},
		{"http://www.ex.com/p", "https://ex.com/p/", true},
		{"https://ex.com/p", "https://ex.com/p/", true},
		{"https://ex.com/P", "https://ex.com/p", false}, // path case IS significant
		{"https://ex.com/a", "https://ex.com/b", false},
		{"https://ex.com/p?utm=x", "https://ex.com/p", false},
		{"https://ex.com/p?a=1", "https://ex.com/p?a=2", false},
		{"https://www.com/p", "https://com/p", false}, // www.com is NOT folded to the TLD
	}
	for _, c := range cases {
		if got := canonicalEquivalent(c.a, c.b); got != c.want {
			t.Errorf("canonicalEquivalent(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEvaluate_Signal2_EitherEmpty_NoFire(t *testing.T) {
	cases := []model.URLIndexStatus{
		{SiteID: 1, URL: "https://ex.com/e1", UserCanonical: "https://ex.com/e1", GoogleCanonical: ""},
		{SiteID: 1, URL: "https://ex.com/e2", UserCanonical: "", GoogleCanonical: "https://ex.com/e2"},
	}
	for _, c := range cases {
		rd := &fakeVerdictReader{idx: map[string]model.URLIndexStatus{c.URL: c}}
		sink := &recordingSink{}
		ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{c.URL}, Alerts: sink}
		if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		for _, e := range sink.ingested {
			if e.ChangeType == changeTypeCanonicalMismatch {
				t.Errorf("empty canonical side must not fire (%q): %+v", c.URL, e)
			}
		}
	}
}

// TestEvaluate_Signal2_CanonicalGoesEmpty_Resolves proves a mismatch incident is NOT stranded
// when Google's canonical later goes empty: an OPEN incident is resolved on the empty-data tick
// (rather than left open forever). A URL that never had an open incident churns no resolve —
// covered by TestEvaluate_Signal2_EitherEmpty_NoFire.
func TestEvaluate_Signal2_CanonicalGoesEmpty_Resolves(t *testing.T) {
	const u = "https://ex.com/p"
	sink := &recordingSink{}
	rd := &fakeVerdictReader{idx: map[string]model.URLIndexStatus{
		u: {SiteID: 1, URL: u, UserCanonical: "https://ex.com/p", GoogleCanonical: "https://ex.com/other"},
	}}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	// Tick 1: a genuine mismatch opens the incident.
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	// Tick 2: Google's canonical goes empty — the open incident must resolve, not strand.
	rd.idx[u] = model.URLIndexStatus{SiteID: 1, URL: u, UserCanonical: "https://ex.com/p", GoogleCanonical: ""}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	var resolved bool
	for _, e := range sink.resolved {
		if e.ChangeType == changeTypeCanonicalMismatch && e.URL == u {
			resolved = true
		}
	}
	if !resolved {
		t.Fatalf("empty canonical after an open mismatch must resolve the incident (not strand it); resolved=%+v", sink.resolved)
	}
}

func TestEvaluate_Signal2_NoIndexStatusRow_Skips(t *testing.T) {
	const u = "https://ex.com/none"
	rd := &fakeVerdictReader{idx: map[string]model.URLIndexStatus{}}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 0 || len(sink.resolved) != 0 {
		t.Errorf("no GSC row must skip signal 2 entirely")
	}
}

// ── error propagation ───────────────────────────────────────────────────────

func TestEvaluate_GetURLSystemicError_Surfaces(t *testing.T) {
	const u = "https://ex.com/a"
	boom := errors.New("db exploded")
	rd := &fakeVerdictReader{
		getURLErr: boom,
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a systemic GetURL error must surface, got %v", err)
	}
}

// A systemic LatestSnapshot error (not ErrNotFound) must surface from signal 1 —
// it is a real read failure, not absent data, so it is never a quiet per-URL skip.
func TestEvaluate_LatestSnapshotSystemicError_Surfaces(t *testing.T) {
	const u = "https://ex.com/a"
	boom := errors.New("snapshot read exploded")
	rd := &fakeVerdictReader{
		urls:    map[string]model.URL{u: urlRow(70, u)},
		snapErr: boom, // LatestSnapshot fails with a non-NotFound error
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a systemic LatestSnapshot error must surface, got %v", err)
	}
}

// A systemic LatestURLIndexStatus error must surface (it gates BOTH signals' reads).
func TestEvaluate_LatestURLIndexStatusError_Surfaces(t *testing.T) {
	const u = "https://ex.com/a"
	boom := errors.New("index_status read exploded")
	rd := &fakeVerdictReader{idxErr: boom}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a systemic LatestURLIndexStatus error must surface, got %v", err)
	}
}

func TestEvaluate_NilAlerts_NoPanic(t *testing.T) {
	const u = "https://ex.com/a"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(1, u)},
		snaps: map[int64]model.Snapshot{1: {ID: 1, URLID: 1, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, CoverageState: "Crawled - currently not indexed"},
		},
	}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: nil}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("nil Alerts must be a clean no-op, got %v", err)
	}
}

// ── fix #3: daily re-page noise (fire-on-state-change idempotency) ────────────

// DAILY RE-PAGE NOISE: the same persistent discrepancy across TWO consecutive daily
// pull ticks (same sink, modeling the same open incident) must Ingest exactly ONCE —
// the second tick observes the URL is already a member of the open incident and skips
// the re-notify. Without the HasOpenMember guard the pipeline's minutes-long DedupWindow
// would re-page every day.
func TestEvaluate_Signal1_SteadyDiscrepancyAcrossTicks_NotifiesOnce(t *testing.T) {
	const u = "https://ex.com/persist"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(40, u)},
		snaps: map[int64]model.Snapshot{40: {ID: 1, URLID: 40, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}

	// Tick 1 and tick 2: identical state (a persistent discrepancy).
	for tick := 1; tick <= 2; tick++ {
		if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
			t.Fatalf("Evaluate tick %d: %v", tick, err)
		}
	}
	if len(sink.ingested) != 1 {
		t.Fatalf("a steady discrepancy across 2 ticks must Ingest exactly once, got %d (%+v)",
			len(sink.ingested), sink.ingested)
	}
}

// RESOLVE-THEN-RECUR: after the discrepancy clears (a tick where Rabbot and Google
// agree → Resolve closes the incident / drops membership), a later recurrence is a
// genuine NEW state change and MUST Ingest again — a second notification.
func TestEvaluate_Signal1_ResolveThenRecur_NotifiesTwice(t *testing.T) {
	const u = "https://ex.com/flap"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(41, u)},
		snaps: map[int64]model.Snapshot{41: {ID: 1, URLID: 41, Indexable: true}},
		idx:   map[string]model.URLIndexStatus{},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}

	discrepant := model.URLIndexStatus{SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"}
	agreeing := model.URLIndexStatus{SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed"}

	// Tick 1: discrepant → fires (1st notification).
	rd.idx[u] = discrepant
	mustEval(t, ev, "tick1-discrepant")
	// Tick 2: still discrepant → idempotent, no re-notify.
	mustEval(t, ev, "tick2-still")
	if len(sink.ingested) != 1 {
		t.Fatalf("after 2 discrepant ticks want 1 ingest, got %d", len(sink.ingested))
	}
	// Tick 3: agreement → Resolve closes the incident (clears membership).
	rd.idx[u] = agreeing
	mustEval(t, ev, "tick3-agree")
	if len(sink.resolved) != 1 {
		t.Fatalf("agreement must drive a resolve, got %d", len(sink.resolved))
	}
	// Tick 4: discrepant again → genuine recurrence → 2nd notification.
	rd.idx[u] = discrepant
	mustEval(t, ev, "tick4-recur")
	if len(sink.ingested) != 2 {
		t.Fatalf("a resolve-then-recur must Ingest a SECOND time, got %d (%+v)",
			len(sink.ingested), sink.ingested)
	}
}

// Signal 2 carries the same fire-on-state-change idempotency: a persistent canonical
// mismatch across ticks notifies once.
func TestEvaluate_Signal2_SteadyMismatchAcrossTicks_NotifiesOnce(t *testing.T) {
	const u = "https://ex.com/canon"
	rd := &fakeVerdictReader{
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/canon", GoogleCanonical: "https://ex.com/other"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	for tick := 1; tick <= 3; tick++ {
		mustEval(t, ev, "mismatch-tick")
	}
	var n int
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeCanonicalMismatch {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a steady canonical mismatch across 3 ticks must Ingest once, got %d", n)
	}
}

// A HasOpenMember read error gates a real alert decision and must surface (never
// silently drop the alert).
func TestEvaluate_HasOpenMemberError_Surfaces(t *testing.T) {
	const u = "https://ex.com/boom"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(42, u)},
		snaps: map[int64]model.Snapshot{42: {ID: 1, URLID: 42, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	boom := errors.New("members table exploded")
	sink := &recordingSink{hasMemErr: boom}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a HasOpenMember error must surface, got %v", err)
	}
}

// mustEval runs Evaluate and fails on error (tick-loop helper).
func mustEval(t *testing.T, ev *GSCSignals, label string) {
	t.Helper()
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate [%s]: %v", label, err)
	}
}

// ── classification table (the falsifiable index-state semantics) ─────────────

func TestGSCIndexed_Classification(t *testing.T) {
	cases := []struct {
		name        string
		idx         model.URLIndexStatus
		wantIndexed bool
		wantKnown   bool
	}{
		{"submitted-and-indexed", model.URLIndexStatus{Verdict: "PASS", CoverageState: "Submitted and indexed"}, true, true},
		{"indexed-not-submitted", model.URLIndexStatus{Verdict: "PASS", CoverageState: "Indexed, not submitted in sitemap"}, true, true},
		{"crawled-not-indexed", model.URLIndexStatus{Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"}, false, true},
		{"discovered-not-indexed", model.URLIndexStatus{Verdict: "NEUTRAL", CoverageState: "Discovered - currently not indexed"}, false, true},
		{"excluded-noindex", model.URLIndexStatus{Verdict: "FAIL", CoverageState: "Excluded by 'noindex' tag"}, false, true},
		{"alternate-canonical", model.URLIndexStatus{Verdict: "NEUTRAL", CoverageState: "Alternate page with proper canonical tag"}, false, true},
		{"unknown-state", model.URLIndexStatus{Verdict: "VERDICT_UNSPECIFIED", CoverageState: "Mystery"}, false, false},
		{"empty", model.URLIndexStatus{}, false, false},
		// No coverage state at all → fall back to the verdict for an unambiguous PASS/FAIL.
		{"verdict-pass-no-coverage", model.URLIndexStatus{Verdict: "PASS"}, true, true},
		{"verdict-fail-no-coverage", model.URLIndexStatus{Verdict: "FAIL"}, false, true},
		{"verdict-lowercase-pass-no-coverage", model.URLIndexStatus{Verdict: "pass"}, true, true},
		{"verdict-neutral-no-coverage-unknown", model.URLIndexStatus{Verdict: "NEUTRAL"}, false, false},
		// A coverage state present (even if whitespace-padded) takes precedence over the verdict.
		{"padded-coverage-still-classified", model.URLIndexStatus{Verdict: "FAIL", CoverageState: "  Submitted and indexed  "}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			indexed, known := gscIndexed(c.idx)
			if indexed != c.wantIndexed || known != c.wantKnown {
				t.Errorf("gscIndexed(%q/%q) = (indexed=%v,known=%v), want (%v,%v)",
					c.idx.Verdict, c.idx.CoverageState, indexed, known, c.wantIndexed, c.wantKnown)
			}
		})
	}
}

// ── signal 3: search_performance_shift enrichment ───────────────────────────

// metrics builds (query,date) rows for one primary query across a date range.
func metric(date, query string, impressions int64, pos float64) model.SearchMetric {
	return model.SearchMetric{URL: "https://ex.com/p", Query: query, Date: date, Impressions: impressions, Position: pos}
}

func TestSearchPerformanceShift_ImpressionDrop_Enriches(t *testing.T) {
	// Change on 2026-06-10. Before window (06-03..06-09) and after window
	// (06-11..06-17) both fully finalized as of now (06-21, lag 3 → final ≤ 06-18).
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	var rows []model.SearchMetric
	for d := 3; d <= 9; d++ {
		rows = append(rows, metric(day(6, d), "widgets", 1000, 4.0))
	}
	for d := 11; d <= 17; d++ {
		rows = append(rows, metric(day(6, d), "widgets", 200, 9.0))
	}
	enr, ok := SearchPerformanceShift(rows, day(6, 10), now)
	if !ok {
		t.Fatalf("want an enrichment for a clear post-change drop")
	}
	if enr.Query != "widgets" {
		t.Errorf("primary query = %q, want widgets", enr.Query)
	}
	if enr.ImpressionsDelta >= 0 {
		t.Errorf("impressions delta = %d, want negative", enr.ImpressionsDelta)
	}
	if enr.PositionDelta <= 0 { // position got worse (numerically larger)
		t.Errorf("position delta = %.2f, want positive (worse)", enr.PositionDelta)
	}
	if enr.String() == "" {
		t.Errorf("enrichment must render a human string")
	}
}

// dataState=final discipline: when the only post-change data is within the partial
// lag (the latest ~3 days), there is NO finalized after-window → no enrichment.
func TestSearchPerformanceShift_OnlyPartialAfterData_NoEnrichment(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // final cutoff = 06-18
	var rows []model.SearchMetric
	for d := 10; d <= 16; d++ {
		rows = append(rows, metric(day(6, d), "widgets", 1000, 4.0))
	}
	// change on 06-19: the only "after" days (06-20, 06-21) are partial.
	rows = append(rows, metric(day(6, 20), "widgets", 50, 9.0))
	if _, ok := SearchPerformanceShift(rows, day(6, 19), now); ok {
		t.Errorf("partial-only after-window must not enrich")
	}
}

// Not enough finalized after-days to be meaningful → no enrichment.
func TestSearchPerformanceShift_InsufficientAfterWindow_NoEnrichment(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // final cutoff = 06-18
	var rows []model.SearchMetric
	for d := 3; d <= 9; d++ {
		rows = append(rows, metric(day(6, d), "widgets", 1000, 4.0))
	}
	// change on 06-17: only 06-18 is a finalized after-day (1 day < minimum).
	rows = append(rows, metric(day(6, 18), "widgets", 50, 9.0))
	if _, ok := SearchPerformanceShift(rows, day(6, 17), now); ok {
		t.Errorf("a single finalized after-day is insufficient; must not enrich")
	}
}

// THE headline invariant: NO change record + raw metric deltas → the enrichment is
// never produced on its own. SearchPerformanceShift requires a change date; the
// alert evaluator never emits a standalone shift event. Assert no Ingest carries a
// performance change_type from Evaluate, regardless of metric movement.
func TestEvaluate_NeverEmitsStandalonePerformanceSignal(t *testing.T) {
	const u = "https://ex.com/ok"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(20, u)},
		snaps: map[int64]model.Snapshot{20: {ID: 7, URLID: 20, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/ok", GoogleCanonical: "https://ex.com/ok"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, e := range sink.ingested {
		if e.ChangeType == "search_performance_shift" || e.ChangeType == "search_performance" {
			t.Errorf("Evaluate must never emit a standalone performance signal; got %+v", e)
		}
	}
}

// no before-window data at all → cannot compute a delta → no enrichment.
func TestSearchPerformanceShift_NoBeforeData_NoEnrichment(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	var rows []model.SearchMetric
	for d := 11; d <= 17; d++ {
		rows = append(rows, metric(day(6, d), "widgets", 200, 9.0))
	}
	if _, ok := SearchPerformanceShift(rows, day(6, 10), now); ok {
		t.Errorf("no before-window data must not enrich")
	}
}

// ── severity classifier extension ───────────────────────────────────────────

func TestSeverityForField_GSCSignals(t *testing.T) {
	for _, ct := range []string{changeTypeIndexDiscrepancy, changeTypeCanonicalMismatch} {
		if got := severityForField(ct); got != model.SeverityWarning {
			t.Errorf("severityForField(%q) = %q, want warning", ct, got)
		}
	}
}

// ── googleStateLabel fallback chain ──────────────────────────────────────────

// googleStateLabel prefers the coverage state, then the indexing state, then the
// verdict — the human-readable Google state that lands in the discrepancy alert body.
// This pins each fallback rung (and the whitespace-trim).
func TestGoogleStateLabel(t *testing.T) {
	cases := []struct {
		name string
		idx  model.URLIndexStatus
		want string
	}{
		{"coverage-wins", model.URLIndexStatus{CoverageState: "Crawled - currently not indexed", IndexingState: "INDEXING_ALLOWED", Verdict: "NEUTRAL"}, "Crawled - currently not indexed"},
		{"indexing-when-no-coverage", model.URLIndexStatus{IndexingState: "BLOCKED_BY_META_TAG", Verdict: "FAIL"}, "BLOCKED_BY_META_TAG"},
		{"verdict-when-only-verdict", model.URLIndexStatus{Verdict: "FAIL"}, "FAIL"},
		{"trims-whitespace-coverage", model.URLIndexStatus{CoverageState: "  Submitted and indexed  "}, "Submitted and indexed"},
		{"blank-coverage-falls-through-to-indexing", model.URLIndexStatus{CoverageState: "   ", IndexingState: "INDEXING_ALLOWED"}, "INDEXING_ALLOWED"},
		{"all-empty", model.URLIndexStatus{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := googleStateLabel(c.idx); got != c.want {
				t.Errorf("googleStateLabel(%+v) = %q, want %q", c.idx, got, c.want)
			}
		})
	}
}

// The discrepancy alert body must carry the Google coverage state via googleStateLabel:
// assert the "Google: <state>" After string the firing path builds.
func TestEvaluate_Signal1_AlertBodyNamesGoogleState(t *testing.T) {
	const u = "https://ex.com/body"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(50, u)},
		snaps: map[int64]model.Snapshot{50: {ID: 1, URLID: 50, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Discovered - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sink.ingested) != 1 {
		t.Fatalf("want 1 discrepancy, got %d", len(sink.ingested))
	}
	got := sink.ingested[0].After
	if got != "Rabbot says indexable; Google: Discovered - currently not indexed" {
		t.Fatalf("alert After = %q, want it to name Google's coverage state", got)
	}
	if sink.ingested[0].Before != "indexable" {
		t.Fatalf("alert Before = %q, want \"indexable\"", sink.ingested[0].Before)
	}
}

// ── budget ────────────────────────────────────────────────────────────────────

// A positive Budget overrides the default AND actually caps the candidate set: with 3
// candidates and Budget=1, only the first is evaluated (one signal-2 ingest, not three).
func TestEvaluate_BudgetCapsCandidates(t *testing.T) {
	mk := func(p string) (string, model.URLIndexStatus) {
		return p, model.URLIndexStatus{SiteID: 1, URL: p, Verdict: "PASS", CoverageState: "Submitted and indexed",
			UserCanonical: p, GoogleCanonical: "https://ex.com/canon"}
	}
	u1, s1 := mk("https://ex.com/1")
	u2, s2 := mk("https://ex.com/2")
	u3, s3 := mk("https://ex.com/3")
	rd := &fakeVerdictReader{idx: map[string]model.URLIndexStatus{u1: s1, u2: s2, u3: s3}}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u1, u2, u3}, Alerts: sink, Budget: 1}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var mismatches int
	for _, e := range sink.ingested {
		if e.ChangeType == changeTypeCanonicalMismatch {
			mismatches++
		}
	}
	if mismatches != 1 {
		t.Fatalf("Budget=1 over 3 candidates must evaluate exactly 1 (got %d mismatch ingests)", mismatches)
	}
}

// ── ingest / resolve error propagation ────────────────────────────────────────

// An Ingest error from the pipeline (a real alert-write failure) must surface wrapped,
// never be swallowed — the alert was supposed to fire.
func TestEvaluate_IngestError_Surfaces(t *testing.T) {
	const u = "https://ex.com/ig"
	boom := errors.New("incidents table exploded")
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(60, u)},
		snaps: map[int64]model.Snapshot{60: {ID: 1, URLID: 60, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{ingestErr: boom}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("an Ingest error must surface, got %v", err)
	}
}

// A Resolve error (closing an incident on agreement) must surface wrapped too.
func TestEvaluate_ResolveError_Surfaces(t *testing.T) {
	const u = "https://ex.com/rs"
	boom := errors.New("resolve write failed")
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(61, u)},
		snaps: map[int64]model.Snapshot{61: {ID: 1, URLID: 61, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			// Agreement (both indexed) → drives Resolve, which errors.
			u: {SiteID: 1, URL: u, Verdict: "PASS", CoverageState: "Submitted and indexed",
				UserCanonical: "https://ex.com/rs", GoogleCanonical: "https://ex.com/rs"},
		},
	}
	sink := &recordingSink{resolveErr: boom}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a Resolve error must surface, got %v", err)
	}
}

// ── Evaluate cooperative shutdown ─────────────────────────────────────────────

// An already-cancelled context makes Evaluate a clean no-op over its candidates (the
// cooperative-shutdown break): nothing is ingested or resolved.
func TestEvaluate_CancelledContext_StopsCleanly(t *testing.T) {
	const u = "https://ex.com/cancel"
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(62, u)},
		snaps: map[int64]model.Snapshot{62: {ID: 1, URLID: 62, Indexable: true}},
		idx: map[string]model.URLIndexStatus{
			u: {SiteID: 1, URL: u, Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE evaluating
	if err := ev.Evaluate(ctx, gscSite()); err != nil {
		t.Fatalf("a cancelled context must be a clean no-op, got %v", err)
	}
	if len(sink.ingested) != 0 || len(sink.resolved) != 0 {
		t.Fatalf("cancelled Evaluate must do nothing (ingested=%d resolved=%d)", len(sink.ingested), len(sink.resolved))
	}
}

// A candidate-selection error surfaces (the InspectionCandidates failure arm).
func TestEvaluate_CandidateSelectError_Surfaces(t *testing.T) {
	boom := errors.New("candidate query failed")
	rd := &fakeVerdictReader{}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: errCandidates{err: boom}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); !errors.Is(err, boom) {
		t.Fatalf("a candidate-select error must surface, got %v", err)
	}
}

// errCandidates is a URLCandidateSource that always fails.
type errCandidates struct{ err error }

func (c errCandidates) InspectionCandidates(context.Context, int64, int) ([]InspectCandidate, error) {
	return nil, c.err
}

// ── canonicalKey edge folds ───────────────────────────────────────────────────

// canonicalKey folds the slashless authority-only form to root (so a bare host keys with
// its rooted form) and returns an unparseable string verbatim (so two identical raw
// strings still compare equal). These two arms are otherwise only reached obliquely.
func TestCanonicalEquivalent_EdgeForms(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		// Authority-only (no path) folds to root: both sides normalize to "https://ex.com/".
		{"authority-only-both", "https://ex.com", "https://ex.com/", true},
		{"authority-only-vs-www", "https://www.ex.com", "https://ex.com", true},
		// An unparseable canonical compares verbatim: identical garbage is equal…
		{"unparseable-identical", "::not a url::", "::not a url::", true},
		// …but differing garbage is not.
		{"unparseable-different", "::not a url::", "::other junk::", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canonicalEquivalent(c.a, c.b); got != c.want {
				t.Errorf("canonicalEquivalent(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func day(month, d int) string {
	return time.Date(2026, time.Month(month), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}
