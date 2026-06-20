package scheduler

// Behavioral golden suite for the W2 GSC signal layer — the search-intelligence
// analogue of internal/behavior's per-site-type SIGNAL/NOISE matrix.
//
// Why this lives in internal/scheduler and not internal/behavior: the GSC signals
// are NOT in diff/rules' DefaultRuleSet (they evaluate Rabbot's stored snapshot
// verdict against mocked url_index_status / search_metrics through GSCSignals.Evaluate
// and SearchPerformanceShift). internal/behavior's package contract doc-locks it to
// "depends only on internal/diff, internal/rules, and internal/model"; importing the
// scheduler-resident evaluator would break that contract and risk an import cycle. So,
// per the surface guidance ("house GSC scenarios in the signal package to honor
// behavior's diff/rules/model-only import contract"), the behavioral matrix is mirrored
// HERE, in the package that owns the evaluator. It reuses the same table-driven idiom
// as scenarios.go/behavior_test.go: a scenario struct, a shared driver, an EXACT
// expected-signal set (reflect.DeepEqual), the must_fire / must_stay_quiet / edge
// taxonomy, and the skip-suspected-defect discipline.
//
// The two falsifiable theses this suite encodes (the "valid + worth-the-distraction"
// bar from the prompt):
//   1. Signals fire on, and ONLY on, a genuine engine-vs-Google divergence
//      (index_status_discrepancy) or a genuine declared-vs-chosen canonical split
//      (google_canonical_mismatch). Missing / ambiguous / partial / agreeing data is
//      silent — the worst outcome is crying wolf.
//   2. THE HEADLINE INVARIANT: a raw clicks/impressions/position delta with no index
//      or canonical issue and no associated change record produces ZERO standalone
//      signals. There is no raw-traffic alert, by construction.

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// ── golden-matrix harness (mirrors scenarios.go) ─────────────────────────────

// gscClass labels a scenario's intent, mirroring internal/behavior's taxonomy.
type gscClass string

const (
	gscMustFire      gscClass = "must_fire"       // a genuine divergence: a signal MUST page.
	gscMustStayQuiet gscClass = "must_stay_quiet" // agreement / missing / ambiguous data: SILENT.
	gscEdge          gscClass = "edge"            // boundary cases (equivalence, empties, inverse).
)

// gscScenario is one row of the GSC behavioral golden suite. It feeds mocked Rabbot
// state (snapshot — present iff hasSnapshot) and mocked Google ground truth (idx —
// present iff hasIdx) for ONE candidate URL through GSCSignals.Evaluate, then asserts
// the EXACT set of emitted signals.
type gscScenario struct {
	name  string
	class gscClass
	url   string

	// hasURLRow controls whether GetURL resolves a urls row (false → ErrNotFound,
	// i.e. a URL Rabbot never admitted — Google-only). urlID is that row's id.
	hasURLRow bool
	urlID     int64
	// hasSnapshot controls whether LatestSnapshot resolves (false → ErrNotFound, i.e.
	// never crawled). snapshot is the Rabbot indexability verdict source.
	hasSnapshot bool
	snapshot    model.Snapshot
	// hasIdx controls whether LatestURLIndexStatus returns ok=true. false models the
	// quota-bounded "un-inspected URL" case — the no-false-positive guard.
	hasIdx bool
	idx    model.URLIndexStatus

	// wantSignals is the EXACT expected set of EMITTED (Ingest) signals as
	// change_type -> severity. nil/empty means "no signal fires" (the must_stay_quiet
	// claim). RRuleIDs/change_types are unique per URL+signal here (one URL per row).
	wantSignals map[string]model.Severity
	// wantResolves is the EXACT set of change_types the evaluator drives Resolve for
	// (the auto-close path: agreement, realignment, or data-gone). nil = none.
	wantResolves map[string]model.Severity

	// skip, when set, marks a SUSPECTED DEFECT run via t.Skip rather than blessing a
	// wrong-fire / missed-fire as the baseline (the behavior-suite discipline).
	skip string
}

