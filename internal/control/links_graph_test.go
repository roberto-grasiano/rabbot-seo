package control

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// ─── GET /v1/links (A9) ──────────────────────────────────────────────────────

// TestLinksHookOK asserts GET /v1/links?url=&limit= passes url + limit through and
// round-trips the DTO (linkers + blast-radius summary).
func TestLinksHookOK(t *testing.T) {
	t.Parallel()
	var (
		gotURL   string
		gotLimit int
	)
	ts := newTestServer(Hooks{
		Links: func(_ context.Context, url string, limit int) (LinksResponse, error) {
			gotURL, gotLimit = url, limit
			return LinksResponse{
				URL:             url,
				Inlinks:         3,
				InlinkTotal:     3,
				HighImportance:  1,
				WeightedInlinks: 2.4,
				Linkers: []LinkerView{
					{URLID: 5, URL: "https://a.test/hub", Importance: 0.9},
					{URLID: 6, URL: "https://a.test/blog", Importance: 0.4},
				},
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got LinksResponse
	if code := getJSON(t, ts, "/v1/links?url=https://a.test/p&limit=10", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotURL != "https://a.test/p" || gotLimit != 10 {
		t.Fatalf("hook args = url %q limit %d", gotURL, gotLimit)
	}
	if got.Inlinks != 3 || got.InlinkTotal != 3 || got.HighImportance != 1 ||
		got.WeightedInlinks != 2.4 || len(got.Linkers) != 2 || got.Linkers[0].Importance != 0.9 {
		t.Fatalf("unexpected links payload: %+v", got)
	}
}

// TestLinksHookMissingURLIs400 asserts a missing ?url= is a caller fault -> 400
// (the handleHistory / handleRichResults contract).
func TestLinksHookMissingURLIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Links: func(context.Context, string, int) (LinksResponse, error) {
			return LinksResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/links", nil); code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", code)
	}
}

// TestLinksHookBadLimitIs400 asserts a non-numeric or negative limit is a 400.
func TestLinksHookBadLimitIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Links: func(context.Context, string, int) (LinksResponse, error) {
			return LinksResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/links?url=x&limit=abc", "/v1/links?url=x&limit=-1"} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

// TestLinksHookNotFoundIsData asserts a never-linked URL is surfaced as data (200 +
// not_found:true), the handleHistory not-found pattern — NOT a 404.
func TestLinksHookNotFoundIsData(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Links: func(_ context.Context, url string, _ int) (LinksResponse, error) {
			return LinksResponse{URL: url, NotFound: true, Linkers: []LinkerView{}}, nil
		},
	})
	t.Cleanup(ts.Close)
	var got LinksResponse
	if code := getJSON(t, ts, "/v1/links?url=https://a.test/orphan", &got); code != http.StatusOK {
		t.Fatalf("not-found status = %d, want 200 (data, not 404)", code)
	}
	if !got.NotFound {
		t.Fatalf("want not_found=true, got %+v", got)
	}
}

// TestLinksHookNilIs501 asserts the route returns 501 when unwired.
func TestLinksHookNilIs501(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/links?url=https://x.example/", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestLinksRequiresToken asserts the route is behind auth (401 without token).
func TestLinksRequiresToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/links?url=x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}

// TestClientLinks asserts Client.Links round-trips the DTO.
func TestClientLinks(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Links: func(_ context.Context, url string, limit int) (LinksResponse, error) {
			return LinksResponse{URL: url, Inlinks: 2, InlinkTotal: 5, HighImportance: 1, Linkers: []LinkerView{{URLID: 1, URL: "https://a/x", Importance: 0.8}}}, nil
		},
	})
	got, err := c.Links(context.Background(), "https://a/p", 2)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	if got.Inlinks != 2 || got.InlinkTotal != 5 || got.HighImportance != 1 || len(got.Linkers) != 1 {
		t.Fatalf("Links = %+v", got)
	}
}

// ─── GET /v1/graph (A9) ──────────────────────────────────────────────────────

