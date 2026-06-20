package diff

import (
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func fieldMap(changes []model.Change) map[string]model.Change {
	m := make(map[string]model.Change, len(changes))
	for _, c := range changes {
		m[c.Field] = c
	}
	return m
}

func TestCompareEmitsFieldChanges(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := model.Snapshot{
		ID:              1,
		Title:           "Old Title",
		MetaDescription: "old desc",
		Canonical:       "https://example.com/a",
		MetaRobots:      "index,follow",
		HTTPStatus:      200,
		Indexable:       true,
		ContentSimhash:  0x00,
	}
	new := model.Snapshot{
		ID:              42,
		URLID:           7,
		Title:           "New Title",
		MetaDescription: "old desc", // unchanged
		Canonical:       "https://example.com/b",
		MetaRobots:      "noindex,follow",
		HTTPStatus:      200,
		Indexable:       false,
		ContentSimhash:  0xFFFFFFFFFFFFFFFF, // substantive
	}

	changes := Compare(new, old, DefaultSimhashThreshold, now)
	byField := fieldMap(changes)

	if _, ok := byField["meta_description"]; ok {
		t.Errorf("unchanged meta_description must not produce a change")
	}
	title, ok := byField["title"]
	if !ok {
		t.Fatalf("expected a title change")
	}
	if title.OldValue != "Old Title" || title.NewValue != "New Title" {
		t.Errorf("title change = %q->%q", title.OldValue, title.NewValue)
	}
	if title.URLID != 7 || title.SnapshotID != 42 || !title.DetectedAt.Equal(now) {
		t.Errorf("change metadata wrong: url=%d snap=%d at=%v", title.URLID, title.SnapshotID, title.DetectedAt)
	}
	if idx, ok := byField["indexable"]; !ok || idx.OldValue != "true" || idx.NewValue != "false" {
		t.Errorf("expected indexable true->false, got %+v ok=%v", idx, ok)
	}
	if _, ok := byField["canonical"]; !ok {
		t.Errorf("expected canonical change")
	}
	if _, ok := byField["meta_robots"]; !ok {
		t.Errorf("expected meta_robots change")
	}
}

// TestCompareEmitsRenderModeChange (A8) guards that a render-mode FLIP is recorded
// as a substantive change-history event so the render path's classification shows up
// on `rabbot report` / `summarize_changes` like any other SEO signal flip. The field
// is typed (model.RenderMode), so Compare must stringify both sides into the generic
// Change.OldValue/NewValue. A baseline first crawl (Old.ID==0 && empty hash) emits
// nothing — but here Old is a real prior snapshot, so the flip must surface.
func TestCompareEmitsRenderModeChange(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	old := model.Snapshot{ID: 1, URLID: 7, RenderMode: model.RenderHydrated, ContentSHA256: "a"}
	new := model.Snapshot{ID: 2, URLID: 7, RenderMode: model.RenderClientShell, ContentSHA256: "a"}

	changes := Compare(new, old, DefaultSimhashThreshold, now)
	c, ok := fieldMap(changes)["render_mode"]
	if !ok {
		t.Fatalf("expected a render_mode change, got %+v", changes)
	}
	if c.OldValue != "hydrated" || c.NewValue != "client_shell" {
		t.Errorf("render_mode change = %q->%q, want hydrated->client_shell", c.OldValue, c.NewValue)
	}
	if c.ChangeClass != model.ChangeSubstantive {
		t.Errorf("render_mode change class = %v, want substantive (it is an SEO signal flip)", c.ChangeClass)
	}
	if c.URLID != 7 || c.SnapshotID != 2 || !c.DetectedAt.Equal(now) {
		t.Errorf("change metadata wrong: url=%d snap=%d at=%v", c.URLID, c.SnapshotID, c.DetectedAt)
	}
}

// TestCompareDoesNotDiffRenderModeUnchanged (A8) — a render_mode that did not flip
// (steady state, incl. both sides "unknown"/"" for pre-A8 rows) must emit nothing.
func TestCompareDoesNotDiffRenderModeUnchanged(t *testing.T) {
	now := time.Now()
	for _, rm := range []model.RenderMode{model.RenderServerRendered, model.RenderUnknown, ""} {
		old := model.Snapshot{ID: 1, RenderMode: rm, ContentSHA256: "a"}
		new := model.Snapshot{ID: 2, RenderMode: rm, ContentSHA256: "a"}
		if _, ok := fieldMap(Compare(new, old, DefaultSimhashThreshold, now))["render_mode"]; ok {
			t.Errorf("unchanged render_mode=%q must not produce a change", rm)
		}
	}
}

// TestCompareDoesNotDiffExtractionSource (A8) — extraction_source is provenance
// metadata (e.g. "dom", "dom+next_data"), NOT an SEO signal. It must NOT be diffed:
// like canonical_type there is no severityForField case for it, so an emitted event
// would mis-route to SeverityInfo and would be pure noise. Guard the producer
// invariant even when the two snapshots' sources disagree.
func TestCompareDoesNotDiffExtractionSource(t *testing.T) {
	now := time.Now()
	old := model.Snapshot{ID: 1, ExtractionSource: "dom", ContentSHA256: "a"}
	new := model.Snapshot{ID: 2, ExtractionSource: "dom+next_data", ContentSHA256: "a"}
	if _, ok := fieldMap(Compare(new, old, DefaultSimhashThreshold, now))["extraction_source"]; ok {
		t.Errorf("extraction_source is provenance, must not be diffed/emitted; got changes=%+v",
			Compare(new, old, DefaultSimhashThreshold, now))
	}
}

func TestCompareDoesNotDiffCanonicalType(t *testing.T) {
	// extract hard-codes snap.CanonicalType = "link" with no other write path, so
	// in production old==new always holds. canonical_type must NOT be emitted as a
	// diffed field: there is no severityForField case for it, so emitting it would
	// mis-route the (future) event to SeverityInfo. Guard the producer/consumer
	// invariant — even when the two snapshots disagree, no canonical_type change
	// may be produced.
	now := time.Now()
	old := model.Snapshot{ID: 1, CanonicalType: "link", ContentSHA256: "a"}
	new := model.Snapshot{ID: 2, CanonicalType: "self", ContentSHA256: "a"}
	changes := Compare(new, old, DefaultSimhashThreshold, now)
	if _, ok := fieldMap(changes)["canonical_type"]; ok {
		t.Errorf("canonical_type must not be diffed/emitted (extract hard-codes it); got changes=%+v", changes)
	}
}

func TestCompareContentChangeClass(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		oldHash uint64
		newHash uint64
		want    model.ChangeClass
		emitted bool
	}{
		{"no content change", 0xAB, 0xAB, "", false},
		{"cosmetic", 0x01, 0x03, model.ChangeCosmetic, true},
		{"substantive", 0x01, 0xFFFF, model.ChangeSubstantive, true},
		// A zero SimHash on either side means "unknown" (e.g. empty body); a
		// content-hash change must be treated as substantive, not cosmetic.
		{"zero old simhash is substantive", 0x00, 0x01, model.ChangeSubstantive, true},
		{"zero new simhash is substantive", 0x01, 0x00, model.ChangeSubstantive, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := model.Snapshot{ContentSimhash: tc.oldHash, ContentSHA256: "a"}
			sha := "a"
			if tc.emitted {
				sha = "b"
			}
			new := model.Snapshot{ContentSimhash: tc.newHash, ContentSHA256: sha}
			changes := Compare(new, old, DefaultSimhashThreshold, now)
			got, ok := fieldMap(changes)["content"]
			if ok != tc.emitted {
				t.Fatalf("content change emitted=%v, want %v (changes=%+v)", ok, tc.emitted, changes)
			}
			if tc.emitted && got.ChangeClass != tc.want {
				t.Errorf("content change class = %q, want %q", got.ChangeClass, tc.want)
			}
		})
	}
}

