package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestRunInspect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Date(2026, 6, 4, 13, 0, 0, 0, time.UTC)
	lc := now // addressable for the *time.Time field
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// A crawled URL with a snapshot.
	urlID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://ex.com/p", FirstSeen: now, LastChecked: &lc, NextCheckAt: now, Interval: 600, Importance: 1, LastFetchClass: model.FetchOK})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID: urlID, FetchedAt: now, HTTPStatus: 200, Title: "Hello Title",
		MetaDescription: "desc", Canonical: "https://ex.com/p", Headings: `{"h1":["Hi"]}`,
		Indexable: true, IndexabilityReason: "indexable", SchemaTypes: `["WebSite"]`,
		InternalLinkCount: 5, ExternalLinkCount: 2, ImageCount: 3, MissingAltCount: 1, WordCount: 42,
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// A seeded URL that has never been crawled (no snapshot yet).
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://ex.com/fresh", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1}); err != nil {
		t.Fatalf("UpsertURL fresh: %v", err)
	}

	t.Run("known URL renders its snapshot", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runInspect(ctx, db, &buf, "https://ex.com/p"); err != nil {
			t.Fatalf("runInspect: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"https://ex.com/p", "Hello Title", "200", "indexable", "ok"} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect output missing %q\n---\n%s", want, out)
			}
		}
	})

	t.Run("seeded URL with no snapshot reports none, not an error", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runInspect(ctx, db, &buf, "https://ex.com/fresh"); err != nil {
			t.Fatalf("runInspect (no snapshot) returned error: %v", err)
		}
		if out := buf.String(); !strings.Contains(out, "none yet") {
			t.Errorf("expected a 'none yet' snapshot line for an un-crawled URL, got:\n%s", out)
		}
	})

	t.Run("unmonitored URL is a clean error", func(t *testing.T) {
		if err := runInspect(ctx, db, &bytes.Buffer{}, "https://ex.com/missing"); err == nil {
			t.Errorf("expected an error for an unmonitored URL")
		}
	})

	t.Run("never-verified site (no proof row) shows throttled, not unknown", func(t *testing.T) {
		// A siteID with no site row makes GetVerification return store.ErrNotFound.
		// verificationTier must mirror verificationState and read this as "throttled"
		// (the effective tier of a never-verified site), NOT "(unknown)".
		if got := verificationTier(ctx, db, 999999); got != "throttled" {
			t.Errorf("verificationTier(missing site) = %q, want %q (ErrNotFound => throttled)", got, "throttled")
		}
	})

	t.Run("genuine DB error degrades to unknown", func(t *testing.T) {
		// A closed DB makes GetVerification fail with a non-ErrNotFound error;
		// "(unknown)" is reserved for exactly that case so a transient glitch never
		// fails inspect yet stays distinguishable from a never-verified site.
		closedCtx := context.Background()
		closedDB, derr := store.Open(closedCtx, filepath.Join(t.TempDir(), "closed.db"))
		if derr != nil {
			t.Fatalf("Open: %v", derr)
		}
		cID, aerr := closedDB.AddSite(closedCtx, model.Site{BaseURL: "https://closed.example", Name: "C", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
		if aerr != nil {
			t.Fatalf("AddSite: %v", aerr)
		}
		if cerr := closedDB.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		if got := verificationTier(closedCtx, closedDB, cID); got != "(unknown)" {
			t.Errorf("verificationTier(closed DB) = %q, want %q (genuine DB error)", got, "(unknown)")
		}
	})

	t.Run("open issue line carries its detail JSON", func(t *testing.T) {
		// A3: inspect's per-issue line shows the raw detail JSON (e.g. the
		// measured/budget px) when present, so the pull surface carries the
		// numbers verbatim. An empty/"{}" detail appends no stray suffix.
		const overflowDetail = `{"measured_px":906,"budget_px":580,"chars":48}`
		now := time.Date(2026, 6, 4, 13, 0, 0, 0, time.UTC)
		if _, err := db.UpsertIssue(ctx, model.Issue{
			URLID: urlID, RuleID: "title_pixel_overflow", Status: model.IssueOpen,
			Severity: model.SeverityWarning, ImpactPoints: 1,
			OpenedAt: now, LastSeenAt: now, Detail: overflowDetail,
		}); err != nil {
			t.Fatalf("UpsertIssue (overflow): %v", err)
		}
		// A second open issue with no meaningful detail: must not print "{}".
		if _, err := db.UpsertIssue(ctx, model.Issue{
			URLID: urlID, RuleID: "title_changed", Status: model.IssueOpen,
			Severity: model.SeverityWarning, ImpactPoints: 1,
			OpenedAt: now, LastSeenAt: now, // Detail empty -> stored as "{}"
		}); err != nil {
			t.Fatalf("UpsertIssue (changed): %v", err)
		}

		var buf bytes.Buffer
		if err := runInspect(ctx, db, &buf, "https://ex.com/p"); err != nil {
			t.Fatalf("runInspect: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, overflowDetail) {
			t.Errorf("inspect issue line missing the detail JSON %q:\n%s", overflowDetail, out)
		}
		if strings.Contains(out, "{}") {
			t.Errorf("an empty/{} detail must not render a literal \"{}\" suffix:\n%s", out)
		}
	})

	t.Run("shows the verification tier", func(t *testing.T) {
		// Default (never verified) reads back throttled.
		var buf bytes.Buffer
		if err := runInspect(ctx, db, &buf, "https://ex.com/p"); err != nil {
			t.Fatalf("runInspect: %v", err)
		}
		if out := buf.String(); !strings.Contains(out, "throttled") {
			t.Errorf("expected a verification tier line showing throttled, got:\n%s", out)
		}

		// Flip the site to verified; the tier line follows.
		if err := db.SaveVerification(ctx, siteID, verify.ProofRecord{
			SiteID: siteID, Method: verify.MethodWellKnown, Token: "rab_x",
			State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
		}); err != nil {
			t.Fatalf("SaveVerification: %v", err)
		}
		var buf2 bytes.Buffer
		if err := runInspect(ctx, db, &buf2, "https://ex.com/p"); err != nil {
			t.Fatalf("runInspect (verified): %v", err)
		}
		if out := buf2.String(); !strings.Contains(out, "verified") {
			t.Errorf("expected a verification tier line showing verified, got:\n%s", out)
		}
	})
}

// TestRunInspectRenderMode pins A8's pull surface: the "Latest snapshot" block
// carries a render_mode line that names how the page delivers its SEO content plus
// the extraction provenance. It exercises BOTH arms of the zero-value guard (the
// BOTH-BASELINE-ARMS lesson): a populated render_mode renders verbatim with its
// source, while a pre-A8 / hydration-disabled snapshot (empty render_mode AND empty
// extraction_source) renders "unknown" with the implicit "dom" source — never a
// blank cell. The values must survive the SaveSnapshot -> LatestSnapshot round trip
// (PERSISTED-ENCODING lesson), so this drives a real store, not a hand-built struct.
func TestRunInspectRenderMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rm.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Date(2026, 6, 13, 11, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://rm.test", Name: "RM", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	t.Run("populated render_mode renders verbatim with its source", func(t *testing.T) {
		urlID, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://rm.test/spa", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		if _, serr := db.SaveSnapshot(ctx, model.Snapshot{
			URLID: urlID, FetchedAt: now, HTTPStatus: 200,
			RenderMode: model.RenderClientShell, ExtractionSource: "dom",
		}); serr != nil {
			t.Fatalf("SaveSnapshot: %v", serr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://rm.test/spa"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		out := buf.String()
		if !strings.Contains(out, "render_mode:") {
			t.Errorf("inspect output missing the render_mode line:\n%s", out)
		}
		if !strings.Contains(out, string(model.RenderClientShell)) {
			t.Errorf("inspect render_mode line missing %q:\n%s", model.RenderClientShell, out)
		}
		// The zero-value fallback must NOT fire for a populated mode.
		if strings.Contains(out, "render_mode:  unknown") {
			t.Errorf("populated render_mode must not render as unknown:\n%s", out)
		}
	})

	t.Run("empty render_mode (pre-A8 row) renders as unknown with dom source", func(t *testing.T) {
		urlID, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://rm.test/legacy", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		// RenderMode + ExtractionSource left at their zero values, mirroring a
		// snapshot written before A8 (migration DEFAULT '') or with hydration off.
		if _, serr := db.SaveSnapshot(ctx, model.Snapshot{
			URLID: urlID, FetchedAt: now, HTTPStatus: 200,
		}); serr != nil {
			t.Fatalf("SaveSnapshot: %v", serr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://rm.test/legacy"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		out := buf.String()
		if !strings.Contains(out, "render_mode:  unknown") {
			t.Errorf("an empty render_mode must render as unknown, got:\n%s", out)
		}
		if !strings.Contains(out, "source: dom") {
			t.Errorf("an empty extraction_source must render the implicit dom source, got:\n%s", out)
		}
	})
}

// TestRenderModeLabel pins the zero-value guard directly so the "unknown" fallback
// cannot regress to printing an empty string, independent of the store round trip.
func TestRenderModeLabel(t *testing.T) {
	t.Parallel()
	if got := renderModeLabel(""); got != "unknown" {
		t.Errorf("renderModeLabel(\"\") = %q, want %q", got, "unknown")
	}
	if got := renderModeLabel(model.RenderHydrated); got != "hydrated" {
		t.Errorf("renderModeLabel(hydrated) = %q, want %q", got, "hydrated")
	}
	if got := extractionSourceLabel(""); got != "dom" {
		t.Errorf("extractionSourceLabel(\"\") = %q, want %q", got, "dom")
	}
	if got := extractionSourceLabel("dom+next_data"); got != "dom+next_data" {
		t.Errorf("extractionSourceLabel(dom+next_data) = %q, want %q", got, "dom+next_data")
	}
}

// TestRunInspectRichResults asserts the "Rich results:" section validates the
// latest snapshot's JSON-LD against the in-binary profile and renders per-type
// eligibility + the profile version, with a neutral line for unprofiled types.
func TestRunInspectRichResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Date(2026, 6, 4, 13, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://shop.test", Name: "Shop", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	t.Run("eligible Product renders eligible + profile version", func(t *testing.T) {
		urlID, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://shop.test/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		if _, serr := db.SaveSnapshot(ctx, model.Snapshot{
			URLID: urlID, FetchedAt: now, HTTPStatus: 200,
			JSONLD:      `{"@type":"Product","name":"Widget","offers":{"@type":"Offer","price":"9.99"}}`,
			SchemaTypes: `["Product"]`,
		}); serr != nil {
			t.Fatalf("SaveSnapshot: %v", serr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://shop.test/p"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		out := buf.String()
		for _, want := range []string{"Rich results:", "grr-2026.06", "Product", "eligible"} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect rich-results output missing %q\n---\n%s", want, out)
			}
		}
	})

	t.Run("Product missing offers renders ineligible naming the gap", func(t *testing.T) {
		urlID, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://shop.test/broken", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		if _, serr := db.SaveSnapshot(ctx, model.Snapshot{
			URLID: urlID, FetchedAt: now, HTTPStatus: 200,
			JSONLD: `{"@type":"Product","name":"Widget"}`,
		}); serr != nil {
			t.Fatalf("SaveSnapshot: %v", serr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://shop.test/broken"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		out := buf.String()
		for _, want := range []string{"Rich results:", "Product", "ineligible", "offers"} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect rich-results output missing %q\n---\n%s", want, out)
			}
		}
	})

	t.Run("only unprofiled types render a neutral line, no verdict", func(t *testing.T) {
		urlID, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://shop.test/faq", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1})
		if uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		if _, serr := db.SaveSnapshot(ctx, model.Snapshot{
			URLID: urlID, FetchedAt: now, HTTPStatus: 200,
			JSONLD: `{"@type":"FAQPage","mainEntity":[]}`,
		}); serr != nil {
			t.Fatalf("SaveSnapshot: %v", serr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://shop.test/faq"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		out := buf.String()
		if !strings.Contains(out, "Rich results:") {
			t.Errorf("expected a Rich results section, got:\n%s", out)
		}
		// An unprofiled type must never earn an eligibility verdict.
		if strings.Contains(out, "eligible") || strings.Contains(out, "ineligible") {
			t.Errorf("unprofiled type must not get an eligibility verdict:\n%s", out)
		}
		if !strings.Contains(out, "unprofiled") {
			t.Errorf("expected a neutral unprofiled count line, got:\n%s", out)
		}
	})

	t.Run("no snapshot yet — no rich-results section, no error", func(t *testing.T) {
		if _, uerr := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://shop.test/fresh", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1}); uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		var buf bytes.Buffer
		if rerr := runInspect(ctx, db, &buf, "https://shop.test/fresh"); rerr != nil {
			t.Fatalf("runInspect: %v", rerr)
		}
		if strings.Contains(buf.String(), "Rich results:") {
			t.Errorf("un-crawled URL must not render a Rich results section:\n%s", buf.String())
		}
	})
}
