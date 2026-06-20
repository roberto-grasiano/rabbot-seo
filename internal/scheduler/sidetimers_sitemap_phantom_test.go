package scheduler

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestRefreshSitemapIncompletePhantomRecovery reproduces the finding: a transient
// incomplete pass persists a PARTIAL-set hash as the snapshot-of-record, so the
// next COMPLETE pass diffs the full set against the partial baseline and emits a
// phantom "recovery" sitemap_xml event.
//
// Sequence: complete {a,b,c} -> incomplete {a} -> complete {a,b,c}.
func TestRefreshSitemapIncompletePhantomRecovery(t *testing.T) {
	site := model.Site{ID: 1, BaseURL: "https://ex.com"}
	full := []string{"https://ex.com/a", "https://ex.com/b", "https://ex.com/c"}
	us := &fakeURLStore{}

	countSetEvents := func(ing *fakeIngestor) []alerts.Event {
		var out []alerts.Event
		for _, e := range ing.events {
			if e.ChangeType == "sitemap_xml" {
				out = append(out, e)
			}
		}
		return out
	}

	pass := func(fs *fakeFileStore, col SitemapCollection) *fakeIngestor {
		ing := &fakeIngestor{}
		st := newSitemapTimer(fs, us, col, ing)
		if err := st.RefreshSitemap(context.Background(), site); err != nil {
			t.Fatalf("RefreshSitemap: %v", err)
		}
		return ing
	}

	fs := &fakeFileStore{}

	// Pass 1: complete {a,b,c} — baseline, alert-silent.
	ing1 := pass(fs, SitemapCollection{Entries: entries(full...), SeedStatus: 200})
	if n := len(countSetEvents(ing1)); n != 0 {
		t.Fatalf("pass 1 (baseline) should be silent, got %d sitemap_xml events", n)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("pass 1 must persist a snapshot, saved %d", len(fs.saved))
	}

	// Pass 2: incomplete {a} — a child sitemap 500'd, only /a came back.
	ing2 := pass(fs, SitemapCollection{Entries: entries("https://ex.com/a"), SeedStatus: 200, Incomplete: true})
	set2 := countSetEvents(ing2)
	t.Logf("pass 2 (incomplete {a}) sitemap_xml events: %d", len(set2))
	for _, e := range set2 {
		t.Logf("  pass2 event: before=%q after=%q", e.Before, e.After)
	}
	// Pass 2 is a partial read: it must NOT emit a sitemap_xml set-change event
	// (spec lines 47-48 — an incomplete collection must never masquerade as a mass
	// URL drop).
	if len(set2) != 0 {
		t.Errorf("PHANTOM DROP: pass 2 (incomplete {a}) emitted %d sitemap_xml event(s); want 0. Event: before=%q after=%q",
			len(set2), set2[0].Before, set2[0].After)
	}
	// Observe what the incomplete snapshot recorded.
	if len(fs.saved) >= 2 {
		d := parsedSitemapDoc(t, fs.saved[len(fs.saved)-1].ParsedEntries)
		t.Logf("incomplete snapshot-of-record: count=%d incomplete=%v hash=%q",
			d.Count, d.Incomplete, fs.saved[len(fs.saved)-1].ContentSHA256)
		if d.Count != 1 {
			t.Logf("NOTE: incomplete snapshot stored count=%d (not the partial 1)", d.Count)
		}
	}

	// Pass 3: complete {a,b,c} again — the child sitemap recovered.
	ing3 := pass(fs, SitemapCollection{Entries: entries(full...), SeedStatus: 200})
	set3 := countSetEvents(ing3)
	t.Logf("pass 3 (complete {a,b,c} again) sitemap_xml events: %d", len(set3))
	for _, e := range set3 {
		t.Logf("  pass3 event: before=%q after=%q", e.Before, e.After)
	}

	// The finding: pass 3 emits a phantom recovery sitemap_xml event because the
	// snapshot-of-record from pass 2 is the partial-set hash, not the last complete
	// set. {a,b,c} == {a,b,c} (pass 1 == pass 3) — nothing actually changed across
	// complete passes, so pass 3 should be SILENT. If it fires, the bug is real.
	if len(set3) != 0 {
		t.Errorf("PHANTOM RECOVERY: pass 3 (complete set identical to pass 1) emitted %d sitemap_xml event(s); want 0. Event: before=%q after=%q",
			len(set3), set3[0].Before, set3[0].After)
	}
}