// TestGraphHookOK asserts GET /v1/graph passes the query through and round-trips a
// focus-mode export DTO.
func TestGraphHookOK(t *testing.T) {
	t.Parallel()
	var gotQ GraphQuery
	ts := newTestServer(Hooks{
		Graph: func(_ context.Context, q GraphQuery) (GraphResponse, bool, error) {
			gotQ = q
			d := 1
			return GraphResponse{
				Mode:  "focus",
				Focus: q.Focus,
				Hops:  2,
				Nodes: []GraphNodeView{
					{URL: "https://a.test/p", Admitted: true, Importance: 0.7, GraphDepth: &d, InSitemap: true},
					{URL: "https://a.test/orphan", Admitted: false},
				},
				Edges:      []GraphEdgeView{{From: "https://a.test/p", To: "https://a.test/orphan"}},
				Truncated:  true,
				TotalNodes: 50,
				TotalEdges: 120,
			}, true, nil
		},
	})
	t.Cleanup(ts.Close)

	var got GraphResponse
	if code := getJSON(t, ts, "/v1/graph?site_id=7&focus=https://a.test/p&hops=2&limit=20", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotQ.SiteID != 7 || gotQ.Focus != "https://a.test/p" || gotQ.Hops != 2 || gotQ.Limit != 20 {
		t.Fatalf("hook query = %+v", gotQ)
	}
	if got.Mode != "focus" || len(got.Nodes) != 2 || len(got.Edges) != 1 ||
		!got.Truncated || got.TotalNodes != 50 || got.TotalEdges != 120 {
		t.Fatalf("unexpected graph payload: %+v", got)
	}
	if got.Nodes[0].GraphDepth == nil || *got.Nodes[0].GraphDepth != 1 || got.Nodes[1].Admitted {
		t.Fatalf("node payload not preserved: %+v", got.Nodes)
	}
}

// TestGraphHookOverview asserts an overview-mode export DTO round-trips (segment
// or folder grouping).
func TestGraphHookOverview(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Graph: func(_ context.Context, q GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{
				Mode:       "overview",
				Grouping:   "folder",
				Groups:     []GraphGroupView{{Name: "/blog"}, {Name: "/product"}},
				GroupEdges: []GraphGroupEdgeView{{From: "/blog", To: "/product", Weight: 12}},
				TotalNodes: 2,
				TotalEdges: 1,
			}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	var got GraphResponse
	if code := getJSON(t, ts, "/v1/graph?site_id=7", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Mode != "overview" || got.Grouping != "folder" || len(got.Groups) != 2 ||
		len(got.GroupEdges) != 1 || got.GroupEdges[0].Weight != 12 {
		t.Fatalf("unexpected overview payload: %+v", got)
	}
}

// TestGraphHookMissingSiteIDIs400 asserts a missing/non-numeric site_id is a 400.
func TestGraphHookMissingSiteIDIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/graph", "/v1/graph?site_id=abc"} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

// TestGraphHookHopsTooBigIs400 asserts hops > 2 is rejected clearly with a 400 —
// BEFORE the hook runs (criterion 8: "hops=3 rejected clearly").
func TestGraphHookHopsTooBigIs400(t *testing.T) {
	t.Parallel()
	called := false
	ts := newTestServer(Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			called = true
			return GraphResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/graph?site_id=1&focus=https://a/p&hops=3", nil); code != http.StatusBadRequest {
		t.Fatalf("hops=3 status = %d, want 400", code)
	}
	if called {
		t.Fatalf("hook must NOT run when hops > 2 (rejected at the handler)")
	}
}

// TestGraphHookBadHopsOrLimitIs400 asserts a non-numeric/negative hops or limit is a 400.
func TestGraphHookBadHopsOrLimitIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{
		"/v1/graph?site_id=1&hops=abc",
		"/v1/graph?site_id=1&hops=-1",
		"/v1/graph?site_id=1&limit=abc",
		"/v1/graph?site_id=1&limit=-5",
	} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

// TestGraphHookBadRequestFromHookIs400 asserts an ErrBadRequest-wrapped hook error
// (e.g. a bad mode the handler does not pre-validate) maps to 400.
func TestGraphHookBadRequestFromHookIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{}, false, fmt.Errorf("unknown export mode: %w", ErrBadRequest)
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/graph?site_id=1&mode=bogus", nil); code != http.StatusBadRequest {
		t.Fatalf("bad-mode hook status = %d, want 400", code)
	}
}

// TestGraphHookNotFoundIsData asserts an unknown site id is surfaced as data (HTTP
// 200 NotFoundResponse), matching handleScore — NOT a 404.
func TestGraphHookNotFoundIsData(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{}, false, nil
		},
	})
	t.Cleanup(ts.Close)
	var raw struct {
		NotFound bool `json:"not_found"`
	}
	if code := getJSON(t, ts, "/v1/graph?site_id=999", &raw); code != http.StatusOK {
		t.Fatalf("unknown site status = %d, want 200 (errors-as-data)", code)
	}
	if !raw.NotFound {
		t.Fatalf("unknown site should set not_found=true; got %+v", raw)
	}
}

// TestGraphHookNilIs501 asserts the route returns 501 when unwired.
func TestGraphHookNilIs501(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/graph?site_id=1", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestGraphRequiresToken asserts the route is behind auth (401 without token).
func TestGraphRequiresToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/graph?site_id=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}

// TestClientGraph asserts Client.Graph round-trips the DTO with found=true.
func TestClientGraph(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Graph: func(_ context.Context, q GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{Mode: "focus", Focus: q.Focus, Nodes: []GraphNodeView{{URL: q.Focus, Admitted: true}}, TotalNodes: 1}, true, nil
		},
	})
	got, found, err := c.Graph(context.Background(), GraphQuery{SiteID: 9, Focus: "https://a/p", Hops: 2})
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if !found || got.Mode != "focus" || got.Focus != "https://a/p" || len(got.Nodes) != 1 {
		t.Fatalf("Graph = %+v found=%v", got, found)
	}
}

// TestClientGraphNotFound asserts an unknown site is surfaced as found=false with a
// nil error (errors-as-data for the MCP bridge), matching SiteDetailFound / Score.
func TestClientGraphNotFound(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Graph: func(context.Context, GraphQuery) (GraphResponse, bool, error) {
			return GraphResponse{}, false, nil
		},
	})
	_, found, err := c.Graph(context.Background(), GraphQuery{SiteID: 999})
	if err != nil {
		t.Fatalf("Graph unknown err = %v, want nil", err)
	}
	if found {
		t.Fatalf("Graph unknown found = true, want false")
	}
}
