package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedSegmentFixture builds a small two-site store with segments and member URLs
// so the segments-surface tests can assert against the EXACT persisted encoding
// (names, patterns, member counts) rather than logical input shapes.
func seedSegmentFixture(t *testing.T) (context.Context, *store.DB, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "seg.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{Name: "content", MatchRule: "^/blog/"},
		{Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}

	// Two blog URLs (content), one product URL (product).
	mk := func(path string) int64 {
		uid, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://ex.com" + path, FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL %q: %v", path, uerr)
		}
		return uid
	}
	blog1 := mk("/blog/a")
	blog2 := mk("/blog/b")
	prod1 := mk("/product/x")
	if err := db.SetURLSegments(ctx, blog1, []int64{ids["content"]}); err != nil {
		t.Fatalf("SetURLSegments blog1: %v", err)
	}
	if err := db.SetURLSegments(ctx, blog2, []int64{ids["content"]}); err != nil {
		t.Fatalf("SetURLSegments blog2: %v", err)
	}
	if err := db.SetURLSegments(ctx, prod1, []int64{ids["product"]}); err != nil {
		t.Fatalf("SetURLSegments prod1: %v", err)
	}
	return ctx, db, siteID
}

func TestNewSegmentsCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := newSegmentsCmd()
	if cmd.Use != "segments" {
		t.Errorf("Use = %q, want segments", cmd.Use)
	}
	if cmd.Flags().Lookup("site") == nil {
		t.Error("segments command must expose --site")
	}
	if cmd.Flags().Lookup("json") == nil {
		t.Error("segments command must expose --json")
	}
}

// TestRunSegments_Table asserts the table form lists name/pattern/member-count
// against the exact persisted encoding (LEFT JOIN counts: content=2, product=1).
func TestRunSegments_Table(t *testing.T) {
	t.Parallel()
	ctx, db, _ := seedSegmentFixture(t)
	var buf bytes.Buffer
	if err := runSegments(ctx, db, &buf, nil, false); err != nil {
		t.Fatalf("runSegments: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "PATTERN", "MEMBERS", "content", "^/blog/", "product", "^/product/"} {
		if !strings.Contains(out, want) {
			t.Errorf("segments table missing %q:\n%s", want, out)
		}
	}
	// Member counts must reflect the exact persisted memberships.
	if !strings.Contains(out, "content") || !lineHasCount(out, "content", 2) {
		t.Errorf("expected content member count 2:\n%s", out)
	}
	if !lineHasCount(out, "product", 1) {
		t.Errorf("expected product member count 1:\n%s", out)
	}
}

// lineHasCount returns true if the line mentioning name ends with the given count.
func lineHasCount(out, name string, count int) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, name) {
			fields := strings.Fields(ln)
			if len(fields) > 0 && fields[len(fields)-1] == itoa(count) {
				return true
			}
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestRunSegments_JSON(t *testing.T) {
	t.Parallel()
	ctx, db, _ := seedSegmentFixture(t)
	var buf bytes.Buffer
	if err := runSegments(ctx, db, &buf, nil, true); err != nil {
		t.Fatalf("runSegments json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if len(got) != 2 {
		t.Fatalf("want 2 segments, got %d: %s", len(got), buf.String())
	}
	// Ordered by (site_id, name): content then product.
	if got[0]["name"] != "content" || got[0]["match"] != "^/blog/" || got[0]["member_count"].(float64) != 2 {
		t.Errorf("segment[0] = %v", got[0])
	}
	if got[1]["name"] != "product" || got[1]["member_count"].(float64) != 1 {
		t.Errorf("segment[1] = %v", got[1])
	}
}

// TestRunSegments_SiteScoped confirms --site restricts the listing to one site.
func TestRunSegments_SiteScoped(t *testing.T) {
	t.Parallel()
	ctx, db, siteID := seedSegmentFixture(t)
	// Add a second site with its own segment that must be excluded when scoped.
	other, err := db.AddSite(ctx, model.Site{BaseURL: "https://two.com", Name: "Two", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite two: %v", err)
	}
	if _, err := db.SyncSiteSegments(ctx, other, []model.Segment{{Name: "docs", MatchRule: "^/docs/"}}); err != nil {
		t.Fatalf("SyncSiteSegments two: %v", err)
	}

	var buf bytes.Buffer
	if err := runSegments(ctx, db, &buf, &siteID, false); err != nil {
		t.Fatalf("runSegments scoped: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "docs") {
		t.Errorf("site-scoped listing leaked another site's segment:\n%s", out)
	}
	if !strings.Contains(out, "content") {
		t.Errorf("site-scoped listing dropped the scoped site's segment:\n%s", out)
	}
}

// TestSegmentHint_UnknownName: an unknown --segment value yields a hint that
// lists every known name so the operator can correct the typo.
func TestSegmentHint_UnknownName(t *testing.T) {
	t.Parallel()
	ctx, db, _ := seedSegmentFixture(t)
	known, err := segmentExists(ctx, db, nil, "bogus")
	if err != nil {
		t.Fatalf("segmentExists: %v", err)
	}
	if known {
		t.Fatal("bogus segment should not exist")
	}
	hint, err := segmentHint(ctx, db, nil)
	if err != nil {
		t.Fatalf("segmentHint: %v", err)
	}
	for _, want := range []string{"content", "product"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q missing known name %q", hint, want)
		}
	}
}

func TestSegmentExists_KnownName(t *testing.T) {
	t.Parallel()
	ctx, db, _ := seedSegmentFixture(t)
	ok, err := segmentExists(ctx, db, nil, "content")
	if err != nil {
		t.Fatalf("segmentExists: %v", err)
	}
	if !ok {
		t.Error("content should exist")
	}
}

// TestUnknownSegment_WritesHintToStderr pins criterion 6: an unknown --segment
// value produces an empty result (no error) and a hint on stderr that lists the
// known names. The hint goes to stderr so it never corrupts a piped --json stdout.
func TestUnknownSegment_WritesHintToStderr(t *testing.T) {
	t.Parallel()
	ctx, db, _ := seedSegmentFixture(t)
	cmd := newSegmentsCmd()
	var outBuf, errBuf strings.Builder
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	if err := unknownSegment(ctx, cmd, db, nil, "bogus"); err != nil {
		t.Fatalf("unknownSegment should not error: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("hint must not write to stdout, got %q", outBuf.String())
	}
	es := errBuf.String()
	if !strings.Contains(es, "bogus") {
		t.Errorf("stderr %q should name the offending value", es)
	}
	for _, want := range []string{"content", "product"} {
		if !strings.Contains(es, want) {
			t.Errorf("stderr hint %q missing known name %q", es, want)
		}
	}
}

func TestNewIssuesCmdHasSegmentFlag(t *testing.T) {
	t.Parallel()
	if newIssuesCmd().Flags().Lookup("segment") == nil {
		t.Error("issues command must expose --segment")
	}
}

func TestNewReportCmdHasSegmentFlag(t *testing.T) {
	t.Parallel()
	if newReportCmd().Flags().Lookup("segment") == nil {
		t.Error("report command must expose --segment")
	}
}
