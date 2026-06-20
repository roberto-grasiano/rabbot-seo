package alerts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
)

func TestFingerprintStable(t *testing.T) {
	a := Event{Site: "example.com", URL: "https://example.com/p", ChangeType: "title", Severity: model.SeverityWarning}
	f1 := Fingerprint(a)
	f2 := Fingerprint(a)
	if f1 != f2 {
		t.Errorf("fingerprint not stable: %q vs %q", f1, f2)
	}
	b := a
	b.URL = "https://example.com/q"
	if Fingerprint(b) == f1 {
		t.Errorf("different URL must yield different fingerprint")
	}
}

func TestGroupKey(t *testing.T) {
	if got := GroupKey("example.com", "title"); got != "example.com|title" {
		t.Errorf("GroupKey = %q, want example.com|title", got)
	}
}

type fakeIncidentStore struct {
	mu       sync.Mutex // guards all fields (the pipeline may call concurrently)
	openByFP map[string]model.Alert
	opened   []model.Alert
	updated  []model.Alert
	closed   []int64
	nextID   int64

	// members models the alert_members table: alertID -> set of live member URLs.
	members map[int64]map[string]bool
	// suppressMembers, when true, makes the member-tracking methods behave as if no
	// alert_members table is populated (legacy incident): AddAlertMember is a no-op,
	// RemoveAlertMember always returns remaining=0, CountAlertMembers returns 0.
	suppressMembers bool
	// addedMembers records every (alertID, url) passed to AddAlertMember.
	addedMembers []memberCall
}

type memberCall struct {
	alertID int64
	url     string
}

func newFakeIncidentStore() *fakeIncidentStore {
	return &fakeIncidentStore{
		openByFP: map[string]model.Alert{},
		members:  map[int64]map[string]bool{},
	}
}
func (f *fakeIncidentStore) GetOpenIncident(ctx context.Context, fp string) (model.Alert, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.openByFP[fp]
	return a, ok, nil
}
func (f *fakeIncidentStore) OpenIncident(ctx context.Context, a model.Alert) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	a.ID = f.nextID
	f.openByFP[a.Fingerprint] = a
	f.opened = append(f.opened, a)
	return a.ID, nil
}
func (f *fakeIncidentStore) UpdateIncident(ctx context.Context, a model.Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openByFP[a.Fingerprint] = a
	f.updated = append(f.updated, a)
	return nil
}
func (f *fakeIncidentStore) CloseIncident(ctx context.Context, id int64, at time.Time, auto bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	for fp, a := range f.openByFP {
		if a.ID == id {
			delete(f.openByFP, fp)
		}
	}
	return nil
}
func (f *fakeIncidentStore) openedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened)
}

func (f *fakeIncidentStore) AddAlertMember(ctx context.Context, alertID int64, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addedMembers = append(f.addedMembers, memberCall{alertID: alertID, url: url})
	if f.suppressMembers {
		return nil
	}
	set := f.members[alertID]
	if set == nil {
		set = map[string]bool{}
		f.members[alertID] = set
	}
	set[url] = true
	return nil
}

func (f *fakeIncidentStore) RemoveAlertMember(ctx context.Context, alertID int64, url string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suppressMembers {
		return 0, nil
	}
	set := f.members[alertID]
	if set != nil {
		delete(set, url)
	}
	return len(set), nil
}

func (f *fakeIncidentStore) CountAlertMembers(ctx context.Context, alertID int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suppressMembers {
		return 0, nil
	}
	return len(f.members[alertID]), nil
}

func (f *fakeIncidentStore) HasOpenIncidentMember(ctx context.Context, fp, url string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.openByFP[fp]
	if !ok {
		return false, nil
	}
	return f.members[a.ID][url], nil
}

func (f *fakeIncidentStore) closedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closed)
}

type capturingDispatcher struct{ got []notify.Alert }

func (c *capturingDispatcher) Dispatch(ctx context.Context, a notify.Alert) error {
	c.got = append(c.got, a)
	return nil
}

func newTestPipeline(now time.Time, st IncidentStore, disp Dispatcher) *Pipeline {
	return newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
	)
}

func TestIngestOpensIncidentAndNotifies(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	ev := Event{SiteID: 1, Site: "example.com", URL: "https://example.com/p",
		ChangeType: "indexability", Severity: model.SeverityCritical, Before: "indexable", After: "noindex"}
	if err := p.Ingest(context.Background(), ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(st.opened) != 1 {
		t.Fatalf("expected 1 incident opened, got %d", len(st.opened))
	}
	if len(disp.got) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(disp.got))
	}
	if disp.got[0].Severity != model.SeverityCritical || disp.got[0].ChangeType != "indexability" {
		t.Errorf("dispatched alert wrong: %+v", disp.got[0])
	}
}

