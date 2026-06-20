package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestHandleFetchClassSuppressesSEOOnHardBlock(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	seoEvents := []Event{{SiteID: 1, Site: "ex.com", URL: "https://ex.com/p", ChangeType: "title", Severity: model.SeverityWarning, After: "B"}}
	handled, err := p.HandleFetchClass(context.Background(), AccessContext{
		SiteID: 1, Site: "ex.com", URL: "https://ex.com/p", FetchClass: model.FetchHardBlock, Detector: "cloudflare",
	}, seoEvents)
	if err != nil {
		t.Fatalf("HandleFetchClass: %v", err)
	}
	if !handled {
		t.Fatal("non-ok fetch must be handled (SEO suppressed)")
	}
	// No SEO alert dispatched; exactly one operational incident.
	for _, a := range disp.got {
		if a.ChangeType == "title" {
			t.Errorf("SEO alert must be suppressed on hard_block, got %+v", a)
		}
	}
	if len(st.opened) != 1 {
		t.Fatalf("expected 1 operational incident opened, got %d", len(st.opened))
	}
	if st.opened[0].GroupKey != GroupKey("ex.com", model.ChangeTypeMonitoringBlocked) {
		t.Errorf("operational incident group_key = %q", st.opened[0].GroupKey)
	}
	if len(disp.got) != 1 || !disp.got[0].Operational || disp.got[0].ChangeType != model.ChangeTypeMonitoringBlocked {
		t.Errorf("expected 1 operational dispatch labeled monitoring_blocked, got %+v", disp.got)
	}
}

func TestHandleFetchClassUnreachable(t *testing.T) {
	now := time.Now()
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)
	handled, err := p.HandleFetchClass(context.Background(), AccessContext{
		SiteID: 1, Site: "ex.com", URL: "https://ex.com/p", FetchClass: model.FetchUnreachable,
	}, nil)
	if err != nil || !handled {
		t.Fatalf("unreachable must be handled, handled=%v err=%v", handled, err)
	}
	if disp.got[0].ChangeType != model.ChangeTypeMonitoringUnreachable {
		t.Errorf("expected monitoring_unreachable, got %q", disp.got[0].ChangeType)
	}
}

func TestHandleFetchClassOKResolvesAndPassesThrough(t *testing.T) {
	now := time.Now()
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	// Pre-open an operational incident as if a prior fetch was blocked.
	opEv := Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/p", ChangeType: model.ChangeTypeMonitoringBlocked, Severity: model.SeverityCritical, Operational: true}
	_ = p.Ingest(context.Background(), opEv)

	handled, err := p.HandleFetchClass(context.Background(), AccessContext{
		SiteID: 1, Site: "ex.com", URL: "https://ex.com/p", FetchClass: model.FetchOK,
	}, nil)
	if err != nil {
		t.Fatalf("HandleFetchClass ok: %v", err)
	}
	if handled {
		t.Error("ok fetch must NOT be handled here (SEO pipeline proceeds normally)")
	}
	if len(st.closed) != 1 {
		t.Errorf("ok fetch must auto-close the operational incident, closed=%v", st.closed)
	}
}