// TestRefreshSitemapFirstPassIncompleteNoPhantomDrop covers the !ok variant of
// the phantom-recovery guard (PR #63 review finding): when the FIRST-ever
// collection for a site is incomplete, there is no prior hash to carry forward,
// so the partial hash must not be persisted as the baseline at all. The next
// complete pass is the true baseline (alert-silent), and diffing starts from it.
//
// Sequence: incomplete {a} (empty store) -> complete {a,b,c} -> complete {a,b}.
func TestRefreshSitemapFirstPassIncompleteNoPhantomDrop(t *testing.T) {
	site := model.Site{ID: 1, BaseURL: "https://ex.com"}
	full := []string{"https://ex.com/a", "https://ex.com/b", "https://ex.com/c"}
	us := &fakeURLStore{}

	countSetEvents := func(ing *fakeIngestor) []alerts.Event {
		var out []alerts.Event
		for _, e := range ing.events {
			if e.ChangeType == "sitemap_xml" {
				out = append(out, e)
			}
		}
		return out
	}

	pass := func(fs *fakeFileStore, col SitemapCollection) *fakeIngestor {
		ing := &fakeIngestor{}
		st := newSitemapTimer(fs, us, col, ing)
		if err := st.RefreshSitemap(context.Background(), site); err != nil {
			t.Fatalf("RefreshSitemap: %v", err)
		}
		return ing
	}

	fs := &fakeFileStore{}

	// Pass 1: FIRST-ever pass is incomplete {a} — a child sitemap 500'd during
	// initial setup. No baseline exists; the partial read must not become one.
	ing1 := pass(fs, SitemapCollection{Entries: entries("https://ex.com/a"), SeedStatus: 200, Incomplete: true})
	if n := len(countSetEvents(ing1)); n != 0 {
		t.Fatalf("pass 1 (first, incomplete) should be silent, got %d sitemap_xml events", n)
	}
	if len(fs.saved) != 0 {
		t.Errorf("PARTIAL BASELINE: pass 1 (first, incomplete) persisted %d snapshot(s); want 0 — the partial hash must not become the diff baseline", len(fs.saved))
	}

	// Pass 2: first COMPLETE pass {a,b,c} — this is the true baseline: persisted,
	// alert-silent.
	ing2 := pass(fs, SitemapCollection{Entries: entries(full...), SeedStatus: 200})
	set2 := countSetEvents(ing2)
	if len(set2) != 0 {
		t.Errorf("PHANTOM DROP: pass 2 (first complete) emitted %d sitemap_xml event(s); want 0 (true baseline). Event: before=%q after=%q",
			len(set2), set2[0].Before, set2[0].After)
	}
	if len(fs.saved) == 0 {
		t.Fatalf("pass 2 (first complete) must persist the baseline snapshot")
	}

	// Pass 3: complete {a,b} — a REAL drop of /c. Diffing from the pass-2 baseline
	// must fire exactly one set-change event (proves the baseline took effect).
	ing3 := pass(fs, SitemapCollection{Entries: entries("https://ex.com/a", "https://ex.com/b"), SeedStatus: 200})
	set3 := countSetEvents(ing3)
	if len(set3) != 1 {
		t.Errorf("pass 3 (real drop) emitted %d sitemap_xml event(s); want exactly 1", len(set3))
	}
}