func TestIngestDedupWithinWindow(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	ev := Event{SiteID: 1, Site: "example.com", URL: "https://example.com/p",
		ChangeType: "title", Severity: model.SeverityWarning, Before: "A", After: "B"}
	_ = p.Ingest(context.Background(), ev)
	_ = p.Ingest(context.Background(), ev) // duplicate within window

	if len(st.opened) != 1 {
		t.Errorf("dedup failed: opened %d incidents, want 1", len(st.opened))
	}
	if len(disp.got) != 1 {
		t.Errorf("dedup failed: dispatched %d, want 1", len(disp.got))
	}
}

func TestIngestGroupsBySiteAndChangeType(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	// Two different URLs, same site + change_type + severity -> one incident, accrues count.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a", ChangeType: "title", Severity: model.SeverityWarning, After: "x"})
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/b", ChangeType: "title", Severity: model.SeverityWarning, After: "y"})

	if len(st.opened) != 1 {
		t.Errorf("grouping failed: opened %d, want 1", len(st.opened))
	}
	if len(st.updated) == 0 {
		t.Errorf("second event should update the grouped incident")
	}
	last := st.openByFP[Fingerprint(Event{Site: "ex.com", URL: "", ChangeType: "title", Severity: model.SeverityWarning})]
	if last.AffectedCount < 2 {
		t.Errorf("incident affected_count = %d, want >= 2", last.AffectedCount)
	}
}

// TestIngestConcurrentSameFingerprintOpensOnce guards the per-fingerprint lock:
// the crawl scheduler dispatches up to MaxParallel fetches concurrently, and two
// URLs sharing a group identity (site+change_type+severity) map to one fingerprint.
// Without serializing the get-then-open, several goroutines all observe "no open
// incident" and each opens one — a logical race the -race detector cannot see.
func TestIngestConcurrentSameFingerprintOpensOnce(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same site+change_type+severity (one fingerprint), distinct URLs.
			_ = p.Ingest(context.Background(), Event{
				SiteID: 1, Site: "ex.com", URL: fmt.Sprintf("https://ex.com/p%d", i),
				ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex",
			})
		}(i)
	}
	wg.Wait()

	if got := st.openedCount(); got != 1 {
		t.Fatalf("concurrent Ingest opened %d incidents for one fingerprint, want exactly 1", got)
	}
}

type fakeDigestSink struct {
	mu  sync.Mutex
	got []notify.Alert
}

func (s *fakeDigestSink) Add(a notify.Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, a)
}
func (s *fakeDigestSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// TestOverCapNonCriticalRoutesToDigest asserts that with a DigestSink configured
// and a small HourlyCap, over-cap non-criticals accrue to the sink (and are NOT
// dispatched immediately), while criticals always dispatch immediately.
func TestOverCapNonCriticalRoutesToDigest(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	sink := &fakeDigestSink{}
	// HourlyCap=1: the first non-critical for a recipient dispatches; the second
	// (distinct identity, same recipient) is over cap -> digest.
	p := newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
		WithDigestSink(sink),
	)

	// First non-critical: under cap, dispatches immediately.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, After: "x"})
	// Second non-critical, different change_type (distinct incident) but same
	// recipient (site+severity) -> over the hourly cap -> digest.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/b",
		ChangeType: "meta_description", Severity: model.SeverityWarning, After: "y"})

	if len(disp.got) != 1 {
		t.Fatalf("expected exactly 1 immediate dispatch (under cap), got %d", len(disp.got))
	}
	if sink.len() != 1 {
		t.Fatalf("expected over-cap non-critical to land in digest sink, got %d", sink.len())
	}
	if sink.got[0].ChangeType != "meta_description" {
		t.Errorf("digest sink got wrong alert: %+v", sink.got[0])
	}

	// A critical always dispatches immediately and never lands in the digest.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/c",
		ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex"})
	if len(disp.got) != 2 {
		t.Fatalf("critical should dispatch immediately; dispatches=%d", len(disp.got))
	}
	if sink.len() != 1 {
		t.Errorf("critical must not land in digest sink; sink len=%d", sink.len())
	}
}

// routingDispatcher is a capturingDispatcher that also resolves a per-alert
// throttle key (the routed notifier name), satisfying the optional throttleKeyer
// seam. It maps every alert to a single fixed channel name regardless of site,
// modeling the default route that funnels all non-criticals into one channel.
type routingDispatcher struct {
	capturingDispatcher
	channel string
}

func (d *routingDispatcher) ThrottleKey(a notify.Alert) (string, bool) {
	return d.channel, true
}