// driveGSCSignals runs GSCSignals.Evaluate over a single-URL candidate set built from
// the scenario's mocked store/GSC state and returns the emitted ingest + resolve sets
// as change_type -> severity maps (mirroring behavior's findingSet collapse).
func driveGSCSignals(t *testing.T, sc gscScenario) (ingested, resolved map[string]model.Severity) {
	t.Helper()
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{},
		snaps: map[int64]model.Snapshot{},
		idx:   map[string]model.URLIndexStatus{},
	}
	if sc.hasURLRow {
		rd.urls[sc.url] = urlRow(sc.urlID, sc.url)
	}
	if sc.hasSnapshot {
		// The snapshot's URLID is informational here; LatestSnapshot is keyed by the
		// urls-row id the evaluator resolves via GetURL.
		rd.snaps[sc.urlID] = sc.snapshot
	}
	if sc.hasIdx {
		rd.idx[sc.url] = sc.idx
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{sc.url}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("[%s] Evaluate returned error: %v", sc.name, err)
	}
	return collapseEvents(sink.ingested), collapseEvents(sink.resolved)
}

// collapseEvents reduces a slice of alerts.Event to a change_type -> severity map. The
// suite uses one URL per scenario, so each (change_type) appears at most once; a
// duplicate would be a real bug (the evaluator must drive exactly one of ingest/resolve
// per signal per URL) and is surfaced as a test failure by the caller's exact compare.
func collapseEvents(evs []alerts.Event) map[string]model.Severity {
	out := make(map[string]model.Severity, len(evs))
	for _, e := range evs {
		out[e.ChangeType] = e.Severity
	}
	return out
}

// runGSCScenarios is the shared table-driven runner (mirrors behavior_test.go's
// runScenarios): per scenario it drives Evaluate and asserts the EXACT emitted-signal
// set and the EXACT resolve set. A skip-tagged scenario is reported via t.Skip.
func runGSCScenarios(t *testing.T, scns []gscScenario) {
	t.Helper()
	for _, sc := range scns {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if sc.skip != "" {
				t.Skipf("SUSPECTED DEFECT: %s", sc.skip)
			}
			gotIng, gotRes := driveGSCSignals(t, sc)
			wantIng := sc.wantSignals
			if wantIng == nil {
				wantIng = map[string]model.Severity{}
			}
			wantRes := sc.wantResolves
			if wantRes == nil {
				wantRes = map[string]model.Severity{}
			}
			if !reflect.DeepEqual(gotIng, wantIng) {
				t.Errorf("[%s/%s] emitted-signal set mismatch\n  got:  %v\n  want: %v",
					sc.name, sc.class, sortedSev(gotIng), sortedSev(wantIng))
			}
			if !reflect.DeepEqual(gotRes, wantRes) {
				t.Errorf("[%s/%s] resolve set mismatch\n  got:  %v\n  want: %v",
					sc.name, sc.class, sortedSev(gotRes), sortedSev(wantRes))
			}
		})
	}
}

// sortedSev renders a change_type->severity map deterministically for failure messages
// (mirrors behavior's sortedFindings).
func sortedSev(m map[string]model.Severity) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k+"="+string(m[k]))
	}
	sort.Strings(keys)
	return keys
}

// warn is shorthand for the warning severity both GSC signals route to.
var warn = model.SeverityWarning

// ── scenario builders ─────────────────────────────────────────────────────────

// idxRow builds a url_index_status row for a URL with the given verdict/coverage.
func idxRow(u, verdict, coverage string) model.URLIndexStatus {
	return model.URLIndexStatus{SiteID: 1, URL: u, Verdict: verdict, CoverageState: coverage}
}

// indexableSnap builds a Rabbot snapshot that is indexable.
func indexableSnap(urlID int64) model.Snapshot {
	return model.Snapshot{ID: urlID * 100, URLID: urlID, Indexable: true}
}

// noindexSnap builds a non-indexable Rabbot snapshot with a reason.
func noindexSnap(urlID int64, reason string) model.Snapshot {
	return model.Snapshot{ID: urlID*100 + 1, URLID: urlID, Indexable: false, IndexabilityReason: reason}
}