func TestCompareDormantCountsAreCosmetic(t *testing.T) {
	// A5: the three previously-dormant count signals (ImageCount, MissingAltCount,
	// ExternalLinkCount) are diffed as cosmetic history records, mirroring
	// word_count. Cosmetic class means: recorded in the changes table (visible to
	// `rabbot report` / `summarize_changes`), but the scheduler's alert ingest loop
	// skips them and they drive no adaptive-cadence shrink — the alerting judgment
	// lives in the A5 rules, not the diff.
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	old := model.Snapshot{
		ID:                1,
		URLID:             7,
		ContentSHA256:     "a",
		ImageCount:        10,
		MissingAltCount:   2,
		ExternalLinkCount: 5,
	}
	new := model.Snapshot{
		ID:                2,
		URLID:             7,
		ContentSHA256:     "a", // no content change
		ImageCount:        12,  // changed
		MissingAltCount:   4,   // changed
		ExternalLinkCount: 9,   // changed
	}
	byField := fieldMap(Compare(new, old, DefaultSimhashThreshold, now))

	for _, tc := range []struct{ field, oldV, newV string }{
		{"image_count", "10", "12"},
		{"missing_alt_count", "2", "4"},
		{"external_link_count", "5", "9"},
	} {
		c, ok := byField[tc.field]
		if !ok {
			t.Fatalf("expected a %s change", tc.field)
		}
		if c.ChangeClass != model.ChangeCosmetic {
			t.Errorf("%s change class = %q, want cosmetic", tc.field, c.ChangeClass)
		}
		if c.OldValue != tc.oldV || c.NewValue != tc.newV {
			t.Errorf("%s change = %q->%q, want %q->%q", tc.field, c.OldValue, c.NewValue, tc.oldV, tc.newV)
		}
		if c.URLID != 7 || c.SnapshotID != 2 || !c.DetectedAt.Equal(now) {
			t.Errorf("%s metadata wrong: url=%d snap=%d at=%v", tc.field, c.URLID, c.SnapshotID, c.DetectedAt)
		}
	}
}

