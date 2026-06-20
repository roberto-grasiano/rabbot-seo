package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestBuildControlHooksPopulatesM1Hooks(t *testing.T) {
	var paused bool
	deps := HookDeps{
		Pause: func(ctx context.Context, p bool) error { paused = p; return nil },
		AddSite: func(ctx context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error) {
			return control.AddSiteResponse{SiteID: 7}, nil
		},
		Crawl: func(ctx context.Context, req control.CrawlRequest) (control.CrawlResponse, error) {
			return control.CrawlResponse{Queued: 1}, nil
		},
		RemoveSite: func(ctx context.Context, id int64, purge bool) error { return nil },
		SetConfig:  func(ctx context.Context, req control.ConfigSetRequest) error { return nil },
		Status: func(ctx context.Context) (control.StatusResponse, error) {
			return control.StatusResponse{DueCount: 5, QueueDepth: 3, EgressIP: []string{"203.0.113.7"}}, nil
		},
	}
	hooks := BuildControlHooks(deps)

	if hooks.Pause == nil || hooks.Crawl == nil || hooks.AddSite == nil ||
		hooks.RemoveSite == nil || hooks.SetConfig == nil || hooks.Status == nil {
		t.Fatal("BuildControlHooks left a required M1 hook nil")
	}
	if err := hooks.Pause(context.Background(), true); err != nil || !paused {
		t.Fatalf("Pause hook not wired: err=%v paused=%v", err, paused)
	}
	st, err := hooks.Status(context.Background())
	if err != nil {
		t.Fatalf("Status hook: %v", err)
	}
	if st.DueCount != 5 || st.QueueDepth != 3 || len(st.EgressIP) != 1 {
		t.Errorf("richer Status not surfaced: %+v", st)
	}
	_ = model.FetchOK // model import kept for parity with hook signatures
}

func TestBuildControlHooksMapsReadHooks(t *testing.T) {
	var (
		listCalled, detailCalled, issuesCalled, historyCalled bool
	)
	hooks := BuildControlHooks(HookDeps{
		ListSites: func(context.Context) ([]control.SiteSummary, error) {
			listCalled = true
			return nil, nil
		},
		SiteDetail: func(context.Context, int64) (control.SiteDetailResponse, bool, error) {
			detailCalled = true
			return control.SiteDetailResponse{}, true, nil
		},
		Issues: func(context.Context, control.IssueQuery) ([]control.IssueView, error) {
			issuesCalled = true
			return nil, nil
		},
		History: func(context.Context, string, time.Time) (control.HistoryResponse, error) {
			historyCalled = true
			return control.HistoryResponse{}, nil
		},
	})

	if hooks.ListSites == nil || hooks.SiteDetail == nil || hooks.Issues == nil || hooks.History == nil {
		t.Fatal("read hooks not mapped into control.Hooks")
	}
	_, _ = hooks.ListSites(context.Background())
	_, _, _ = hooks.SiteDetail(context.Background(), 1)
	_, _ = hooks.Issues(context.Background(), control.IssueQuery{})
	_, _ = hooks.History(context.Background(), "u", time.Time{})
	if !listCalled || !detailCalled || !issuesCalled || !historyCalled {
		t.Fatalf("mapped closures not invoked: list=%v detail=%v issues=%v history=%v",
			listCalled, detailCalled, issuesCalled, historyCalled)
	}
}

// Nil read deps leave the control hook nil (route returns 501).
func TestBuildControlHooksNilReadHooksStayNil(t *testing.T) {
	hooks := BuildControlHooks(HookDeps{})
	if hooks.ListSites != nil || hooks.SiteDetail != nil || hooks.Issues != nil || hooks.History != nil {
		t.Fatal("unset read deps should leave control hooks nil")
	}
}

func TestBuildControlHooks_MapsVerify(t *testing.T) {
	called := false
	hooks := BuildControlHooks(HookDeps{
		Verify: func(_ context.Context, req control.VerifyRequest) (control.VerifyResponse, error) {
			called = true
			return control.VerifyResponse{SiteID: req.SiteID, Token: "rab_X"}, nil
		},
	})
	if hooks.Verify == nil {
		t.Fatal("BuildControlHooks did not map Verify")
	}
	resp, err := hooks.Verify(context.Background(), control.VerifyRequest{SiteID: 3, Action: "begin"})
	if err != nil {
		t.Fatalf("Verify hook: %v", err)
	}
	if !called || resp.SiteID != 3 || resp.Token != "rab_X" {
		t.Fatalf("Verify hook = %+v (called=%v), want it threaded through", resp, called)
	}
}

func TestBuildControlHooks_NilVerifyStaysNil(t *testing.T) {
	hooks := BuildControlHooks(HookDeps{})
	if hooks.Verify != nil {
		t.Fatal("unset Verify dep should leave Hooks.Verify nil (route 501)")
	}
}
