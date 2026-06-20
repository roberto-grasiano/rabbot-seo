package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

type fakeFileStore struct {
	saved []model.FileSnapshot
	// preload seeds LatestFileSnapshot with a prior snapshot that was NOT written
	// via SaveFileSnapshot (so the change-detection path sees a real "prev").
	preload []model.FileSnapshot
	// latestErr, when set, is returned by LatestFileSnapshot (to exercise the
	// error-wrapping path).
	latestErr error
}

// fakeIngestor records every Event RefreshRobots feeds it.
type fakeIngestor struct {
	events []alerts.Event
	err    error
}

func (f *fakeIngestor) Ingest(_ context.Context, e alerts.Event) error {
	f.events = append(f.events, e)
	return f.err
}

func (f *fakeFileStore) SaveFileSnapshot(ctx context.Context, fs model.FileSnapshot) (int64, error) {
	f.saved = append(f.saved, fs)
	return int64(len(f.saved)), nil
}
func (f *fakeFileStore) LatestFileSnapshot(ctx context.Context, siteID int64, kind model.FileSnapshotKind) (model.FileSnapshot, bool, error) {
	if f.latestErr != nil {
		return model.FileSnapshot{}, false, f.latestErr
	}
	for i := len(f.saved) - 1; i >= 0; i-- {
		if f.saved[i].SiteID == siteID && f.saved[i].Kind == kind {
			return f.saved[i], true, nil
		}
	}
	for i := len(f.preload) - 1; i >= 0; i-- {
		if f.preload[i].SiteID == siteID && f.preload[i].Kind == kind {
			return f.preload[i], true, nil
		}
	}
	return model.FileSnapshot{}, false, nil
}

func TestRefreshRobotsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin/\n"))
	}))
	defer srv.Close()

	fs := &fakeFileStore{}
	rc := frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Minute)
	r := &SideTimers{FileStore: fs, Robots: rc, Now: func() time.Time { return time.Unix(1000, 0).UTC() }}

	if err := r.RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots() error = %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("saved %d file snapshots, want 1", len(fs.saved))
	}
	if fs.saved[0].Kind != model.FileKindRobots {
		t.Errorf("kind = %q, want robots", fs.saved[0].Kind)
	}
	if fs.saved[0].ContentSHA256 == "" {
		t.Errorf("ContentSHA256 empty")
	}
	if fs.saved[0].HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", fs.saved[0].HTTPStatus)
	}
}

// TestRefreshRobotsDedupsUnchanged guards F23: robots.txt is near-static and the
// side-timer fires every 5 minutes, so RefreshRobots must NOT write a new
// file_snapshot row when the content (hash) and HTTP status are unchanged — else
// the table grows ~288 redundant rows/site/day forever. A real content change must
// still persist a new row.
func TestRefreshRobotsDedupsUnchanged(t *testing.T) {
	var mu sync.Mutex
	body := "User-agent: *\nDisallow: /admin/\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := body
		mu.Unlock()
		_, _ = w.Write([]byte(b))
	}))
	defer srv.Close()

	fs := &fakeFileStore{}
	// A near-zero cache TTL avoids RobotsCache's own caching masking a later change.
	mkTimer := func() *SideTimers {
		rc := frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Nanosecond)
		return &SideTimers{FileStore: fs, Robots: rc, Now: func() time.Time { return time.Unix(1000, 0).UTC() }}
	}

	if err := mkTimer().RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots #1: %v", err)
	}
	if err := mkTimer().RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots #2 (identical): %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("identical robots.txt must not write a second snapshot, saved %d", len(fs.saved))
	}

	// Now change the content: a new snapshot must be written.
	mu.Lock()
	body = "User-agent: *\nDisallow: /private/\n"
	mu.Unlock()
	if err := mkTimer().RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots #3 (changed): %v", err)
	}
	if len(fs.saved) != 2 {
		t.Fatalf("a changed robots.txt must write a new snapshot, saved %d", len(fs.saved))
	}
}

// robotsServer serves a fixed robots.txt body once and returns a fresh-TTL cache,
// so each RefreshRobots call re-fetches (no robots cache masking the change).
func robotsServer(t *testing.T, body string) (*httptest.Server, *frontier.RobotsCache) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Nanosecond)
}

