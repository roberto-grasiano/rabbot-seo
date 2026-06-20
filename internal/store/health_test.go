package store_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// hsSeed builds a site and returns its id. Each test seeds its own URLs/issues.
func hsSeedSite(t *testing.T, st *store.DB, host string) int64 {
	t.Helper()
	id, err := st.AddSite(context.Background(), model.Site{
		BaseURL: "https://" + host, Name: host, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", host, err)
	}
	return id
}

// hsSeedURL inserts a URL with the given importance and processed state.
// processed=true sets last_checked (the "crawled at least once" marker); false
// leaves it NULL (never crawled, invisible to the masses but counted in known).
func hsSeedURL(t *testing.T, st *store.DB, siteID int64, path string, importance float64, processed bool) int64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	var lc *time.Time
	if processed {
		c := now.Add(-time.Hour)
		lc = &c
	}
	id, err := st.UpsertURL(context.Background(), model.URL{
		SiteID: siteID, URL: "https://x/" + path, FirstSeen: now, LastChecked: lc,
		NextCheckAt: now, Interval: 600, Importance: importance,
		StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	})
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", path, err)
	}
	return id
}

// hsOpenIssue opens an issue on a URL with an explicit impact_points value.
func hsOpenIssue(t *testing.T, st *store.DB, urlID int64, ruleID string, sev model.Severity, impact int, status model.IssueStatus) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := st.UpsertIssue(context.Background(), model.Issue{
		URLID: urlID, RuleID: ruleID, Status: status, Severity: sev,
		ImpactPoints: impact, OpenedAt: now, LastSeenAt: now, Detail: "{}",
	}); err != nil {
		t.Fatalf("UpsertIssue(%s): %v", ruleID, err)
	}
}

