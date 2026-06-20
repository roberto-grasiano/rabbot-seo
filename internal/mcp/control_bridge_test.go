package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestControlBridge_SatisfiesBridge(t *testing.T) {
	t.Parallel()
	// Compile-time guard that the concrete production type satisfies Bridge.
	// (NewControlBridge already returns Bridge, so asserting its result is
	// redundant — staticcheck QF1011 — and the concrete-type check is stronger.)
	var _ Bridge = (*controlBridge)(nil)
}

// newBridgeAgainstControl wires a production controlBridge against a real control
// server (httptest) whose ListSites hook returns canned, tier-enriched summaries —
// proving Sites() now flows over GET /v1/sites and no longer touches the store.
func newBridgeAgainstControl(t *testing.T, summaries []control.SiteSummary) Bridge {
	t.Helper()
	srv := control.NewServer(control.ServerOptions{
		Token:   "tok",
		Version: "0.1.0",
		Hooks: control.Hooks{
			ListSites: func(context.Context) ([]control.SiteSummary, error) { return summaries, nil },
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := control.NewClientWithBaseURL(ts.URL, "tok")
	return NewControlBridge(client)
}

func TestControlBridgeSitesOverControl(t *testing.T) {
	bridge := newBridgeAgainstControl(t, []control.SiteSummary{
		{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"},
		{ID: 2, URL: "https://b.test", Name: "B", Enabled: false, VerificationState: "throttled"},
	})

	got, err := bridge.Sites(context.Background())
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sites, want 2", len(got))
	}
	want := SiteView{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"}
	if got[0] != want {
		t.Fatalf("Sites()[0] = %+v, want %+v", got[0], want)
	}
	if got[1].VerificationState != "throttled" {
		t.Fatalf("Sites()[1].VerificationState = %q, want throttled", got[1].VerificationState)
	}
}

// TestControlBridgeRichResultsOverControl proves the JSON-identical seam: the
// control RichResultsResponse decodes straight through the loopback client into the
// mcp-local RichResultsView (the ReportView pattern), including per-entity
// eligibility and the not-found-as-data flag.
func TestControlBridgeRichResultsOverControl(t *testing.T) {
	t.Parallel()
	srv := control.NewServer(control.ServerOptions{
		Token: "tok",
		Hooks: control.Hooks{
			RichResults: func(_ context.Context, u string) (control.RichResultsResponse, error) {
				if u == "https://a.test/missing" {
					return control.RichResultsResponse{URL: u, NotFound: true}, nil
				}
				return control.RichResultsResponse{
					URL:         u,
					HasSnapshot: true,
					Profile:     "grr-2026.06",
					Entities:    []control.RichResultEntity{{Type: "Article", RawType: "BlogPosting", Eligible: false, Missing: []string{"headline"}}},
					Unprofiled:  2,
				}, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	br := NewControlBridge(control.NewClientWithBaseURL(ts.URL, "tok"))

	got, err := br.RichResults(context.Background(), "https://a.test/post")
	if err != nil {
		t.Fatalf("RichResults: %v", err)
	}
	if got.Profile != "grr-2026.06" || !got.HasSnapshot || got.Unprofiled != 2 {
		t.Fatalf("RichResults = %+v", got)
	}
	if len(got.Entities) != 1 || got.Entities[0].RawType != "BlogPosting" ||
		got.Entities[0].Eligible || got.Entities[0].Missing[0] != "headline" {
		t.Fatalf("entities = %+v", got.Entities)
	}

	nf, err := br.RichResults(context.Background(), "https://a.test/missing")
	if err != nil {
		t.Fatalf("RichResults not-found must be data, not error: %v", err)
	}
	if !nf.NotFound {
		t.Fatalf("want NotFound=true, got %+v", nf)
	}
}

// TestControlBridgeLinksOverControl proves the JSON-identical A9 links seam:
// control.LinksResponse decodes straight through the loopback client into the
// mcp-local LinksView, including the ranked linkers, the blast-radius summary, and
// the not-found-as-data flag (a never-linked URL). Both BlastRadius and WhatLinksTo
// hit the same GET /v1/links endpoint.
func TestControlBridgeLinksOverControl(t *testing.T) {
	t.Parallel()
	srv := control.NewServer(control.ServerOptions{
		Token: "tok",
		Hooks: control.Hooks{
			Links: func(_ context.Context, u string, limit int) (control.LinksResponse, error) {
				if u == "https://a.test/orphan" {
					return control.LinksResponse{URL: u, NotFound: true, Linkers: []control.LinkerView{}}, nil
				}
				return control.LinksResponse{
					URL: u, Inlinks: 3, InlinkTotal: 3, HighImportance: 1, WeightedInlinks: 2.4,
					Linkers: []control.LinkerView{{URLID: 5, URL: "https://a.test/hub", Importance: 0.9}},
				}, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	br := NewControlBridge(control.NewClientWithBaseURL(ts.URL, "tok"))

	got, err := br.WhatLinksTo(context.Background(), "https://a.test/p", 20)
	if err != nil {
		t.Fatalf("WhatLinksTo: %v", err)
	}
	if got.Inlinks != 3 || got.HighImportance != 1 || got.WeightedInlinks != 2.4 ||
		len(got.Linkers) != 1 || got.Linkers[0].Importance != 0.9 {
		t.Fatalf("WhatLinksTo = %+v", got)
	}

	bg, err := br.BlastRadius(context.Background(), "https://a.test/p", 5)
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if bg.Inlinks != 3 || bg.HighImportance != 1 {
		t.Fatalf("BlastRadius = %+v", bg)
	}

	nf, err := br.BlastRadius(context.Background(), "https://a.test/orphan", 5)
	if err != nil {
		t.Fatalf("links not-found must be data, not error: %v", err)
	}
	if !nf.NotFound {
		t.Fatalf("want NotFound=true, got %+v", nf)
	}
}

// TestControlBridgeGraphOverControl proves the JSON-identical A9 graph seam:
// control.GraphResponse decodes straight through the loopback client into the
// mcp-local GraphView (focus mode), and an unknown site is surfaced as
// NotFound-as-data (found=false → GraphView.NotFound=true).
func TestControlBridgeGraphOverControl(t *testing.T) {
	t.Parallel()
	srv := control.NewServer(control.ServerOptions{
		Token: "tok",
		Hooks: control.Hooks{
			Graph: func(_ context.Context, q control.GraphQuery) (control.GraphResponse, bool, error) {
				if q.SiteID == 999 {
					return control.GraphResponse{}, false, nil
				}
				d := 1
				return control.GraphResponse{
					Mode:  "focus",
					Focus: q.Focus,
					Hops:  2,
					Nodes: []control.GraphNodeView{
						{URL: q.Focus, Admitted: true, Importance: 0.7, GraphDepth: &d, InSitemap: true},
						{URL: "https://a.test/orphan", Admitted: false},
					},
					Edges:      []control.GraphEdgeView{{From: q.Focus, To: "https://a.test/orphan"}},
					Truncated:  true,
					TotalNodes: 50,
					TotalEdges: 120,
				}, true, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	br := NewControlBridge(control.NewClientWithBaseURL(ts.URL, "tok"))

	got, err := br.GetLinkGraph(context.Background(), GraphQuery{SiteID: 7, Focus: "https://a.test/p", Hops: 2})
	if err != nil {
		t.Fatalf("GetLinkGraph: %v", err)
	}
	if got.Mode != "focus" || len(got.Nodes) != 2 || len(got.Edges) != 1 ||
		!got.Truncated || got.TotalNodes != 50 || got.TotalEdges != 120 {
		t.Fatalf("GetLinkGraph = %+v", got)
	}
	if got.Nodes[0].GraphDepth == nil || *got.Nodes[0].GraphDepth != 1 || got.Nodes[1].Admitted {
		t.Fatalf("node payload not preserved through the seam: %+v", got.Nodes)
	}

	nf, err := br.GetLinkGraph(context.Background(), GraphQuery{SiteID: 999})
	if err != nil {
		t.Fatalf("graph unknown-site must be data, not error: %v", err)
	}
	if !nf.NotFound {
		t.Fatalf("want NotFound=true, got %+v", nf)
	}
}

func TestControlBridgeSitesDaemonDown(t *testing.T) {
	client := control.NewClientWithBaseURL("http://127.0.0.1:1", "tok") // nothing listening
	bridge := NewControlBridge(client)
	if _, err := bridge.Sites(context.Background()); err == nil {
		t.Fatal("Sites() against dead daemon: want error, got nil")
	}
}

// TestControlBridgeIndexStatusOverControl proves the GSC index-status seam: the
// control IndexStatusResponse decodes straight through the loopback client into the
// mcp-local IndexStatusView, including the not-found-as-data flag (un-inspected URL =
// data, HTTP 200, never an error) and the full verdict/coverage/canonical field set.
func TestControlBridgeIndexStatusOverControl(t *testing.T) {
	t.Parallel()
	srv := control.NewServer(control.ServerOptions{
		Token: "tok",
		Hooks: control.Hooks{
			IndexStatus: func(_ context.Context, u string) (control.IndexStatusResponse, error) {
				if u == "https://a.test/missing" {
					return control.IndexStatusResponse{URL: u, NotFound: true}, nil
				}
				return control.IndexStatusResponse{
					URL:             u,
					HasStatus:       true,
					Verdict:         "PASS",
					CoverageState:   "Submitted and indexed",
					IndexingState:   "INDEXING_ALLOWED",
					RobotsTxtState:  "ALLOWED",
					PageFetchState:  "SUCCESSFUL",
					GoogleCanonical: "https://a.test/p",
					UserCanonical:   "https://a.test/p",
					CrawledAs:       "DESKTOP",
					InspectedAt:     "2026-06-18T00:00:00Z",
					LastCrawlTime:   "2026-06-17T00:00:00Z",
				}, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	br := NewControlBridge(control.NewClientWithBaseURL(ts.URL, "tok"))

	got, err := br.IndexStatus(context.Background(), "https://a.test/p")
	if err != nil {
		t.Fatalf("IndexStatus: %v", err)
	}
	if !got.HasStatus || got.Verdict != "PASS" || got.CoverageState != "Submitted and indexed" ||
		got.GoogleCanonical != "https://a.test/p" || got.UserCanonical != "https://a.test/p" ||
		got.IndexingState != "INDEXING_ALLOWED" || got.RobotsTxtState != "ALLOWED" ||
		got.PageFetchState != "SUCCESSFUL" || got.CrawledAs != "DESKTOP" ||
		got.InspectedAt != "2026-06-18T00:00:00Z" || got.LastCrawlTime != "2026-06-17T00:00:00Z" {
		t.Fatalf("IndexStatus mapped = %+v", got)
	}

	// Un-inspected URL: surfaced as DATA (NotFound=true / HasStatus=false), never an error.
	nf, err := br.IndexStatus(context.Background(), "https://a.test/missing")
	if err != nil {
		t.Fatalf("un-inspected IndexStatus must be data, not error: %v", err)
	}
	if !nf.NotFound || nf.HasStatus {
		t.Fatalf("want NotFound=true HasStatus=false, got %+v", nf)
	}
}

// TestControlBridgeIndexStatusDaemonDown asserts an unreachable daemon surfaces as a
// Go error (distinct from the not-found-as-data path).
func TestControlBridgeIndexStatusDaemonDown(t *testing.T) {
	t.Parallel()
	client := control.NewClientWithBaseURL("http://127.0.0.1:1", "tok") // nothing listening
	br := NewControlBridge(client)
	if _, err := br.IndexStatus(context.Background(), "https://a.test/p"); err == nil {
		t.Fatal("IndexStatus against a dead daemon: want error, got nil")
	}
}

// TestControlBridgeSearchPerformanceOverControl proves the search-performance seam:
// the control response decodes into SearchPerformanceView, since is forwarded, the
// rows map across, and a no-data response is surfaced as data (HasData=false), never
// an error. Rows is normalized to a non-nil slice.
func TestControlBridgeSearchPerformanceOverControl(t *testing.T) {
	t.Parallel()
	var gotURL, gotSince string
	srv := control.NewServer(control.ServerOptions{
		Token: "tok",
		Hooks: control.Hooks{
			SearchPerformance: func(_ context.Context, u, since string) (control.SearchPerformanceResponse, error) {
				gotURL, gotSince = u, since
				if u == "https://a.test/nometrics" {
					return control.SearchPerformanceResponse{URL: u}, nil // HasData=false, no rows
				}
				return control.SearchPerformanceResponse{
					URL:     u,
					HasData: true,
					Rows: []control.SearchMetricView{
						{Query: "rabbit seo", Date: "2026-06-15", Clicks: 10, Impressions: 100, CTR: 0.1, Position: 4.2},
						{Query: "rabbit seo", Date: "2026-06-14", Clicks: 8, Impressions: 90, CTR: 0.089, Position: 4.6},
					},
				}, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	br := NewControlBridge(control.NewClientWithBaseURL(ts.URL, "tok"))

	since := "2026-06-10T00:00:00Z"
	got, err := br.SearchPerformance(context.Background(), "https://a.test/p", since)
	if err != nil {
		t.Fatalf("SearchPerformance: %v", err)
	}
	if gotURL != "https://a.test/p" || gotSince != since {
		t.Fatalf("daemon got (url=%q since=%q), want forwarded verbatim", gotURL, gotSince)
	}
	if !got.HasData || len(got.Rows) != 2 {
		t.Fatalf("SearchPerformance = %+v, want 2 rows / has_data=true", got)
	}
	if got.Rows[0].Query != "rabbit seo" || got.Rows[0].Impressions != 100 || got.Rows[0].Position != 4.2 {
		t.Fatalf("row[0] = %+v", got.Rows[0])
	}

	// No-data URL: surfaced as data (HasData=false), Rows a non-nil empty slice.
	nd, err := br.SearchPerformance(context.Background(), "https://a.test/nometrics", "")
	if err != nil {
		t.Fatalf("no-data SearchPerformance must be data, not error: %v", err)
	}
	if nd.HasData {
		t.Fatalf("want HasData=false, got %+v", nd)
	}
	if nd.Rows == nil {
		t.Fatalf("Rows = nil, want a non-nil empty slice (JSON []) ")
	}
}

func TestControlBridgeSearchPerformanceDaemonDown(t *testing.T) {
	t.Parallel()
	client := control.NewClientWithBaseURL("http://127.0.0.1:1", "tok")
	br := NewControlBridge(client)
	if _, err := br.SearchPerformance(context.Background(), "https://a.test/p", ""); err == nil {
		t.Fatal("SearchPerformance against a dead daemon: want error, got nil")
	}
}

func TestControlBridge_HealthAndStatusOverHTTP(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(control.StatusResponse{Version: "7.7.7", SiteCount: 4, Paused: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	client := control.NewClientWithBaseURL(ts.URL, "test-token")
	br := NewControlBridge(client)

	if err := br.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	st, err := br.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Version != "7.7.7" || st.SiteCount != 4 || !st.Paused {
		t.Fatalf("Status = %+v, want version 7.7.7 / SiteCount 4 / paused", st)
	}
}

func TestControlBridge_HealthDaemonDown(t *testing.T) {
	t.Parallel()

	// Point at a closed server so the client's transport fails -> ErrDaemonNotRunning.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	client := control.NewClientWithBaseURL(url, "test-token")
	br := NewControlBridge(client)
	if err := br.Health(context.Background()); !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("Health on down daemon = %v, want ErrDaemonNotRunning", err)
	}
}

func TestControlBridgeSitePassesCapFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sites/7/detail" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"url":"https://a.test","monitored_pages":2000,"max_pages":2000,"capped":true}`))
	}))
	defer srv.Close()

	b := NewControlBridge(control.NewClientWithBaseURL(srv.URL, "tok"))
	got, err := b.Site(context.Background(), 7)
	if err != nil {
		t.Fatalf("Site: %v", err)
	}
	if got.MonitoredPages != 2000 || got.MaxPages != 2000 || !got.Capped {
		t.Fatalf("cap fields not carried: %+v", got)
	}
}