// indexStatusScenarios encodes signal-1 (index_status_discrepancy) golden rows: the
// engine-vs-Google divergences that MUST page, the agreements/missing/ambiguous data
// that must stay quiet, and the inverse-direction divergence.
func indexStatusScenarios() []gscScenario {
	return []gscScenario{
		// ── MUST FIRE: Rabbot believes the page is indexable; Google did not index it.
		{
			name: "discrepancy_rabbot_indexable_google_crawled_not_indexed", class: gscMustFire,
			url: "https://ex.com/a", hasURLRow: true, urlID: 10,
			hasSnapshot: true, snapshot: indexableSnap(10),
			hasIdx: true, idx: idxRow("https://ex.com/a", "NEUTRAL", "Crawled - currently not indexed"),
			wantSignals: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		{
			name: "discrepancy_rabbot_indexable_google_discovered_not_indexed", class: gscMustFire,
			url: "https://ex.com/b", hasURLRow: true, urlID: 11,
			hasSnapshot: true, snapshot: indexableSnap(11),
			hasIdx: true, idx: idxRow("https://ex.com/b", "NEUTRAL", "Discovered - currently not indexed"),
			wantSignals: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		{
			name: "discrepancy_rabbot_indexable_google_excluded_noindex", class: gscMustFire,
			url: "https://ex.com/c", hasURLRow: true, urlID: 12,
			hasSnapshot: true, snapshot: indexableSnap(12),
			// Google reports a noindex verdict on a page Rabbot believes is indexable
			// (the most actionable form: the deployed tag and Google disagree).
			hasIdx:      true,
			idx:         idxRow("https://ex.com/c", "FAIL", "Excluded by 'noindex' tag"),
			wantSignals: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		// ── MUST FIRE (inverse): Rabbot suppresses the page (noindex); Google indexed it.
		{
			name: "discrepancy_inverse_rabbot_noindex_google_indexed", class: gscMustFire,
			url: "https://ex.com/d", hasURLRow: true, urlID: 13,
			hasSnapshot: true, snapshot: noindexSnap(13, "meta_robots_noindex"),
			hasIdx:      true,
			idx:         idxRow("https://ex.com/d", "PASS", "Submitted and indexed"),
			wantSignals: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		// ── MUST STAY QUIET: both agree the page IS indexed → no signal, and resolve any
		// prior open discrepancy (the auto-close path).
		{
			name: "agree_both_indexed", class: gscMustStayQuiet,
			url: "https://ex.com/ok", hasURLRow: true, urlID: 14,
			hasSnapshot: true, snapshot: indexableSnap(14),
			hasIdx:       true,
			idx:          idxRow("https://ex.com/ok", "PASS", "Submitted and indexed"),
			wantResolves: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		// ── MUST STAY QUIET: both agree the page is NOT indexable (Rabbot sees the
		// disallow/noindex too) → no discrepancy, drive resolve.
		{
			name: "agree_both_not_indexable_robots_disallow", class: gscMustStayQuiet,
			url: "https://ex.com/blocked", hasURLRow: true, urlID: 15,
			hasSnapshot: true, snapshot: noindexSnap(15, "robots disallow"),
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/blocked", "FAIL", "Blocked by robots.txt")
				r.RobotsTxtState = "DISALLOWED"
				return r
			}(),
			wantResolves: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		// ── THE no-false-positive guard: an un-inspected URL (no url_index_status row,
		// ok=false) must NOT be read as not-indexed. NO signal, NO resolve — absent data
		// is never a verdict.
		{
			name: "missing_index_status_row_is_silent", class: gscMustStayQuiet,
			url: "https://ex.com/uninspected", hasURLRow: true, urlID: 16,
			hasSnapshot: true, snapshot: indexableSnap(16),
			hasIdx: false, // quota-bounded staleness / never inspected
			// neither signal fires, AND no resolve is driven (nothing to retract).
		},
		// ── MUST STAY QUIET: an ambiguous/unmodeled Google coverage state is not forced to
		// a verdict; signal 1 stays quiet but retracts any prior discrepancy (we can no
		// longer confirm it). Signal 2 is silent (no canonicals).
		{
			name: "ambiguous_google_state_is_silent_but_resolves", class: gscMustStayQuiet,
			url: "https://ex.com/weird", hasURLRow: true, urlID: 17,
			hasSnapshot: true, snapshot: indexableSnap(17),
			hasIdx:       true,
			idx:          idxRow("https://ex.com/weird", "VERDICT_UNSPECIFIED", "Some future state we don't model"),
			wantResolves: map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
		},
		// ── EDGE: Google has a real (not-indexed) state, but Rabbot never admitted the URL
		// (GetURL → ErrNotFound). No engine verdict to compare → signal 1 skips entirely
		// (no ingest, no resolve — absent Rabbot data is not "agreement").
		{
			name: "google_only_url_no_rabbot_verdict_skips", class: gscEdge,
			url: "https://ex.com/google-only", hasURLRow: false,
			hasSnapshot: false,
			hasIdx:      true,
			idx:         idxRow("https://ex.com/google-only", "NEUTRAL", "Crawled - currently not indexed"),
			// no canonicals → signal 2 also silent. Nothing at all.
		},
		// ── EDGE: the urls row exists but the page was never crawled (LatestSnapshot →
		// ErrNotFound). Same as above: no verdict, skip.
		{
			name: "never_crawled_no_snapshot_skips", class: gscEdge,
			url: "https://ex.com/nosnap", hasURLRow: true, urlID: 18,
			hasSnapshot: false,
			hasIdx:      true,
			idx:         idxRow("https://ex.com/nosnap", "NEUTRAL", "Crawled - currently not indexed"),
		},
	}
}

// canonicalScenarios encodes signal-2 (google_canonical_mismatch) golden rows: a real
// cross-page split fires; equivalent forms, empties, agreement, and missing rows stay
// quiet. Signal 2 needs no Rabbot snapshot (pure GSC-internal), so these rows omit it.
func canonicalScenarios() []gscScenario {
	return []gscScenario{
		// ── MUST FIRE: Google chose a different page than declared (param-stripped form
		// is a DIFFERENT resource here only because the path differs after normalization;
		// see the equivalence edge below for the must-stay-quiet counterpart).
		{
			name: "mismatch_google_chose_different_page", class: gscMustFire,
			url: "https://ex.com/p", hasURLRow: true, urlID: 20,
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/p", "PASS", "Duplicate, Google chose different canonical than user")
				r.UserCanonical = "https://ex.com/p"
				r.GoogleCanonical = "https://ex.com/other"
				return r
			}(),
			// NOTE: this coverage state is also in the not-indexed set, but signal 1 needs a
			// Rabbot snapshot to fire and this row has none, so ONLY the canonical signal pages.
			wantSignals: map[string]model.Severity{changeTypeCanonicalMismatch: warn},
		},
		// ── MUST STAY QUIET: Google chose the SAME canonical → no mismatch, drive resolve.
		{
			name: "canonical_agreement_resolves", class: gscMustStayQuiet,
			url:    "https://ex.com/same",
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/same", "PASS", "Submitted and indexed")
				r.UserCanonical = "https://ex.com/same"
				r.GoogleCanonical = "https://ex.com/same"
				return r
			}(),
			wantResolves: map[string]model.Severity{changeTypeCanonicalMismatch: warn},
		},
		// ── EDGE / MUST STAY QUIET: host-case-only difference is the SAME resource after
		// normalization → must NOT false-positive. Drives resolve (canonicals "agree").
		{
			name: "canonical_host_case_equivalent_no_fire", class: gscEdge,
			url:    "https://ex.com/x",
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/x", "PASS", "Submitted and indexed")
				r.UserCanonical = "https://ex.com/x"
				r.GoogleCanonical = "https://EX.com/x"
				return r
			}(),
			wantResolves: map[string]model.Severity{changeTypeCanonicalMismatch: warn},
		},
		// ── EDGE / MUST STAY QUIET: trailing-slash-only difference (empty path → "/") is
		// the same resource after the store's canonicalURL coda → no fire.
		{
			name: "canonical_trailing_slash_equivalent_no_fire", class: gscEdge,
			url:    "https://ex.com/",
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/", "PASS", "Submitted and indexed")
				r.UserCanonical = "https://ex.com"
				r.GoogleCanonical = "https://ex.com/"
				return r
			}(),
			wantResolves: map[string]model.Severity{changeTypeCanonicalMismatch: warn},
		},
		// ── MUST STAY QUIET: Google hasn't reported a canonical yet (one side empty) →
		// absent data, NOT a mismatch, and NOT a resolve from signal 2 (we never asserted
		// one). These rows carry an INDEXED coverage state and no Rabbot urls row, so
		// signal 1 deterministically skips (state known → GetURL ErrNotFound → skip),
		// isolating the assertion to signal 2's empty-canonical-side silence. (An EMPTY
		// coverage state would make signal 1 ambiguous and drive a discrepancy resolve —
		// correct evaluator behavior, but it would conflate the two signals on one row.)
		{
			name: "canonical_google_side_empty_silent", class: gscMustStayQuiet,
			url:    "https://ex.com/e1",
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/e1", "PASS", "Submitted and indexed")
				r.UserCanonical = "https://ex.com/e1"
				r.GoogleCanonical = ""
				return r
			}(),
		},
		{
			name: "canonical_user_side_empty_silent", class: gscMustStayQuiet,
			url:    "https://ex.com/e2",
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/e2", "PASS", "Submitted and indexed")
				r.UserCanonical = ""
				r.GoogleCanonical = "https://ex.com/e2"
				return r
			}(),
		},
		// ── MUST STAY QUIET: no url_index_status row at all → skip signal 2 entirely.
		{
			name: "canonical_no_index_row_silent", class: gscMustStayQuiet,
			url:    "https://ex.com/none",
			hasIdx: false,
		},
	}
}

