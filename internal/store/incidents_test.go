package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// readAutoClosedAt reads the alerts.auto_closed_at column directly (no public
// getter exists for a single incident by id).
func readAutoClosedAt(t *testing.T, st *store.DB, id int64) sql.NullTime {
	t.Helper()
	var got sql.NullTime
	if err := st.Read().QueryRowContext(context.Background(),
		`SELECT auto_closed_at FROM alerts WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read auto_closed_at(id=%d): %v", id, err)
	}
	return got
}

// seedSite creates a site so the alerts.site_id foreign key (PRAGMA
// foreign_keys = ON, set by the M0 connection hook) resolves. The exact id is
// opaque; callers assert on row contents, not on the literal id.
func seedSite(t *testing.T, st *store.DB, host string) int64 {
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

func TestListOpenIncidents(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	a := model.Alert{
		SiteID: seedSite(t, st, "incidents-list.com"), Fingerprint: "fp-1", GroupKey: "ex.com|title",
		Severity: model.SeverityWarning, Status: model.AlertOpen,
		AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
	}
	id, err := st.OpenIncident(ctx, a)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	got, ok, err := st.GetOpenIncident(ctx, "fp-1")
	if err != nil || !ok {
		t.Fatalf("GetOpenIncident: ok=%v err=%v", ok, err)
	}
	if got.ID != id || got.GroupKey != "ex.com|title" {
		t.Errorf("GetOpenIncident wrong: %+v", got)
	}

	open, err := st.ListOpenIncidents(ctx)
	if err != nil {
		t.Fatalf("ListOpenIncidents: %v", err)
	}
	if len(open) != 1 || open[0].ID != id {
		t.Fatalf("ListOpenIncidents = %+v, want 1 open incident id=%d", open, id)
	}

	// Closed incidents drop out of the open list and GetOpenIncident.
	if err := st.CloseIncident(ctx, id, now.Add(time.Hour), true); err != nil {
		t.Fatalf("CloseIncident: %v", err)
	}
	if _, ok, _ := st.GetOpenIncident(ctx, "fp-1"); ok {
		t.Error("closed incident must not be returned by GetOpenIncident")
	}
	open, _ = st.ListOpenIncidents(ctx)
	if len(open) != 0 {
		t.Errorf("ListOpenIncidents after close = %d, want 0", len(open))
	}
}

// TestOpenIncidentPartialUniqueIndexEnforcesSingleOpen covers migration 0002:
// the partial unique index idx_alerts_open_fingerprint must reject a second OPEN
// incident for the same fingerprint at the DB level (the invariant previously
// relied solely on the pipeline's in-process mutex), while still permitting a
// new open incident once the prior one is closed.
func TestOpenIncidentPartialUniqueIndexEnforcesSingleOpen(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	siteID := seedSite(t, st, "single-open.com")
	mk := func() model.Alert {
		return model.Alert{
			SiteID: siteID, Fingerprint: "fp-dup", GroupKey: "ex.com|title",
			Severity: model.SeverityWarning, Status: model.AlertOpen,
			AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
		}
	}

	id1, err := st.OpenIncident(ctx, mk())
	if err != nil {
		t.Fatalf("first OpenIncident: %v", err)
	}

	// A second OPEN incident with the same fingerprint must be rejected by the
	// partial unique index.
	if _, err := st.OpenIncident(ctx, mk()); err == nil {
		t.Fatalf("second open incident for same fingerprint succeeded, want unique-index violation")
	}

	// After closing the first, a fresh open incident is allowed again (the index
	// only constrains status='open' rows).
	if err := st.CloseIncident(ctx, id1, now.Add(time.Hour), true); err != nil {
		t.Fatalf("CloseIncident: %v", err)
	}
	if _, err := st.OpenIncident(ctx, mk()); err != nil {
		t.Fatalf("reopen after close: %v (partial index must not block once prior closed)", err)
	}
}

// TestCloseIncidentManualPreservesAutoClosedAt covers the contract that a
// MANUAL close (autoClosed=false) must NOT erase a previously-set
// auto_closed_at timestamp. The buggy single-statement UPDATE wrote
// auto_closed_at = NULL on the manual path because autoAt was nil.
func TestCloseIncidentManualPreservesAutoClosedAt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	autoStamp := now.Add(30 * time.Minute)
	manualAt := now.Add(time.Hour)

	tests := []struct {
		name string
		// seed sets the prior auto_closed_at and returns the incident id.
		seed func(t *testing.T, st *store.DB) int64
		want sql.NullTime
	}{
		{
			name: "prior auto_closed_at via UpdateIncident survives manual close",
			seed: func(t *testing.T, st *store.DB) int64 {
				id, err := st.OpenIncident(ctx, model.Alert{
					SiteID: seedSite(t, st, "incidents-manual-update.com"), Fingerprint: "fp-mu",
					GroupKey: "ex.com|title", Severity: model.SeverityWarning, Status: model.AlertOpen,
					AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
				})
				if err != nil {
					t.Fatalf("OpenIncident: %v", err)
				}
				cur, _, _ := st.GetOpenIncident(ctx, "fp-mu")
				ac := autoStamp
				cur.AutoClosedAt = &ac
				if err := st.UpdateIncident(ctx, cur); err != nil {
					t.Fatalf("UpdateIncident: %v", err)
				}
				return id
			},
			want: sql.NullTime{Time: autoStamp, Valid: true},
		},
		{
			name: "prior auto_closed_at via auto-close survives later manual close",
			seed: func(t *testing.T, st *store.DB) int64 {
				id, err := st.OpenIncident(ctx, model.Alert{
					SiteID: seedSite(t, st, "incidents-manual-auto.com"), Fingerprint: "fp-ma",
					GroupKey: "ex.com|indexability", Severity: model.SeverityCritical, Status: model.AlertOpen,
					AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
				})
				if err != nil {
					t.Fatalf("OpenIncident: %v", err)
				}
				if err := st.CloseIncident(ctx, id, autoStamp, true); err != nil {
					t.Fatalf("CloseIncident(auto): %v", err)
				}
				return id
			},
			want: sql.NullTime{Time: autoStamp, Valid: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			id := tc.seed(t, st)

			// Manual close must not touch auto_closed_at.
			if err := st.CloseIncident(ctx, id, manualAt, false); err != nil {
				t.Fatalf("CloseIncident(manual): %v", err)
			}

			got := readAutoClosedAt(t, st, id)
			if got.Valid != tc.want.Valid || (got.Valid && !got.Time.Equal(tc.want.Time)) {
				t.Errorf("auto_closed_at = %+v, want %+v (manual close must not erase it)", got, tc.want)
			}
		})
	}
}

func TestUpdateIncidentAccrues(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	id, err := st.OpenIncident(ctx, model.Alert{
		SiteID: seedSite(t, st, "incidents-accrue.com"), Fingerprint: "fp-acc", GroupKey: "ex.com|indexability",
		Severity: model.SeverityCritical, Status: model.AlertOpen,
		AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	cur, _, _ := st.GetOpenIncident(ctx, "fp-acc")
	cur.AffectedCount = 3
	cur.LastUpdatedAt = now.Add(10 * time.Minute)
	notified := now.Add(10 * time.Minute)
	cur.LastNotifiedAt = &notified
	if err := st.UpdateIncident(ctx, cur); err != nil {
		t.Fatalf("UpdateIncident: %v", err)
	}
	got, ok, _ := st.GetOpenIncident(ctx, "fp-acc")
	if !ok || got.ID != id || got.AffectedCount != 3 || got.LastNotifiedAt == nil {
		t.Fatalf("UpdateIncident did not persist: %+v ok=%v", got, ok)
	}
}
