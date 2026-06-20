package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestAddSiteDuplicateBaseURLReturnsErrSiteExists pins finding #20.2: a second
// AddSite with the same base_url violates UNIQUE(base_url) and must surface as
// the typed sentinel store.ErrSiteExists (wrapped), not a raw modernc driver
// error — otherwise callers doing errors.Is(err, store.ErrSiteExists) never match.
func TestAddSiteDuplicateBaseURLReturnsErrSiteExists(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	site := model.Site{
		BaseURL: "https://dup.example.com", Name: "Dup", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	}

	if _, err := db.AddSite(ctx, site); err != nil {
		t.Fatalf("first AddSite() error = %v, want nil", err)
	}

	_, err := db.AddSite(ctx, site)
	if err == nil {
		t.Fatalf("second AddSite() with duplicate base_url returned nil error, want ErrSiteExists")
	}
	if !errors.Is(err, store.ErrSiteExists) {
		t.Fatalf("second AddSite() error = %v, want errors.Is(err, store.ErrSiteExists)", err)
	}
}
