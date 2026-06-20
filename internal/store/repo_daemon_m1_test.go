package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestCountDueURLs(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	enabledSite, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://enabled.com", Name: "Enabled", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	disabledSite, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://disabled.com", Name: "Disabled", Enabled: false,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})

	// Two due URLs on the enabled site.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: enabledSite, URL: "https://enabled.com/a", FirstSeen: past, NextCheckAt: past, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: enabledSite, URL: "https://enabled.com/b", FirstSeen: past, NextCheckAt: past, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	// A not-yet-due URL on the enabled site (excluded by the next_check_at filter).
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: enabledSite, URL: "https://enabled.com/later", FirstSeen: past, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	// A due URL on the disabled site (excluded by the enabled filter).
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: disabledSite, URL: "https://disabled.com/a", FirstSeen: past, NextCheckAt: past, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	got, err := db.CountDueURLs(ctx, now)
	if err != nil {
		t.Fatalf("CountDueURLs() error = %v", err)
	}
	if got != 2 {
		t.Errorf("CountDueURLs() = %d, want 2", got)
	}
}

func TestEnqueueRecheckAllSites(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour)

	enabledSite, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://enabled.com", Name: "Enabled", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	disabledSite, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://disabled.com", Name: "Disabled", Enabled: false,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})

	// Two not-yet-due URLs on the enabled site.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: enabledSite, URL: "https://enabled.com/a", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: enabledSite, URL: "https://enabled.com/b", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	// A not-yet-due URL on the disabled site — must NOT be touched.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: disabledSite, URL: "https://disabled.com/a", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	// Confirm precondition: nothing is due before the recheck.
	if n, err := db.CountDueURLs(ctx, now); err != nil || n != 0 {
		t.Fatalf("precondition CountDueURLs() = %d err=%v, want 0", n, err)
	}

	affected, err := db.EnqueueRecheck(ctx, "", now)
	if err != nil {
		t.Fatalf("EnqueueRecheck() error = %v", err)
	}
	if affected != 2 {
		t.Errorf("EnqueueRecheck() affected = %d, want 2", affected)
	}

	// The two enabled URLs are now due; the disabled-site URL still is not.
	got, err := db.GetURL(ctx, enabledSite, "https://enabled.com/a")
	if err != nil {
		t.Fatalf("GetURL() error = %v", err)
	}
	if !got.NextCheckAt.Equal(now) {
		t.Errorf("NextCheckAt = %v, want %v", got.NextCheckAt, now)
	}

	if n, err := db.CountDueURLs(ctx, now); err != nil || n != 2 {
		t.Fatalf("CountDueURLs() = %d err=%v, want 2", n, err)
	}

	disabled, err := db.GetURL(ctx, disabledSite, "https://disabled.com/a")
	if err != nil {
		t.Fatalf("GetURL() error = %v", err)
	}
	if !disabled.NextCheckAt.Equal(future) {
		t.Errorf("disabled-site URL NextCheckAt = %v, want untouched %v", disabled.NextCheckAt, future)
	}
}

