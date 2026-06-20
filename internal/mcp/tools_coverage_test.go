package mcpsrv

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestGetCoverageHandler_OK asserts get_coverage against a mock Bridge returns the
// DTO (acceptance 11) and passes the site id through.
func TestGetCoverageHandler_OK(t *testing.T) {
	t.Parallel()
	m := &mockBridge{coverage: CoverageView{
		HasSitemap:           true,
		SeedStatus:           200,
		SitemappedUncrawled:  5,
		SitemappedUnadmitted: 2,
		CrawledNotInSitemap:  3,
		SampleUncrawled:      []string{"https://a.test/u1"},
		SampleNotInSitemap:   []string{"https://a.test/n1"},
	}}
	h := getCoverageHandler(m)
	_, out, err := h(context.Background(), nil, GetCoverageInput{SiteID: 7})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if m.lastCoverageSiteID != 7 {
		t.Fatalf("site id = %d, want 7", m.lastCoverageSiteID)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %q", out.Error)
	}
	c := out.Coverage
	if !c.HasSitemap || c.SeedStatus != 200 || c.SitemappedUncrawled != 5 ||
		c.SitemappedUnadmitted != 2 || c.CrawledNotInSitemap != 3 ||
		len(c.SampleUncrawled) != 1 || len(c.SampleNotInSitemap) != 1 {
		t.Fatalf("coverage out = %+v", c)
	}
}

// TestGetCoverageHandler_NotFound asserts an unknown site is reported as data
// (CoverageView.NotFound=true), not a Go error.
func TestGetCoverageHandler_NotFound(t *testing.T) {
	t.Parallel()
	m := &mockBridge{coverage: CoverageView{NotFound: true}}
	h := getCoverageHandler(m)
	_, out, err := h(context.Background(), nil, GetCoverageInput{SiteID: 999})
	if err != nil {
		t.Fatalf("not-found must be data, not error: %v", err)
	}
	if !out.Coverage.NotFound || out.Error != "" {
		t.Fatalf("want not-found-as-data, got %+v", out)
	}
}

// TestGetCoverageHandler_DaemonDown asserts a down daemon is errors-as-data (the
// Error field is set), never a tool crash.
func TestGetCoverageHandler_DaemonDown(t *testing.T) {
	t.Parallel()
	m := &mockBridge{coverageErr: control.ErrDaemonNotRunning}
	h := getCoverageHandler(m)
	_, out, err := h(context.Background(), nil, GetCoverageInput{SiteID: 1})
	if err != nil {
		t.Fatalf("daemon-down must be data, not error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("expected errors-as-data Error field")
	}
}