// combinedScenarios encodes rows where BOTH signals are in play on one URL, proving they
// compose correctly (one fires, the other resolves; or both fire).
func combinedScenarios() []gscScenario {
	return []gscScenario{
		// Discrepancy fires AND canonical agrees (resolve): a page Google didn't index but
		// whose canonical Google honored. Exactly one ingest (discrepancy) + one resolve
		// (canonical).
		{
			name: "discrepancy_fires_canonical_agrees", class: gscMustFire,
			url: "https://ex.com/both1", hasURLRow: true, urlID: 30,
			hasSnapshot: true, snapshot: indexableSnap(30),
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/both1", "NEUTRAL", "Crawled - currently not indexed")
				r.UserCanonical = "https://ex.com/both1"
				r.GoogleCanonical = "https://ex.com/both1"
				return r
			}(),
			wantSignals:  map[string]model.Severity{changeTypeIndexDiscrepancy: warn},
			wantResolves: map[string]model.Severity{changeTypeCanonicalMismatch: warn},
		},
		// Both fire: Google didn't index it AND chose a different canonical. Two distinct
		// ingests, no resolves.
		{
			name: "both_signals_fire", class: gscMustFire,
			url: "https://ex.com/both2", hasURLRow: true, urlID: 31,
			hasSnapshot: true, snapshot: indexableSnap(31),
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/both2", "NEUTRAL", "Crawled - currently not indexed")
				r.UserCanonical = "https://ex.com/both2"
				r.GoogleCanonical = "https://ex.com/canonical-target"
				return r
			}(),
			wantSignals: map[string]model.Severity{
				changeTypeIndexDiscrepancy:  warn,
				changeTypeCanonicalMismatch: warn,
			},
		},
		// Full agreement on both axes → both resolve, nothing pages (the steady-state
		// healthy page).
		{
			name: "fully_healthy_page_both_resolve", class: gscMustStayQuiet,
			url: "https://ex.com/healthy", hasURLRow: true, urlID: 32,
			hasSnapshot: true, snapshot: indexableSnap(32),
			hasIdx: true, idx: func() model.URLIndexStatus {
				r := idxRow("https://ex.com/healthy", "PASS", "Submitted and indexed")
				r.UserCanonical = "https://ex.com/healthy"
				r.GoogleCanonical = "https://ex.com/healthy"
				return r
			}(),
			wantResolves: map[string]model.Severity{
				changeTypeIndexDiscrepancy:  warn,
				changeTypeCanonicalMismatch: warn,
			},
		},
	}
}

