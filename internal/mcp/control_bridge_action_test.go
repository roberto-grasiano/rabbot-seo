package mcpsrv

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// newActionBridgeAgainstControl spins a real control.Server (with the given hooks)
// on an httptest server and returns a production controlBridge pointed at it. The
// action methods go over the loopback control client, so no store handle is needed
// (D2: NewControlBridge dropped its dbPath argument in Phase 1). Named distinctly
// from the Phase-1 summaries-based helper in control_bridge_test.go.
func newActionBridgeAgainstControl(t *testing.T, hooks control.Hooks) Bridge {
	t.Helper()
	srv := control.NewServer(control.ServerOptions{Token: "tok", Version: "test", Hooks: hooks})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := control.NewClientWithBaseURL(ts.URL, "tok")
	return NewControlBridge(client)
}

func TestControlBridge_AddSite(t *testing.T) {
	t.Parallel()
	var got control.AddSiteRequest
	b := newActionBridgeAgainstControl(t, control.Hooks{
		AddSite: func(_ context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error) {
			got = req
			return control.AddSiteResponse{SiteID: 99}, nil
		},
	})
	resp, err := b.AddSite(context.Background(), control.AddSiteRequest{URL: "https://x.test", Name: "X"})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	if resp.SiteID != 99 {
		t.Fatalf("SiteID = %d, want 99", resp.SiteID)
	}
	if got.URL != "https://x.test" || got.Name != "X" {
		t.Fatalf("hook received %+v, want url/name passed through", got)
	}
}

func TestControlBridge_Recheck(t *testing.T) {
	t.Parallel()
	var gotTarget string
	b := newActionBridgeAgainstControl(t, control.Hooks{
		Crawl: func(_ context.Context, req control.CrawlRequest) (control.CrawlResponse, error) {
			gotTarget = req.Target
			return control.CrawlResponse{Queued: 4}, nil
		},
	})
	resp, err := b.Recheck(context.Background(), "https://x.test")
	if err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if resp.Queued != 4 || gotTarget != "https://x.test" {
		t.Fatalf("Recheck queued=%d target=%q, want 4 / https://x.test", resp.Queued, gotTarget)
	}
}

func TestControlBridge_PauseResume(t *testing.T) {
	t.Parallel()
	var paused []bool
	b := newActionBridgeAgainstControl(t, control.Hooks{
		Pause: func(_ context.Context, p bool) error { paused = append(paused, p); return nil },
	})
	if err := b.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := b.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(paused) != 2 || !paused[0] || paused[1] {
		t.Fatalf("pause calls = %v, want [true false]", paused)
	}
}

func TestControlBridge_IgnoreIssue(t *testing.T) {
	t.Parallel()
	var gotID int64
	b := newActionBridgeAgainstControl(t, control.Hooks{
		IgnoreIssue: func(_ context.Context, id int64) error { gotID = id; return nil },
	})
	if err := b.IgnoreIssue(context.Background(), 7); err != nil {
		t.Fatalf("IgnoreIssue: %v", err)
	}
	if gotID != 7 {
		t.Fatalf("ignored id = %d, want 7", gotID)
	}
}

func TestControlBridge_TestAlert(t *testing.T) {
	t.Parallel()
	var gotNotifier string
	b := newActionBridgeAgainstControl(t, control.Hooks{
		NotifyTest: func(_ context.Context, n string) error { gotNotifier = n; return nil },
	})
	if err := b.TestAlert(context.Background(), "slack"); err != nil {
		t.Fatalf("TestAlert: %v", err)
	}
	if gotNotifier != "slack" {
		t.Fatalf("notifier = %q, want slack", gotNotifier)
	}
}