// TestRefreshRobotsIngestsChangeEvent closes the robots half of M2 #8: when a
// PRIOR robots.txt snapshot exists and the content changed, RefreshRobots must
// diff (diff.CompareFile) and ingest a robots_txt change Event at critical
// severity into the alerts pipeline — mirroring the per-URL change stream.
func TestRefreshRobotsIngestsChangeEvent(t *testing.T) {
	srv, rc := robotsServer(t, "User-agent: *\nDisallow: /\n")

	ing := &fakeIngestor{}
	fs := &fakeFileStore{preload: []model.FileSnapshot{{
		ID: 7, SiteID: 1, Kind: model.FileKindRobots,
		ContentSHA256: "different-prior-hash", HTTPStatus: 200,
	}}}
	r := &SideTimers{
		FileStore: fs, Robots: rc, Alerts: ing,
		Now: func() time.Time { return time.Unix(1000, 0).UTC() },
	}

	if err := r.RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots() error = %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("changed robots.txt must write a snapshot, saved %d", len(fs.saved))
	}
	if len(ing.events) != 1 {
		t.Fatalf("ingested %d events, want 1 (robots_txt change)", len(ing.events))
	}
	ev := ing.events[0]
	if ev.ChangeType != "robots_txt" {
		t.Errorf("ChangeType = %q, want robots_txt", ev.ChangeType)
	}
	if ev.Severity != model.SeverityCritical {
		t.Errorf("Severity = %q, want critical", ev.Severity)
	}
	if ev.URL != "" {
		t.Errorf("URL = %q, want empty (site-level event)", ev.URL)
	}
	if ev.SiteID != 1 {
		t.Errorf("SiteID = %d, want 1", ev.SiteID)
	}
	if ev.Site != srv.URL {
		t.Errorf("Site = %q, want %q (baseURL, matching per-URL events)", ev.Site, srv.URL)
	}
}

// TestRefreshRobotsWrapsErrorWithSiteContext guards the Info finding: RefreshRobots
// returned bare errors with no context, so a failure was unattributable to a site.
// Each returned error must wrap the underlying error (errors.Is-able) AND carry the
// site id in its message for debuggability.
func TestRefreshRobotsWrapsErrorWithSiteContext(t *testing.T) {
	srv, rc := robotsServer(t, "User-agent: *\nDisallow: /\n")

	boom := errors.New("store boom")
	fs := &fakeFileStore{latestErr: boom}
	r := &SideTimers{
		FileStore: fs, Robots: rc,
		Now: func() time.Time { return time.Unix(1000, 0).UTC() },
	}

	err := r.RefreshRobots(context.Background(), 42, srv.URL)
	if err == nil {
		t.Fatal("RefreshRobots must surface the LatestFileSnapshot error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("returned error must wrap the underlying error (errors.Is), got %v", err)
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("returned error must carry the site id for attribution, got %q", err.Error())
	}
}

// TestRefreshRobotsFirstSnapshotNoEvent: the very first snapshot is a baseline —
// there is no prior to diff against, so no alert fires.
func TestRefreshRobotsFirstSnapshotNoEvent(t *testing.T) {
	srv, rc := robotsServer(t, "User-agent: *\nDisallow: /admin/\n")

	ing := &fakeIngestor{}
	fs := &fakeFileStore{}
	r := &SideTimers{
		FileStore: fs, Robots: rc, Alerts: ing,
		Now: func() time.Time { return time.Unix(1000, 0).UTC() },
	}

	if err := r.RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots() error = %v", err)
	}
	if len(fs.saved) != 1 {
		t.Fatalf("first snapshot must be written, saved %d", len(fs.saved))
	}
	if len(ing.events) != 0 {
		t.Fatalf("baseline snapshot must not alert, ingested %d events", len(ing.events))
	}
}

// TestRefreshRobotsUnchangedNoEvent: a prior snapshot with identical content/status
// is a no-op — no new row, no alert.
func TestRefreshRobotsUnchangedNoEvent(t *testing.T) {
	body := "User-agent: *\nDisallow: /admin/\n"
	srv, rc := robotsServer(t, body)

	ing := &fakeIngestor{}
	fs := &fakeFileStore{preload: []model.FileSnapshot{{
		ID: 9, SiteID: 1, Kind: model.FileKindRobots,
		ContentSHA256: extract.ContentSHA256(body), HTTPStatus: 200,
	}}}
	r := &SideTimers{
		FileStore: fs, Robots: rc, Alerts: ing,
		Now: func() time.Time { return time.Unix(1000, 0).UTC() },
	}

	if err := r.RefreshRobots(context.Background(), 1, srv.URL); err != nil {
		t.Fatalf("RefreshRobots() error = %v", err)
	}
	if len(fs.saved) != 0 {
		t.Fatalf("unchanged robots.txt must not write a snapshot, saved %d", len(fs.saved))
	}
	if len(ing.events) != 0 {
		t.Fatalf("unchanged robots.txt must not alert, ingested %d events", len(ing.events))
	}
}