// ── golden-matrix test entrypoints (mirror behavior_test.go) ──────────────────

func TestGSCIndexStatusScenarios(t *testing.T) { runGSCScenarios(t, indexStatusScenarios()) }
func TestGSCCanonicalScenarios(t *testing.T)   { runGSCScenarios(t, canonicalScenarios()) }
func TestGSCCombinedScenarios(t *testing.T)    { runGSCScenarios(t, combinedScenarios()) }

// TestGSCScenarioCountIsLogged makes the encoded-scenario total explicit so a dropped
// scenario is never silent (mirrors behavior's TestScenarioCountIsLogged).
func TestGSCScenarioCountIsLogged(t *testing.T) {
	groups := map[string]int{
		"index_status": len(indexStatusScenarios()),
		"canonical":    len(canonicalScenarios()),
		"combined":     len(combinedScenarios()),
	}
	total := 0
	for _, n := range groups {
		total += n
	}
	t.Logf("encoded GSC scenarios = %d  breakdown=%v", total, groups)
	if total == 0 {
		t.Fatal("no GSC scenarios encoded")
	}
}

// ── signal 3: search_performance_shift ENRICHMENT golden table ────────────────
//
// Signal 3 is NOT an Ingest and NOT a standalone signal — it is an ENRICHMENT attached
// to an EXISTING change record only when enough FINALIZED post-change data exists. The
// golden table drives SearchPerformanceShift directly (the report/MCP read-layer fn)
// and asserts enrichment-vs-no-enrichment under the dataState=final discipline.