func TestEnqueueRecheckTargeted(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour)

	siteA, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://a.com", Name: "A", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	siteB, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://b.com", Name: "B", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})

	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteA, URL: "https://a.com/x", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteA, URL: "https://a.com/y", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteB, URL: "https://b.com/x", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	// Target a single URL by its exact address.
	affected, err := db.EnqueueRecheck(ctx, "https://a.com/x", now)
	if err != nil {
		t.Fatalf("EnqueueRecheck(url) error = %v", err)
	}
	if affected != 1 {
		t.Errorf("EnqueueRecheck(url) affected = %d, want 1", affected)
	}
	gotX, _ := db.GetURL(ctx, siteA, "https://a.com/x")
	if !gotX.NextCheckAt.Equal(now) {
		t.Errorf("targeted URL NextCheckAt = %v, want %v", gotX.NextCheckAt, now)
	}
	gotY, _ := db.GetURL(ctx, siteA, "https://a.com/y")
	if !gotY.NextCheckAt.Equal(future) {
		t.Errorf("sibling URL should be untouched, NextCheckAt = %v, want %v", gotY.NextCheckAt, future)
	}

	// Target a whole site by its base_url: both of siteB's URLs (here one) plus
	// any URL literally matching the target. Re-seed siteB with two URLs.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteB, URL: "https://b.com/y", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	affected, err = db.EnqueueRecheck(ctx, "https://b.com", now)
	if err != nil {
		t.Fatalf("EnqueueRecheck(site) error = %v", err)
	}
	if affected != 2 {
		t.Errorf("EnqueueRecheck(site) affected = %d, want 2", affected)
	}
	bx, _ := db.GetURL(ctx, siteB, "https://b.com/x")
	by, _ := db.GetURL(ctx, siteB, "https://b.com/y")
	if !bx.NextCheckAt.Equal(now) || !by.NextCheckAt.Equal(now) {
		t.Errorf("site-targeted URLs not made due: x=%v y=%v want %v", bx.NextCheckAt, by.NextCheckAt, now)
	}
}

// TestEnqueueRecheckTargetedSkipsDisabledSite reproduces F51: a targeted
// EnqueueRecheck must not touch URLs belonging to disabled sites, since
// PopDueURLs/CountDueURLs filter on enabled = 1 and would never surface them.
// The returned Queued count must reflect only rows that can actually be popped.
func TestEnqueueRecheckTargetedSkipsDisabledSite(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour)

	disabledSite, _ := db.AddSite(ctx, model.Site{
		BaseURL: "https://off.com", Name: "Off", Enabled: false,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})

	if _, err := db.UpsertURL(ctx, model.URL{SiteID: disabledSite, URL: "https://off.com/page", FirstSeen: now, NextCheckAt: future, Interval: 600}); err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	// Target the disabled site's page URL directly.
	affected, err := db.EnqueueRecheck(ctx, "https://off.com/page", now)
	if err != nil {
		t.Fatalf("EnqueueRecheck(url) error = %v", err)
	}
	if affected != 0 {
		t.Errorf("EnqueueRecheck targeting a disabled-site URL = %d, want 0 (PopDueURLs never surfaces it)", affected)
	}

	// And targeting the disabled site by its base_url is likewise a no-op.
	affected, err = db.EnqueueRecheck(ctx, "https://off.com", now)
	if err != nil {
		t.Fatalf("EnqueueRecheck(site) error = %v", err)
	}
	if affected != 0 {
		t.Errorf("EnqueueRecheck targeting a disabled site = %d, want 0", affected)
	}

	// The URL's schedule must be untouched, so it is still not due.
	got, err := db.GetURL(ctx, disabledSite, "https://off.com/page")
	if err != nil {
		t.Fatalf("GetURL() error = %v", err)
	}
	if !got.NextCheckAt.Equal(future) {
		t.Errorf("disabled-site URL NextCheckAt = %v, want untouched %v", got.NextCheckAt, future)
	}
}

func TestDeleteSiteCascades(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://gone.com", Name: "Gone", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite() error = %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://gone.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600,
	})
	if err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200}); err != nil {
		t.Fatalf("SaveSnapshot() error = %v", err)
	}

	if err := db.DeleteSite(ctx, siteID); err != nil {
		t.Fatalf("DeleteSite() error = %v", err)
	}

	// The site is gone.
	if _, err := db.GetSite(ctx, siteID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetSite() after delete error = %v, want ErrNotFound", err)
	}
	// Its URL cascaded away.
	if _, err := db.GetURL(ctx, siteID, "https://gone.com/p"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetURL() after delete error = %v, want ErrNotFound", err)
	}
	// Its snapshot cascaded away.
	if _, err := db.LatestSnapshot(ctx, urlID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestSnapshot() after delete error = %v, want ErrNotFound", err)
	}
}
