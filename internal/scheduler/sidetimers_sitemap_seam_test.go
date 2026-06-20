package scheduler

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// fakeCollector is a static SitemapCollector used to pin the seam contract: a
// SitemapCollector returns a SitemapCollection exposing exactly the fields the
// watch consumes (Entries/SeedURL/SeedStatus/Incomplete/Admitted).
type fakeCollector struct {
	col SitemapCollection
	err error
}

func (f fakeCollector) CollectAndSeed(_ context.Context, _ model.Site) (SitemapCollection, error) {
	return f.col, f.err
}

// TestSitemapCollectionShape pins the public field set of SitemapCollection (the
// snapshot/diff/reconcile inputs) so a future refactor cannot silently drop one.
func TestSitemapCollectionShape(t *testing.T) {
	t.Parallel()
	col := SitemapCollection{
		Entries:    []SitemapEntry{{Loc: "https://ex.com/a", Priority: 0.5}},
		SeedURL:    "https://ex.com/sitemap.xml",
		SeedStatus: 200,
		Incomplete: true,
		Admitted:   3,
	}
	if len(col.Entries) != 1 || col.Entries[0].Loc != "https://ex.com/a" {
		t.Errorf("Entries not carried: %+v", col.Entries)
	}
	if col.SeedURL != "https://ex.com/sitemap.xml" {
		t.Errorf("SeedURL = %q", col.SeedURL)
	}
	if col.SeedStatus != 200 {
		t.Errorf("SeedStatus = %d", col.SeedStatus)
	}
	if !col.Incomplete {
		t.Errorf("Incomplete should round-trip true")
	}
	if col.Admitted != 3 {
		t.Errorf("Admitted = %d, want 3", col.Admitted)
	}
}

// TestSitemapCollectorInterface proves the interface is satisfiable by a plain
// value and returns the collection verbatim (the seam RefreshSitemap depends on).
func TestSitemapCollectorInterface(t *testing.T) {
	t.Parallel()
	var c SitemapCollector = fakeCollector{col: SitemapCollection{SeedStatus: 404, Admitted: 1}}
	got, err := c.CollectAndSeed(context.Background(), model.Site{ID: 7})
	if err != nil {
		t.Fatalf("CollectAndSeed: %v", err)
	}
	if got.SeedStatus != 404 || got.Admitted != 1 {
		t.Errorf("collection not returned verbatim: %+v", got)
	}
}