func TestCompareDormantCountsEqualEmitNothing(t *testing.T) {
	// Equal counts must emit no change at all (no spurious cosmetic churn rows).
	now := time.Now()
	snap := model.Snapshot{
		ContentSHA256:     "a",
		ImageCount:        10,
		MissingAltCount:   2,
		ExternalLinkCount: 5,
	}
	old := snap
	old.ID = 1
	new := snap
	new.ID = 2
	byField := fieldMap(Compare(new, old, DefaultSimhashThreshold, now))
	for _, field := range []string{"image_count", "missing_alt_count", "external_link_count"} {
		if _, ok := byField[field]; ok {
			t.Errorf("equal %s must not emit a change", field)
		}
	}
}

func TestCompareRedirectChainStaysSubstantive(t *testing.T) {
	// A5 acceptance criterion 2: the diff layer keeps producing the redirect_chain
	// change record as SUBSTANTIVE (history/audit trail). De-noising redirect
	// ALERTS — owner decision #4: churn that neither grows nor loops stops paging —
	// is a scheduler skip-list change (process.go ingest loop), NOT a diff change.
	// Parsed growth/loop alerting lives in the A5 rules (Wave 2). This test pins the
	// diff invariant so a future "stop diffing redirect_chain" refactor can't
	// silently drop the history record.
	now := time.Now()
	old := model.Snapshot{ID: 1, ContentSHA256: "a", RedirectChain: `["https://x/a"]`}
	new := model.Snapshot{ID: 2, ContentSHA256: "a", RedirectChain: `["https://x/a","https://x/b"]`}
	c, ok := fieldMap(Compare(new, old, DefaultSimhashThreshold, now))["redirect_chain"]
	if !ok {
		t.Fatalf("expected a redirect_chain change record (history)")
	}
	if c.ChangeClass != model.ChangeSubstantive {
		t.Errorf("redirect_chain change class = %q, want substantive", c.ChangeClass)
	}
}

func TestCompareNoPriorSnapshot(t *testing.T) {
	// First-ever snapshot (zero-value old, ID 0) must emit no changes (baseline).
	now := time.Now()
	new := model.Snapshot{Title: "Hello", HTTPStatus: 200, ContentSHA256: "x", ContentSimhash: 1}
	if got := Compare(new, model.Snapshot{}, DefaultSimhashThreshold, now); len(got) != 0 {
		t.Errorf("baseline snapshot must emit 0 changes, got %d: %+v", len(got), got)
	}
}
