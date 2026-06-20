package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func addTestSite(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.AddSite(context.Background(), model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	return id
}

func TestSaveAndGetVerification(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := addTestSite(t, db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	rec := verify.ProofRecord{
		SiteID:           siteID,
		Method:           verify.MethodWellKnown,
		Token:            "rab_TESTTOKENVALUE",
		State:            verify.StateVerified,
		VerifiedAt:       now,
		LastReverifiedAt: now,
	}
	if err := db.SaveVerification(ctx, siteID, rec); err != nil {
		t.Fatalf("SaveVerification: %v", err)
	}

	got, err := db.GetVerification(ctx, siteID)
	if err != nil {
		t.Fatalf("GetVerification: %v", err)
	}
	if got.SiteID != siteID {
		t.Errorf("SiteID = %d, want %d", got.SiteID, siteID)
	}
	if got.Method != verify.MethodWellKnown {
		t.Errorf("Method = %q, want %q", got.Method, verify.MethodWellKnown)
	}
	if got.Token != rec.Token {
		t.Errorf("Token = %q, want %q", got.Token, rec.Token)
	}
	if got.State != verify.StateVerified {
		t.Errorf("State = %q, want %q", got.State, verify.StateVerified)
	}
	if d := got.VerifiedAt.Sub(now); d > time.Second || d < -time.Second {
		t.Errorf("VerifiedAt = %v, want within 1s of %v", got.VerifiedAt, now)
	}
	if d := got.LastReverifiedAt.Sub(now); d > time.Second || d < -time.Second {
		t.Errorf("LastReverifiedAt = %v, want within 1s of %v", got.LastReverifiedAt, now)
	}
}

func TestGetVerificationDefaultsThrottled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := addTestSite(t, db)

	got, err := db.GetVerification(ctx, siteID)
	if err != nil {
		t.Fatalf("GetVerification: %v", err)
	}
	if got.State != verify.StateThrottled {
		t.Errorf("State = %q, want %q (the safe default for an unverified site)", got.State, verify.StateThrottled)
	}
	if got.Method != "" {
		t.Errorf("Method = %q, want empty", got.Method)
	}
	if got.Token != "" {
		t.Errorf("Token = %q, want empty", got.Token)
	}
	if !got.VerifiedAt.IsZero() {
		t.Errorf("VerifiedAt = %v, want zero (NULL)", got.VerifiedAt)
	}
	if !got.LastReverifiedAt.IsZero() {
		t.Errorf("LastReverifiedAt = %v, want zero (NULL)", got.LastReverifiedAt)
	}
}

func TestSaveVerificationUnknownSite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	err := db.SaveVerification(ctx, 999999, verify.ProofRecord{
		SiteID: 999999, Method: verify.MethodDNS, State: verify.StateVerified,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveVerification for missing site err = %v, want ErrNotFound", err)
	}
}
