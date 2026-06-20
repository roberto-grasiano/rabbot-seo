package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// stubBlastRadius builds a WithBlastRadius func that returns fixed counts. ok
// controls whether the page is "in the graph"; when false the enrichment leaves
// After untouched. calls counts invocations so a test can assert the seam is
// consulted only for the http_status >= 400 case.
type stubBlastRadius struct {
	inlinks, high int
	ok            bool
	calls         int
}

func (s *stubBlastRadius) fn(_ context.Context, _ int64, _ string) (int, int, bool) {
	s.calls++
	return s.inlinks, s.high, s.ok
}

// TestProcessFetchEnrichesCriticalHTTPStatus (criterion 10) proves a 200->404
// transition on a page with 3 inlinks (1 high-importance) yields a dispatched
// http_status alert whose After contains "linked from 3 pages (1 high-importance)".
// The 404 surfaces here via the change-stream loop (there is a prior 200 snapshot,
// so http_status is a diff field) — and also potentially the rule bridge; either
// way exactly one http_status event is dispatched and it must carry the suffix.
func TestProcessFetchEnrichesCriticalHTTPStatus(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubBlastRadius{inlinks: 3, high: 1, ok: true}
	deps := &fakeProcDeps{
		// status_regression also opens a finding; with dedup it must still be ONE
		// event, and it must carry the suffix regardless of which path emitted it.
		newFindings: []NewFinding{{Field: "http_status", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 404, Indexable: false, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now }, WithBlastRadius(stub.fn))
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	var httpEvents []string
	for _, e := range deps.ingested {
		if e.ChangeType == "http_status" {
			httpEvents = append(httpEvents, e.After)
		}
	}
	if len(httpEvents) != 1 {
		t.Fatalf("expected exactly one http_status alert (dedup), got %d: %+v", len(httpEvents), deps.ingested)
	}
	const want = "linked from 3 pages (1 high-importance)"
	if !strings.Contains(httpEvents[0], want) {
		t.Errorf("http_status After = %q, want it to contain %q", httpEvents[0], want)
	}
}

// TestProcessFetchEnrichesFirstCrawl404 (criterion 10) proves the enrichment also
// reaches a page broken on its FIRST crawl: with no prior snapshot the 404 reaches
// Slack ONLY via the rule bridge (no diff -> no change-stream event), and the bridge
// emit site must enrich identically.
func TestProcessFetchEnrichesFirstCrawl404(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubBlastRadius{inlinks: 7, high: 2, ok: true}
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{Field: "http_status", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// First crawl: zero oldSnap. A 404 is breakage regardless of baseline.
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 404, Indexable: false, ContentSHA256: "a"}

	p := NewProcessor(deps, 4, func() time.Time { return now }, WithBlastRadius(stub.fn))
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, model.Snapshot{}, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	found := ""
	for _, e := range deps.ingested {
		if e.ChangeType == "http_status" {
			found = e.After
		}
	}
	if found == "" {
		t.Fatalf("expected a bridged http_status alert on a first-crawl 404; got %+v", deps.ingested)
	}
	const want = "linked from 7 pages (2 high-importance)"
	if !strings.Contains(found, want) {
		t.Errorf("first-crawl http_status After = %q, want it to contain %q", found, want)
	}
}

// TestProcessFetchNoEnrichWhenGraphDisabled (criterion 10) proves that with no
// BlastRadius func wired (graph disabled / scope-gated out) the suffix is absent —
// the alert After is unchanged. This is the OFF arm: it guards against the suffix
// leaking in unconditionally.
func TestProcessFetchNoEnrichWhenGraphDisabled(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{
		newFindings: []NewFinding{{Field: "http_status", Severity: model.SeverityCritical}},
	}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 404, Indexable: false, ContentSHA256: "a", ContentSimhash: 0x01}

	// No WithBlastRadius option: p.blastRadius is nil.
	p := NewProcessor(deps, 4, func() time.Time { return now })
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, e := range deps.ingested {
		if e.ChangeType == "http_status" && strings.Contains(e.After, "linked from") {
			t.Errorf("no enrichment expected when graph disabled, got After=%q", e.After)
		}
	}
}

// TestProcessFetchNilBlastRadiusNoPanic (criterion 10) proves a nil BlastRadius func
// causes no panic on the critical http_status path (it is the same nil-disabled arm
// as the prior test, asserted explicitly as the panic-safety contract).
func TestProcessFetchNilBlastRadiusNoPanic(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 500, Indexable: false, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now }) // nil blastRadius
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch must not error with a nil BlastRadius func: %v", err)
	}
}

// TestProcessFetchNoEnrichOnHealthyStatus proves the suffix is appended ONLY for a
// broken page (status >= 400): a healthy 200 page whose other fields change must NOT
// gain the blast-radius suffix, and the seam must not even be consulted for a non-
// http_status / sub-400 event. This guards the gate against firing on every alert.
func TestProcessFetchNoEnrichOnHealthyStatus(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubBlastRadius{inlinks: 9, high: 9, ok: true}
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// A title change on a healthy 200 page: emits a warning title event, no http_status.
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "New", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now }, WithBlastRadius(stub.fn))
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, e := range deps.ingested {
		if strings.Contains(e.After, "linked from") {
			t.Errorf("a healthy 200 page must not be enriched, got After=%q on %q", e.After, e.ChangeType)
		}
	}
	// The seam is gated on http_status >= 400, so it must not be consulted at all here.
	if stub.calls != 0 {
		t.Errorf("BlastRadius seam must not be consulted for a healthy/non-http_status event, got %d calls", stub.calls)
	}
}

// TestProcessFetchNoEnrichWhenPageNotInGraph proves ok=false from the seam (the page
// is not yet in the graph) leaves After unchanged — the suffix is not emitted with
// zero/garbage counts.
func TestProcessFetchNoEnrichWhenPageNotInGraph(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubBlastRadius{inlinks: 0, high: 0, ok: false}
	deps := &fakeProcDeps{}
	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	old := model.Snapshot{ID: 1, URLID: 5, Title: "T", HTTPStatus: 200, Indexable: true, ContentSHA256: "a", ContentSimhash: 0x01}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "T", HTTPStatus: 404, Indexable: false, ContentSHA256: "a", ContentSimhash: 0x01}

	p := NewProcessor(deps, 4, func() time.Time { return now }, WithBlastRadius(stub.fn))
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
	for _, e := range deps.ingested {
		if e.ChangeType == "http_status" && strings.Contains(e.After, "linked from") {
			t.Errorf("ok=false must leave After unchanged, got %q", e.After)
		}
	}
	if stub.calls == 0 {
		t.Error("the seam SHOULD be consulted for a 404 http_status event (then returns ok=false)")
	}
}