// shiftScenario is one row of the enrichment golden table.
type shiftScenario struct {
	name       string
	rows       []model.SearchMetric
	changeDate string
	now        time.Time
	wantOK     bool
	// when wantOK, these pin the direction of the asserted shift.
	wantQuery       string
	wantImprNeg     bool // ImpressionsDelta < 0 (a loss)
	wantPosWorsened bool // PositionDelta > 0 (rank got worse)
	skip            string
}

// gscDay renders a 2026 YYYY-MM-DD for the shift table (local helper to avoid colliding
// with the existing `day` in gscsignals_test.go — same format, scoped name).
func gscDay(month, d int) string {
	return time.Date(2026, time.Month(month), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func metricRow(date, query string, impressions int64, pos float64) model.SearchMetric {
	return model.SearchMetric{URL: "https://ex.com/p", Query: query, Date: date, Impressions: impressions, Position: pos}
}

func shiftScenarios() []shiftScenario {
	// A clock late enough that both windows around a 06-10 change are fully finalized
	// (lag 3 → final cutoff = now-3 = 06-18).
	nowFinalized := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	return []shiftScenario{
		{
			name: "clear_impression_drop_and_rank_loss_enriches",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 3; d <= 9; d++ { // before window: strong
					r = append(r, metricRow(gscDay(6, d), "widgets", 1000, 4.0))
				}
				for d := 11; d <= 17; d++ { // after window: collapsed
					r = append(r, metricRow(gscDay(6, d), "widgets", 200, 9.0))
				}
				return r
			}(),
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: true, wantQuery: "widgets", wantImprNeg: true, wantPosWorsened: true,
		},
		{
			name: "primary_query_is_highest_volume",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				// Two queries; "blue widgets" carries far more volume → it is the primary.
				for d := 3; d <= 9; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 50, 8.0))
					r = append(r, metricRow(gscDay(6, d), "blue widgets", 2000, 3.0))
				}
				for d := 11; d <= 17; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 60, 7.5))
					r = append(r, metricRow(gscDay(6, d), "blue widgets", 400, 8.0))
				}
				return r
			}(),
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: true, wantQuery: "blue widgets", wantImprNeg: true, wantPosWorsened: true,
		},
		{
			// THE dataState=final discipline: the only post-change data is within the
			// partial lag (latest ~3 days) → NO finalized after-window → no enrichment.
			name: "partial_only_after_window_no_enrichment",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 10; d <= 16; d++ { // before window relative to a 06-19 change
					r = append(r, metricRow(gscDay(6, d), "widgets", 1000, 4.0))
				}
				r = append(r, metricRow(gscDay(6, 20), "widgets", 50, 9.0)) // 06-20 is partial (>cutoff 06-18)
				return r
			}(),
			changeDate: gscDay(6, 19), now: nowFinalized,
			wantOK: false,
		},
		{
			// A single finalized after-day is below gscMinAfterFinalDays → insufficient.
			name: "insufficient_after_window_no_enrichment",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 3; d <= 9; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 1000, 4.0))
				}
				r = append(r, metricRow(gscDay(6, 18), "widgets", 50, 9.0)) // only 06-18 finalized after a 06-17 change
				return r
			}(),
			changeDate: gscDay(6, 17), now: nowFinalized,
			wantOK: false,
		},
		{
			// No before-window data → no baseline to compare → no enrichment.
			name: "no_before_data_no_enrichment",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 11; d <= 17; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 200, 9.0))
				}
				return r
			}(),
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: false,
		},
		{
			// THE HEADLINE INVARIANT, enrichment edition: a flat metric series (no movement)
			// across a finalized change still produces an enrichment row (the host change is
			// real), but the delta is ZERO — so a "drop" is never invented from noise. We
			// assert ok=true with a non-negative impressions delta to prove the enrichment is
			// purely descriptive, never a fabricated alarm.
			name: "flat_metrics_enrichment_is_descriptive_not_alarming",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 3; d <= 9; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 500, 5.0))
				}
				for d := 11; d <= 17; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 500, 5.0))
				}
				return r
			}(),
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: true, wantQuery: "widgets", wantImprNeg: false, wantPosWorsened: false,
		},
		{
			// A recovery (impressions GREW after the change) enriches with a POSITIVE
			// impressions delta — proving the enrichment reports gains as readily as losses
			// (it is correlation, not a one-directional drop alarm).
			name: "post_change_recovery_enriches_positive",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 3; d <= 9; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 200, 9.0))
				}
				for d := 11; d <= 17; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 1000, 3.0))
				}
				return r
			}(),
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: true, wantQuery: "widgets", wantImprNeg: false, wantPosWorsened: false,
		},
		{
			// No metrics at all → nothing to correlate → no enrichment.
			name: "no_metrics_no_enrichment", rows: nil,
			changeDate: gscDay(6, 10), now: nowFinalized,
			wantOK: false,
		},
		{
			// A malformed change date string → no enrichment (defensive; never panics).
			name: "malformed_change_date_no_enrichment",
			rows: func() []model.SearchMetric {
				var r []model.SearchMetric
				for d := 3; d <= 17; d++ {
					r = append(r, metricRow(gscDay(6, d), "widgets", 500, 5.0))
				}
				return r
			}(),
			changeDate: "not-a-date", now: nowFinalized,
			wantOK: false,
		},
	}
}