// TestComputeHealthScore locks the score model (acceptance criterion 1 a–h).
func TestComputeHealthScore(t *testing.T) {
	ctx := context.Background()

	// (a) no crawled URLs → defined=false.
	t.Run("a_no_crawled_undefined", func(t *testing.T) {
		st := newTestStore(t)
		site := hsSeedSite(t, st, "a.test")
		hsSeedURL(t, st, site, "p", 1.0, false) // known but never processed
		hs, err := st.ComputeHealthScore(ctx, site, nil)
		if err != nil {
			t.Fatalf("ComputeHealthScore: %v", err)
		}
		if hs.Defined {
			t.Fatalf("defined=true, want false (no crawled URLs)")
		}
	})

	// (b) 1 URL importance 1.0, no open issues → 100.0.
	t.Run("b_healthy_100", func(t *testing.T) {
		st := newTestStore(t)
		site := hsSeedSite(t, st, "b.test")
		hsSeedURL(t, st, site, "p", 1.0, true)
		hs, err := st.ComputeHealthScore(ctx, site, nil)
		if err != nil {
			t.Fatalf("ComputeHealthScore: %v", err)
		}
		if !hs.Defined || hs.Score != 100.0 {
			t.Fatalf("score=%v defined=%v, want 100.0/true", hs.Score, hs.Defined)
		}
	})

	// (c) one open critical → 0.0, warning → 50.0, info → 80.0 (severityWeight coupling).
	t.Run("c_severity_coupling", func(t *testing.T) {
		cases := []struct {
			sev    model.Severity
			impact int
			want   float64
		}{
			{model.SeverityCritical, 1000, 0.0},
			{model.SeverityWarning, 500, 50.0},
			{model.SeverityInfo, 200, 80.0},
		}
		for _, c := range cases {
			st := newTestStore(t)
			site := hsSeedSite(t, st, "c-"+string(c.sev)+".test")
			u := hsSeedURL(t, st, site, "p", 1.0, true)
			hsOpenIssue(t, st, u, "r", c.sev, c.impact, model.IssueOpen)
			hs, err := st.ComputeHealthScore(ctx, site, nil)
			if err != nil {
				t.Fatalf("ComputeHealthScore(%s): %v", c.sev, err)
			}
			if !hs.Defined || hs.Score != c.want {
				t.Fatalf("%s: score=%v, want %v", c.sev, hs.Score, c.want)
			}
		}
	})

	// (d) two equal-importance pages, one fully impaired → 50.0.
	t.Run("d_two_pages_one_impaired", func(t *testing.T) {
		st := newTestStore(t)
		site := hsSeedSite(t, st, "d.test")
		u1 := hsSeedURL(t, st, site, "p1", 1.0, true)
		_ = hsSeedURL(t, st, site, "p2", 1.0, true)
		hsOpenIssue(t, st, u1, "r", model.SeverityCritical, 1000, model.IssueOpen)
		hs, err := st.ComputeHealthScore(ctx, site, nil)
		if err != nil {
			t.Fatalf("ComputeHealthScore: %v", err)
		}
		if hs.Score != 50.0 {
			t.Fatalf("score=%v, want 50.0", hs.Score)
		}
	})

	// (e) three criticals on one page score the same as one (page cap).
	t.Run("e_page_cap", func(t *testing.T) {
		st := newTestStore(t)
		site := hsSeedSite(t, st, "e.test")
		u := hsSeedURL(t, st, site, "p", 1.0, true)
		_ = hsSeedURL(t, st, site, "p2", 1.0, true)
		hsOpenIssue(t, st, u, "r1", model.SeverityCritical, 1000, model.IssueOpen)
		hsOpenIssue(t, st, u, "r2", model.SeverityCritical, 1000, model.IssueOpen)
		hsOpenIssue(t, st, u, "r3", model.SeverityCritical, 1000, model.IssueOpen)
		hs, err := st.ComputeHealthScore(ctx, site, nil)
		if err != nil {
			t.Fatalf("ComputeHealthScore: %v", err)
		}
		// deficit capped at cap(u)=1000; max_mass=2000 → score 50.0, same as one.
		if hs.Score != 50.0 {
			t.Fatalf("score=%v, want 50.0 (page cap)", hs.Score)
		}
		if hs.ImpactMass != 1000 {
			t.Fatalf("impact_mass=%d, want 1000 (capped)", hs.ImpactMass)
		}
	})

	// (f) ignored/closed issues do not move the score.
	t.Run("f_ignored_closed_excluded", func(t *testing.T) {
		st := newTestStore(t)
		site := hsSeedSite(t, st, "f.test")
		u := hsSeedURL(t, st, site, "p", 1.0, true)
		hsOpenIssue(t, st, u, "ig", model.SeverityCritical, 1000, model.IssueIgnored)
		hsOpenIssue(t, st, u, "cl", model.SeverityCritical, 1000, model.IssueClosed)
		hs, err := st.ComputeHealthScore(ctx, site, nil)
		if err != nil {
			t.Fatalf("ComputeHealthScore: %v", err)
		}
		if hs.Score != 100.0 {
			t.Fatalf("score=%v, want 100.0 (ignored+closed excluded)", hs.Score)
		}
	})

	// (g) a higher-importance page moves the score more than a low-importance one.
	t.Run("g_importance_weighted", func(t *testing.T) {
		// site high: critical on importance-1.0 page (+ a healthy 1.0 page).
		stHi := newTestStore(t)
		siteHi := hsSeedSite(t, stHi, "g-hi.test")
		uHi := hsSeedURL(t, stHi, siteHi, "p", 1.0, true)
		_ = hsSeedURL(t, stHi, siteHi, "q", 1.0, true)
		hsOpenIssue(t, stHi, uHi, "r", model.SeverityCritical, 1000, model.IssueOpen)
		hsHi, err := stHi.ComputeHealthScore(ctx, siteHi, nil)
		if err != nil {
			t.Fatalf("hi: %v", err)
		}

		// site low: critical on importance-0.2 page (+ a healthy 1.0 page).
		stLo := newTestStore(t)
		siteLo := hsSeedSite(t, stLo, "g-lo.test")
		uLo := hsSeedURL(t, stLo, siteLo, "p", 0.2, true)
		_ = hsSeedURL(t, stLo, siteLo, "q", 1.0, true)
		hsOpenIssue(t, stLo, uLo, "r", model.SeverityCritical, 200, model.IssueOpen)
		hsLo, err := stLo.ComputeHealthScore(ctx, siteLo, nil)
		if err != nil {
			t.Fatalf("lo: %v", err)
		}

		if !(hsHi.Score < hsLo.Score) {
			t.Fatalf("hi-importance score %v should be < lo-importance score %v", hsHi.Score, hsLo.Score)
		}
	})

	// (h) coverage floor: 4/10 processed → undefined; 5/10 → defined (inclusive boundary).
	t.Run("h_coverage_floor", func(t *testing.T) {
		mk := func(host string, processed int) store.HealthScore {
			st := newTestStore(t)
			site := hsSeedSite(t, st, host)
			for i := 0; i < 10; i++ {
				hsSeedURL(t, st, site, host+"-"+itoa(i), 1.0, i < processed)
			}
			hs, err := st.ComputeHealthScore(ctx, site, nil)
			if err != nil {
				t.Fatalf("ComputeHealthScore(%s): %v", host, err)
			}
			return hs
		}
		// max_mass>0 in both cases (processed URLs have importance 1.0).
		four := mk("h4.test", 4)
		if four.Defined {
			t.Fatalf("4/10 processed: defined=true, want false")
		}
		five := mk("h5.test", 5)
		if !five.Defined {
			t.Fatalf("5/10 processed: defined=false, want true (inclusive boundary, ceil(0.5*10)=5)")
		}
		if five.KnownURLs != 10 || five.ProcessedURLs != 5 {
			t.Fatalf("coverage counts known=%d processed=%d, want 10/5", five.KnownURLs, five.ProcessedURLs)
		}
	})
}