// TestThrottleKeyedByRoutedChannel reproduces F13: when many sites route into one
// notifier/channel, the per_recipient_hourly_cap must bound messages-per-channel,
// not per-site. With the buggy site|severity key, N sites each get an independent
// bucket and can push cap×N messages into the one channel. Keying by the routed
// notifier bounds the channel at the cap, so the SECOND site's alert lands in the
// digest (over the channel cap) rather than dispatching immediately.
func TestThrottleKeyedByRoutedChannel(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &routingDispatcher{channel: "slack-digest"}
	sink := &fakeDigestSink{}
	// Cap of 1 per channel. Two DIFFERENT sites, same severity, both route to the
	// single "slack-digest" channel.
	p := newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
		WithDigestSink(sink),
	)

	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "siteA.com", URL: "https://siteA.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, After: "x"})
	_ = p.Ingest(context.Background(), Event{SiteID: 2, Site: "siteB.com", URL: "https://siteB.com/b",
		ChangeType: "title", Severity: model.SeverityWarning, After: "y"})

	// Per-channel keying: siteA consumes the channel's only slot; siteB is over the
	// channel cap and must route to the digest. The buggy per-site key would give
	// siteB its own fresh bucket, dispatching immediately and leaving the digest empty.
	if len(disp.got) != 1 {
		t.Fatalf("per-channel cap=1: only the first site dispatches immediately, got %d", len(disp.got))
	}
	if sink.len() != 1 {
		t.Fatalf("second site routed to the same channel is over cap and must buffer to digest; sink=%d, want 1", sink.len())
	}
	if sink.got[0].Site != "siteB.com" {
		t.Errorf("digest should hold the over-cap second site; got %q", sink.got[0].Site)
	}
}

// TestOverCapNonCriticalNoDigestSinkDispatches reproduces F14: when NO digest
// sink is configured, an over-cap non-critical must NOT be silently dropped. The
// safest degrade is immediate delivery (losing the cap beats losing the alert).
func TestOverCapNonCriticalNoDigestSinkDispatches(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	// HourlyCap=1, no WithDigestSink.
	p := newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
	)

	// First non-critical: under cap, dispatches.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, After: "x"})
	// Second non-critical, distinct identity, same recipient -> over cap. With no
	// digest sink it MUST still be delivered (degrade to no-throttle), not dropped.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/b",
		ChangeType: "meta_description", Severity: model.SeverityWarning, After: "y"})

	if len(disp.got) != 2 {
		t.Fatalf("over-cap non-critical with no digest sink must dispatch immediately, not drop; dispatches=%d, want 2", len(disp.got))
	}
}

// TestDigestSeveritiesFilter reproduces F15 part 2: an over-cap alert whose
// severity is NOT in the configured digest severities must be dropped from the
// digest (not buffered), while a listed severity is buffered.
func TestDigestSeveritiesFilter(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	sink := &fakeDigestSink{}
	// Only "warning" is digested; "info" over-cap alerts must not buffer.
	p := newPipeline(st, disp,
		WithCaps(Caps{DedupWindow: 5 * time.Minute, HourlyCap: 1, IncidentAutoClose: 24 * time.Hour}),
		WithClock(func() time.Time { return now }),
		WithDigestSink(sink),
		WithDigestSeverities([]model.Severity{model.SeverityWarning}),
	)

	// Recipient site|info: first info dispatches (under cap), second info over cap.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "title", Severity: model.SeverityInfo, After: "x"})
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/b",
		ChangeType: "meta_description", Severity: model.SeverityInfo, After: "y"})
	if sink.len() != 0 {
		t.Fatalf("info over-cap alert must NOT buffer (info not in digest severities); sink=%d", sink.len())
	}

	// Recipient site|warning: first warning dispatches, second over cap -> buffered.
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/c",
		ChangeType: "title", Severity: model.SeverityWarning, After: "x"})
	_ = p.Ingest(context.Background(), Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/d",
		ChangeType: "meta_description", Severity: model.SeverityWarning, After: "y"})
	if sink.len() != 1 {
		t.Fatalf("warning over-cap alert must buffer (warning in digest severities); sink=%d, want 1", sink.len())
	}
}

func TestResolveClosesIncident(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	ev := Event{SiteID: 1, Site: "example.com", URL: "https://example.com/p", ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex"}
	_ = p.Ingest(context.Background(), ev)

	if err := p.Resolve(context.Background(), ev); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(st.closed) != 1 {
		t.Errorf("expected incident closed on resolve, got %d", len(st.closed))
	}
}

// TestIngestRecordsMembership asserts that opening or attaching to a group
// incident records the event's URL as a member of that incident so the close
// path can wait for the LAST member to recover.
func TestIngestRecordsMembership(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	// Two distinct URLs sharing one group fingerprint -> one incident, two members.
	evA := Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex"}
	evB := evA
	evB.URL = "https://ex.com/b"
	if err := p.Ingest(context.Background(), evA); err != nil {
		t.Fatalf("Ingest(A): %v", err)
	}
	if err := p.Ingest(context.Background(), evB); err != nil {
		t.Fatalf("Ingest(B): %v", err)
	}

	if len(st.opened) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(st.opened))
	}
	id := st.opened[0].ID
	if got := []memberCall{{id, "https://ex.com/a"}, {id, "https://ex.com/b"}}; len(st.addedMembers) != 2 ||
		st.addedMembers[0] != got[0] || st.addedMembers[1] != got[1] {
		t.Fatalf("AddAlertMember calls = %+v, want %+v", st.addedMembers, got)
	}
	if n, _ := st.CountAlertMembers(context.Background(), id); n != 2 {
		t.Fatalf("member count = %d, want 2", n)
	}
}