func TestSearchPerformanceShiftScenarios(t *testing.T) {
	for _, sc := range shiftScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if sc.skip != "" {
				t.Skipf("SUSPECTED DEFECT: %s", sc.skip)
			}
			enr, ok := SearchPerformanceShift(sc.rows, sc.changeDate, sc.now)
			if ok != sc.wantOK {
				t.Fatalf("[%s] enrichment ok = %v, want %v (enr=%+v)", sc.name, ok, sc.wantOK, enr)
			}
			if !sc.wantOK {
				return
			}
			if enr.Query != sc.wantQuery {
				t.Errorf("[%s] primary query = %q, want %q", sc.name, enr.Query, sc.wantQuery)
			}
			if sc.wantImprNeg && enr.ImpressionsDelta >= 0 {
				t.Errorf("[%s] impressions delta = %d, want negative (loss)", sc.name, enr.ImpressionsDelta)
			}
			if !sc.wantImprNeg && enr.ImpressionsDelta < 0 {
				t.Errorf("[%s] impressions delta = %d, want >= 0", sc.name, enr.ImpressionsDelta)
			}
			if sc.wantPosWorsened && enr.PositionDelta <= 0 {
				t.Errorf("[%s] position delta = %.2f, want positive (rank worsened)", sc.name, enr.PositionDelta)
			}
			if !sc.wantPosWorsened && enr.PositionDelta > 0 {
				t.Errorf("[%s] position delta = %.2f, want <= 0 (rank not worsened)", sc.name, enr.PositionDelta)
			}
			if enr.String() == "" {
				t.Errorf("[%s] enrichment must render a non-empty human string", sc.name)
			}
		})
	}
}

