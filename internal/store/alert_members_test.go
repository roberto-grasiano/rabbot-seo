package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// openAlert seeds a site + one open incident and returns the alert id. Member
// tests track per-URL membership against this incident.
func openAlert(t *testing.T, st *store.DB, host, fp string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	id, err := st.OpenIncident(ctx, model.Alert{
		SiteID: seedSite(t, st, host), Fingerprint: fp, GroupKey: "ex.com|title",
		Severity: model.SeverityWarning, Status: model.AlertOpen,
		AffectedCount: 1, FirstDetectedAt: now, LastUpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	return id
}

// TestAlertMembersAddRemoveCount covers the per-incident membership lifecycle:
// add two member URLs, removing one leaves remaining=1, removing the other
// leaves remaining=0. CountAlertMembers reflects the live set throughout, and
// AddAlertMember is idempotent (INSERT OR IGNORE) on a duplicate (alert_id,url).
func TestAlertMembersAddRemoveCount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := openAlert(t, st, "members-lifecycle.com", "fp-mem")

	const (
		u1 = "https://members-lifecycle.com/a"
		u2 = "https://members-lifecycle.com/b"
	)

	if err := st.AddAlertMember(ctx, id, u1); err != nil {
		t.Fatalf("AddAlertMember(u1): %v", err)
	}
	if err := st.AddAlertMember(ctx, id, u2); err != nil {
		t.Fatalf("AddAlertMember(u2): %v", err)
	}

	// Re-adding the same member is a no-op (INSERT OR IGNORE), not a dup row.
	if err := st.AddAlertMember(ctx, id, u1); err != nil {
		t.Fatalf("AddAlertMember(u1 again): %v", err)
	}

	if n, err := st.CountAlertMembers(ctx, id); err != nil || n != 2 {
		t.Fatalf("CountAlertMembers after 2 distinct adds = %d, err=%v; want 2", n, err)
	}

	// Remove the first member: one sibling still broken => remaining=1.
	remaining, err := st.RemoveAlertMember(ctx, id, u1)
	if err != nil {
		t.Fatalf("RemoveAlertMember(u1): %v", err)
	}
	if remaining != 1 {
		t.Fatalf("RemoveAlertMember(u1) remaining = %d, want 1", remaining)
	}
	if n, _ := st.CountAlertMembers(ctx, id); n != 1 {
		t.Fatalf("CountAlertMembers after first remove = %d, want 1", n)
	}

	// Remove the last member: no siblings left => remaining=0 (caller closes).
	remaining, err = st.RemoveAlertMember(ctx, id, u2)
	if err != nil {
		t.Fatalf("RemoveAlertMember(u2): %v", err)
	}
	if remaining != 0 {
		t.Fatalf("RemoveAlertMember(u2) remaining = %d, want 0", remaining)
	}
	if n, _ := st.CountAlertMembers(ctx, id); n != 0 {
		t.Fatalf("CountAlertMembers after last remove = %d, want 0", n)
	}
}

// TestRemoveAlertMemberAbsentURL covers removing a URL that was never a member:
// it must not error and must report the unchanged remaining count for the alert.
func TestRemoveAlertMemberAbsentURL(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := openAlert(t, st, "members-absent.com", "fp-abs")

	if err := st.AddAlertMember(ctx, id, "https://members-absent.com/present"); err != nil {
		t.Fatalf("AddAlertMember: %v", err)
	}
	remaining, err := st.RemoveAlertMember(ctx, id, "https://members-absent.com/never")
	if err != nil {
		t.Fatalf("RemoveAlertMember(absent): %v", err)
	}
	if remaining != 1 {
		t.Fatalf("RemoveAlertMember(absent) remaining = %d, want 1 (unchanged)", remaining)
	}
}

// TestHasOpenIncidentMember covers the fire-on-state-change idempotency probe: it is
// true only when an OPEN incident exists for the fingerprint AND the URL is a tracked
// member; false for an unknown fingerprint, a non-member URL, or a CLOSED incident.
func TestHasOpenIncidentMember(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const fp = "fp-has-mem"
	id := openAlert(t, st, "has-member.com", fp)
	const (
		member    = "https://has-member.com/in"
		nonMember = "https://has-member.com/out"
	)
	if err := st.AddAlertMember(ctx, id, member); err != nil {
		t.Fatalf("AddAlertMember: %v", err)
	}

	// Open incident + tracked member → true.
	if ok, err := st.HasOpenIncidentMember(ctx, fp, member); err != nil || !ok {
		t.Fatalf("HasOpenIncidentMember(open,member) = %v, err=%v; want true", ok, err)
	}
	// Open incident but URL not a member → false.
	if ok, err := st.HasOpenIncidentMember(ctx, fp, nonMember); err != nil || ok {
		t.Fatalf("HasOpenIncidentMember(open,non-member) = %v, err=%v; want false", ok, err)
	}
	// Unknown fingerprint → false.
	if ok, err := st.HasOpenIncidentMember(ctx, "fp-unknown", member); err != nil || ok {
		t.Fatalf("HasOpenIncidentMember(unknown-fp) = %v, err=%v; want false", ok, err)
	}

	// Close the incident: the member is no longer part of an OPEN incident → false.
	if err := st.CloseIncident(ctx, id, time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), false); err != nil {
		t.Fatalf("CloseIncident: %v", err)
	}
	if ok, err := st.HasOpenIncidentMember(ctx, fp, member); err != nil || ok {
		t.Fatalf("HasOpenIncidentMember(closed,member) = %v, err=%v; want false", ok, err)
	}
}

// TestAlertMembersCascadeOnAlertDelete covers the ON DELETE CASCADE: deleting
// the parent alerts row must remove all of its alert_members rows.
func TestAlertMembersCascadeOnAlertDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := openAlert(t, st, "members-cascade.com", "fp-cas")

	if err := st.AddAlertMember(ctx, id, "https://members-cascade.com/x"); err != nil {
		t.Fatalf("AddAlertMember(x): %v", err)
	}
	if err := st.AddAlertMember(ctx, id, "https://members-cascade.com/y"); err != nil {
		t.Fatalf("AddAlertMember(y): %v", err)
	}
	if n, _ := st.CountAlertMembers(ctx, id); n != 2 {
		t.Fatalf("precondition CountAlertMembers = %d, want 2", n)
	}

	// Delete the parent incident row directly.
	if err := st.WriteTx(ctx, func(tx store.Tx) error {
		_, e := tx.ExecContext(ctx, `DELETE FROM alerts WHERE id = ?`, id)
		return e
	}); err != nil {
		t.Fatalf("delete alert row: %v", err)
	}

	// CASCADE must have removed the member rows.
	if n, err := st.CountAlertMembers(ctx, id); err != nil || n != 0 {
		t.Fatalf("CountAlertMembers after parent delete = %d, err=%v; want 0 (CASCADE)", n, err)
	}
}