// TestComputeHealthScore_Segment is acceptance criterion 2.
func TestComputeHealthScore_Segment(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	site := hsSeedSite(t, st, "seg.test")

	// Segment A: two processed importance-1.0 URLs; a critical hits one.
	a1 := hsSeedURL(t, st, site, "a1", 1.0, true)
	a2 := hsSeedURL(t, st, site, "a2", 1.0, true)
	// Segment B: two processed importance-1.0 URLs, healthy.
	b1 := hsSeedURL(t, st, site, "b1", 1.0, true)
	b2 := hsSeedURL(t, st, site, "b2", 1.0, true)

	ids, err := st.SyncSiteSegments(ctx, site, []model.Segment{
		{SiteID: site, Name: "a", MatchRule: "^/a"},
		{SiteID: site, Name: "b", MatchRule: "^/b"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	for _, u := range []int64{a1, a2} {
		if err := st.SetURLSegments(ctx, u, []int64{ids["a"]}); err != nil {
			t.Fatalf("SetURLSegments(a): %v", err)
		}
	}
	for _, u := range []int64{b1, b2} {
		if err := st.SetURLSegments(ctx, u, []int64{ids["b"]}); err != nil {
			t.Fatalf("SetURLSegments(b): %v", err)
		}
	}

	hsOpenIssue(t, st, a1, "r", model.SeverityCritical, 1000, model.IssueOpen)

	segA := ids["a"]
	segB := ids["b"]

	scoreA, err := st.ComputeHealthScore(ctx, site, &segA)
	if err != nil {
		t.Fatalf("compute segA: %v", err)
	}
	if scoreA.Score != 50.0 { // one of two equal pages impaired
		t.Fatalf("segment A score=%v, want 50.0", scoreA.Score)
	}
	scoreB, err := st.ComputeHealthScore(ctx, site, &segB)
	if err != nil {
		t.Fatalf("compute segB: %v", err)
	}
	if scoreB.Score != 100.0 { // B contains no impaired URL
		t.Fatalf("segment B score=%v, want 100.0", scoreB.Score)
	}
	scoreSite, err := st.ComputeHealthScore(ctx, site, nil)
	if err != nil {
		t.Fatalf("compute site: %v", err)
	}
	// site has 4 equal pages, one impaired → 75.0.
	if scoreSite.Score != 75.0 {
		t.Fatalf("site score=%v, want 75.0", scoreSite.Score)
	}

	// A segment below its own coverage floor is undefined while the site is defined.
	cIDs, err := st.SyncSiteSegments(ctx, site, []model.Segment{
		{SiteID: site, Name: "a", MatchRule: "^/a"},
		{SiteID: site, Name: "b", MatchRule: "^/b"},
		{SiteID: site, Name: "c", MatchRule: "^/c"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments(+c): %v", err)
	}
	// segment c: 3 members, only 1 processed → 1/3 < ceil(0.5*3)=2 → undefined.
	c1 := hsSeedURL(t, st, site, "c1", 1.0, true)
	c2 := hsSeedURL(t, st, site, "c2", 1.0, false)
	c3 := hsSeedURL(t, st, site, "c3", 1.0, false)
	for _, u := range []int64{c1, c2, c3} {
		if err := st.SetURLSegments(ctx, u, []int64{cIDs["c"]}); err != nil {
			t.Fatalf("SetURLSegments(c): %v", err)
		}
	}
	segC := cIDs["c"]
	scoreC, err := st.ComputeHealthScore(ctx, site, &segC)
	if err != nil {
		t.Fatalf("compute segC: %v", err)
	}
	if scoreC.Defined {
		t.Fatalf("segment C defined=true, want false (below coverage floor)")
	}
	siteAfter, err := st.ComputeHealthScore(ctx, site, nil)
	if err != nil {
		t.Fatalf("compute site after c: %v", err)
	}
	if !siteAfter.Defined {
		t.Fatalf("site defined=false after adding mostly-uncrawled segment C, want true")
	}
}

// TestRecordHealthScores is acceptance criterion 3 (+ UTC round-trip).
func TestRecordHealthScores(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	site := hsSeedSite(t, st, "rec.test")

	// Two segments; the rechecked URL is in segment A only.
	a := hsSeedURL(t, st, site, "a", 1.0, true)
	_ = hsSeedURL(t, st, site, "a2", 1.0, true)
	b := hsSeedURL(t, st, site, "b", 1.0, true)
	ids, err := st.SyncSiteSegments(ctx, site, []model.Segment{
		{SiteID: site, Name: "a", MatchRule: "^/a"},
		{SiteID: site, Name: "b", MatchRule: "^/b"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	if err := st.SetURLSegments(ctx, a, []int64{ids["a"]}); err != nil {
		t.Fatalf("SetURLSegments(a): %v", err)
	}
	if err := st.SetURLSegments(ctx, b, []int64{ids["b"]}); err != nil {
		t.Fatalf("SetURLSegments(b): %v", err)
	}

	now := time.Now().UTC()

	// First record: site scope + segment A (contains the rechecked URL `a`).
	// Segment B does not contain `a`, so it must NOT be written.
	if err := st.RecordHealthScores(ctx, site, a, now); err != nil {
		t.Fatalf("RecordHealthScores #1: %v", err)
	}
	if got := countHealthRows(t, st, site, nil); got != 1 {
		t.Fatalf("site rows after #1 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["a"])); got != 1 {
		t.Fatalf("segA rows after #1 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["b"])); got != 0 {
		t.Fatalf("segB rows after #1 = %d, want 0 (does not contain rechecked URL)", got)
	}

	// Second record, unchanged state → no new rows (write-on-change).
	if err := st.RecordHealthScores(ctx, site, a, now.Add(time.Minute)); err != nil {
		t.Fatalf("RecordHealthScores #2: %v", err)
	}
	if got := countHealthRows(t, st, site, nil); got != 1 {
		t.Fatalf("site rows after unchanged #2 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["a"])); got != 1 {
		t.Fatalf("segA rows after unchanged #2 = %d, want 1", got)
	}

	// Open an issue on `a`, then record → exactly one new row per affected scope.
	hsOpenIssue(t, st, a, "r", model.SeverityCritical, 1000, model.IssueOpen)
	if err := st.RecordHealthScores(ctx, site, a, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordHealthScores #3: %v", err)
	}
	if got := countHealthRows(t, st, site, nil); got != 2 {
		t.Fatalf("site rows after state change = %d, want 2", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["a"])); got != 2 {
		t.Fatalf("segA rows after state change = %d, want 2", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["b"])); got != 0 {
		t.Fatalf("segB rows still = %d, want 0", got)
	}

	// computed_at round-trips as UTC.
	series, err := st.HealthScoreSeries(ctx, site, nil, time.Time{})
	if err != nil {
		t.Fatalf("HealthScoreSeries: %v", err)
	}
	if len(series) == 0 {
		t.Fatalf("empty series")
	}
	for _, p := range series {
		if p.ComputedAt.Location() != time.UTC {
			t.Fatalf("computed_at location = %v, want UTC", p.ComputedAt.Location())
		}
	}

	// Below the coverage floor: nothing is persisted (series starts at first defined).
	cold := hsSeedSite(t, st, "cold.test")
	for i := 0; i < 10; i++ {
		hsSeedURL(t, st, cold, "cold-"+itoa(i), 1.0, i < 4) // 4/10 < floor
	}
	coldURL := hsSeedURL(t, st, cold, "cold-target", 1.0, true)
	if err := st.RecordHealthScores(ctx, cold, coldURL, now); err != nil {
		t.Fatalf("RecordHealthScores(cold): %v", err)
	}
	if got := countHealthRows(t, st, cold, nil); got != 0 {
		t.Fatalf("cold-site rows = %d, want 0 (below coverage floor)", got)
	}
}

// TestRecordSiteHealthScores covers the A7-coordination seam: a re-segmentation
// changes membership wholesale, so reconcile records a site-scoped event — the
// whole site PLUS every segment of the site (not just the segments containing one
// URL). Write-on-change still holds, and below-floor scopes persist nothing.
func TestRecordSiteHealthScores(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	site := hsSeedSite(t, st, "site-rec.test")

	a := hsSeedURL(t, st, site, "a", 1.0, true)
	b := hsSeedURL(t, st, site, "b", 1.0, true)
	ids, err := st.SyncSiteSegments(ctx, site, []model.Segment{
		{SiteID: site, Name: "a", MatchRule: "^/a"},
		{SiteID: site, Name: "b", MatchRule: "^/b"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	if err := st.SetURLSegments(ctx, a, []int64{ids["a"]}); err != nil {
		t.Fatalf("SetURLSegments(a): %v", err)
	}
	if err := st.SetURLSegments(ctx, b, []int64{ids["b"]}); err != nil {
		t.Fatalf("SetURLSegments(b): %v", err)
	}

	now := time.Now().UTC()

	// One site-scoped record writes the site row AND a row for EVERY segment of the
	// site (a re-segmentation moved membership wholesale, so all segments may have
	// changed) — unlike RecordHealthScores, which only touches segments of one URL.
	if err := st.RecordSiteHealthScores(ctx, site, now); err != nil {
		t.Fatalf("RecordSiteHealthScores #1: %v", err)
	}
	if got := countHealthRows(t, st, site, nil); got != 1 {
		t.Fatalf("site rows after #1 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["a"])); got != 1 {
		t.Fatalf("segA rows after #1 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["b"])); got != 1 {
		t.Fatalf("segB rows after #1 = %d, want 1 (site-scoped records ALL segments)", got)
	}

	// Unchanged state → no new rows (write-on-change holds for every scope).
	if err := st.RecordSiteHealthScores(ctx, site, now.Add(time.Minute)); err != nil {
		t.Fatalf("RecordSiteHealthScores #2: %v", err)
	}
	if got := countHealthRows(t, st, site, nil); got != 1 {
		t.Fatalf("site rows after unchanged #2 = %d, want 1", got)
	}
	if got := countHealthRows(t, st, site, ptr(ids["b"])); got != 1 {
		t.Fatalf("segB rows after unchanged #2 = %d, want 1", got)
	}

	// Below-floor scope persists nothing.
	cold := hsSeedSite(t, st, "site-cold.test")
	for i := 0; i < 10; i++ {
		hsSeedURL(t, st, cold, "c-"+itoa(i), 1.0, i < 4) // 4/10 < floor
	}
	if err := st.RecordSiteHealthScores(ctx, cold, now); err != nil {
		t.Fatalf("RecordSiteHealthScores(cold): %v", err)
	}
	if got := countHealthRows(t, st, cold, nil); got != 0 {
		t.Fatalf("cold-site rows = %d, want 0 (below coverage floor)", got)
	}
}

// TestHealthScoreExplainability is acceptance criterion 9: every persisted row's
// score is recomputable from its own integers, and Σ per-URL capped deficits ==
// impact_mass. breakdown stores UNCAPPED per-rule mass (distinct from the capped
// score math).
func TestHealthScoreExplainability(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	site := hsSeedSite(t, st, "explain.test")

	u1 := hsSeedURL(t, st, site, "p1", 1.0, true)
	u2 := hsSeedURL(t, st, site, "p2", 0.5, true)
	_ = hsSeedURL(t, st, site, "p3", 1.0, true)
	// u1: three criticals (uncapped mass 3000, capped deficit 1000).
	hsOpenIssue(t, st, u1, "r1", model.SeverityCritical, 1000, model.IssueOpen)
	hsOpenIssue(t, st, u1, "r2", model.SeverityCritical, 1000, model.IssueOpen)
	hsOpenIssue(t, st, u1, "r3", model.SeverityCritical, 1000, model.IssueOpen)
	// u2: one warning (mass 250, cap=500 → deficit 250).
	hsOpenIssue(t, st, u2, "r1", model.SeverityWarning, 250, model.IssueOpen)

	hs, err := st.ComputeHealthScore(ctx, site, nil)
	if err != nil {
		t.Fatalf("ComputeHealthScore: %v", err)
	}

	// Σ capped deficits: u1 min(3000,1000)=1000; u2 min(250,500)=250; u3 0 → 1250.
	if hs.ImpactMass != 1250 {
		t.Fatalf("impact_mass=%d, want 1250 (Σ capped deficits)", hs.ImpactMass)
	}
	// max_mass: cap u1=1000, u2=500, u3=1000 → 2500.
	if hs.MaxMass != 2500 {
		t.Fatalf("max_mass=%d, want 2500", hs.MaxMass)
	}
	// score recomputable from the row's own integers.
	want := 100 * (1 - float64(hs.ImpactMass)/float64(hs.MaxMass))
	if math.Abs(hs.Score-want) > 1e-9 {
		t.Fatalf("score=%v, recomputed=%v", hs.Score, want)
	}

	// breakdown holds UNCAPPED per-rule mass (ranking attribution), NOT the capped
	// score math: r1 = 1000+250 = 1250, r2 = 1000, r3 = 1000.
	var bd map[string]int
	if err := json.Unmarshal([]byte(hs.Breakdown), &bd); err != nil {
		t.Fatalf("breakdown not JSON: %v (%q)", err, hs.Breakdown)
	}
	if bd["r1"] != 1250 || bd["r2"] != 1000 || bd["r3"] != 1000 {
		t.Fatalf("breakdown=%v, want uncapped r1=1250 r2=1000 r3=1000", bd)
	}
	// The capped impact_mass (1250) differs from the total uncapped breakdown mass
	// (3250) — the distinction the ADR records.
	total := 0
	for _, v := range bd {
		total += v
	}
	if total == hs.ImpactMass {
		t.Fatalf("uncapped breakdown total (%d) must differ from capped impact_mass (%d)", total, hs.ImpactMass)
	}
}

// ── small local helpers ─────────────────────────────────────────────────────

func ptr[T any](v T) *T { return &v }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// countHealthRows counts persisted health_scores rows for a scope. segmentID nil
// counts whole-site rows (segment_id IS NULL).
func countHealthRows(t *testing.T, st *store.DB, siteID int64, segmentID *int64) int {
	t.Helper()
	var n int
	var err error
	if segmentID == nil {
		err = st.Read().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM health_scores WHERE site_id = ? AND segment_id IS NULL`, siteID).Scan(&n)
	} else {
		err = st.Read().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM health_scores WHERE site_id = ? AND segment_id = ?`, siteID, *segmentID).Scan(&n)
	}
	if err != nil {
		t.Fatalf("countHealthRows: %v", err)
	}
	return n
}