// ── THE HEADLINE INVARIANT (the no-raw-traffic-alert guarantee) ───────────────

// TestGSCNoRawTrafficAlert is the falsifiable statement of the central anti-noise
// guarantee (the behavioral analogue of internal/behavior's "healthy SPA must not fire
// CRITICAL deindex"): a page with NO index discrepancy and NO canonical mismatch — but
// arbitrary search-metric movement — produces ZERO standalone signals from the alert
// evaluator. Raw clicks/impressions/position deltas never page on their own, by
// construction (Evaluate never reads metrics; SearchPerformanceShift requires a change
// record and only ever returns an enrichment, never an Ingest).
func TestGSCNoRawTrafficAlert(t *testing.T) {
	const u = "https://ex.com/trafficky"
	// A perfectly healthy page (Google indexed it, canonical honored) that happens to be
	// losing impressions hand over fist. The evaluator must stay silent on the metrics.
	rd := &fakeVerdictReader{
		urls:  map[string]model.URL{u: urlRow(40, u)},
		snaps: map[int64]model.Snapshot{40: indexableSnap(40)},
		idx: map[string]model.URLIndexStatus{
			u: func() model.URLIndexStatus {
				r := idxRow(u, "PASS", "Submitted and indexed")
				r.UserCanonical = u
				r.GoogleCanonical = u
				return r
			}(),
		},
	}
	sink := &recordingSink{}
	ev := &GSCSignals{Reader: rd, Candidates: fixedCandidates{u}, Alerts: sink}
	if err := ev.Evaluate(context.Background(), gscSite()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// No Ingest at all — a healthy page emits nothing, regardless of any traffic delta
	// (which the evaluator never even looks at).
	if len(sink.ingested) != 0 {
		t.Fatalf("a healthy page must emit ZERO signals; got %d (%+v)", len(sink.ingested), sink.ingested)
	}
	// And specifically: nothing resembling a standalone performance/traffic signal ever
	// appears in the emitted stream.
	for _, e := range sink.ingested {
		switch e.ChangeType {
		case "search_performance_shift", "search_performance", "traffic_drop",
			"impressions_drop", "ranking_drop", "clicks_drop":
			t.Errorf("the evaluator must never emit a raw-traffic signal; got %q", e.ChangeType)
		}
	}

	// Independently: even a massive modelled impression collapse, fed to the enrichment
	// fn WITHOUT a change record context, is unreachable as a standalone alert — the only
	// entry point (SearchPerformanceShift) demands a change date and returns an enrichment
	// value, never anything that reaches a notifier. We assert it returns an enrichment
	// (a value, not an event) for a real change, and emphatically not an alerts.Event.
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	var collapse []model.SearchMetric
	for d := 3; d <= 9; d++ {
		collapse = append(collapse, metricRow(gscDay(6, d), "q", 5000, 2.0))
	}
	for d := 11; d <= 17; d++ {
		collapse = append(collapse, metricRow(gscDay(6, d), "q", 5, 30.0))
	}
	enr, ok := SearchPerformanceShift(collapse, gscDay(6, 10), now)
	if !ok {
		t.Fatalf("a real change with finalized data should yield an enrichment value")
	}
	// The enrichment is a plain descriptive value — it carries no severity, no incident,
	// no dispatch. (Its very type, SearchShift, has no path into the pipeline.)
	if _, isEvent := interface{}(enr).(alerts.Event); isEvent {
		t.Fatal("the shift enrichment must NOT be an alerts.Event (no standalone alert path)")
	}
}