// TestResolveClosesOnlyWhenLastMemberRecovers is the core of Feature B: a group
// incident with two member URLs must stay OPEN when the first URL recovers
// (remaining=1: no CloseIncident, no resolve work) and only CLOSE when the last
// member recovers (remaining=0).
func TestResolveClosesOnlyWhenLastMemberRecovers(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	evA := Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex"}
	evB := evA
	evB.URL = "https://ex.com/b"
	if err := p.Ingest(context.Background(), evA); err != nil {
		t.Fatalf("Ingest(A): %v", err)
	}
	if err := p.Ingest(context.Background(), evB); err != nil {
		t.Fatalf("Ingest(B): %v", err)
	}

	// First URL recovers: incident must stay OPEN (sibling still broken).
	if err := p.Resolve(context.Background(), evA); err != nil {
		t.Fatalf("Resolve(A): %v", err)
	}
	if got := st.closedCount(); got != 0 {
		t.Fatalf("after first member recovery the incident must stay open; closed=%d, want 0", got)
	}

	// Last URL recovers: incident now closes.
	if err := p.Resolve(context.Background(), evB); err != nil {
		t.Fatalf("Resolve(B): %v", err)
	}
	if got := st.closedCount(); got != 1 {
		t.Fatalf("after last member recovery the incident must close; closed=%d, want 1", got)
	}
}

// TestResolveLegacyIncidentNoMembersClosesImmediately covers the fallback: an
// incident with NO alert_members rows (opened before this migration, or a member
// set that was never populated) must still close on the first Resolve so it is
// never stranded open forever.
func TestResolveLegacyIncidentNoMembersClosesImmediately(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	st.suppressMembers = true // model a legacy incident: no member rows.
	disp := &capturingDispatcher{}
	p := newTestPipeline(now, st, disp)

	ev := Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "indexability", Severity: model.SeverityCritical, After: "noindex"}
	if err := p.Ingest(context.Background(), ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := p.Resolve(context.Background(), ev); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := st.closedCount(); got != 1 {
		t.Fatalf("legacy/no-members incident must close on first resolve; closed=%d, want 1", got)
	}
}

// TestHasOpenMember exercises the fire-on-state-change idempotency probe through the
// real pipeline state machine: false before any incident, true once a URL is a member
// of an open incident, false for a different (non-member) URL, and false again after
// the incident closes — so a fixed-cadence evaluator fires only on state change.
func TestHasOpenMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := newFakeIncidentStore()
	p := newTestPipeline(now, st, &capturingDispatcher{})

	ev := Event{SiteID: 1, Site: "ex.com", URL: "https://ex.com/a",
		ChangeType: "index_status_discrepancy", Severity: model.SeverityWarning, After: "x"}

	// Before any incident: not a member.
	if ok, err := p.HasOpenMember(ctx, ev); err != nil || ok {
		t.Fatalf("HasOpenMember before ingest = %v, err=%v; want false", ok, err)
	}
	// After Ingest opens the incident and records the member: true.
	if err := p.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ok, err := p.HasOpenMember(ctx, ev); err != nil || !ok {
		t.Fatalf("HasOpenMember after ingest = %v, err=%v; want true", ok, err)
	}
	// A different URL sharing the identity is not yet a member.
	other := ev
	other.URL = "https://ex.com/b"
	if ok, err := p.HasOpenMember(ctx, other); err != nil || ok {
		t.Fatalf("HasOpenMember for a non-member URL = %v, err=%v; want false", ok, err)
	}
	// An empty-URL event is never a member (site-level).
	siteLevel := ev
	siteLevel.URL = ""
	if ok, err := p.HasOpenMember(ctx, siteLevel); err != nil || ok {
		t.Fatalf("HasOpenMember for empty URL = %v, err=%v; want false", ok, err)
	}
	// After Resolve closes the incident (last member recovers): false again.
	if err := p.Resolve(ctx, ev); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok, err := p.HasOpenMember(ctx, ev); err != nil || ok {
		t.Fatalf("HasOpenMember after resolve = %v, err=%v; want false", ok, err)
	}
}
