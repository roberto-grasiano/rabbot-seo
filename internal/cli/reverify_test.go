package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// testLogger returns a quiet logger for re-verify tests (best-effort log lines
// are emitted at debug; the tests assert state, not log output).
func testLogger() *slog.Logger { return obs.NewLogger(nil, "error") }

// seedSite inserts an enabled site and writes its proof record, returning the id.
func seedSite(t *testing.T, db *store.DB, baseURL string, rec verify.ProofRecord) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := db.AddSite(ctx, model.Site{
		BaseURL: baseURL, Name: baseURL, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("AddSite(%s): %v", baseURL, err)
	}
	rec.SiteID = id
	if err := db.SaveVerification(ctx, id, rec); err != nil {
		t.Fatalf("SaveVerification(%s): %v", baseURL, err)
	}
	return id
}

// TestReverifyLivingState is THE mandated living-state guard. A verified site
// whose token has vanished flips verified->throttled on re-verify; an attested
// site is NEVER re-checked and NEVER auto-promoted (the stub is asserted
// un-called for it).
func TestReverifyLivingState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	aID := seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})
	bID := seedSite(t, db, "https://b.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_b", State: verify.StateAttested,
		LastReverifiedAt: now,
	})

	var mu sync.Mutex
	called := map[int64]bool{}
	key := []byte("test-instance-key")
	stub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		mu.Lock()
		called[req.SiteID] = true
		mu.Unlock()
		if len(opts.Key) == 0 {
			t.Error("re-verify called with no instance key")
		}
		// Token vanished for A: a clean miss yields StateThrottled.
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateThrottled, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonNotFound,
		}, nil
	}

	if _, err := reverifyAll(ctx, db, stub, key, now, testLogger()); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}

	// A: verified -> throttled FLIP (living state).
	if rec, _ := db.GetVerification(ctx, aID); rec.State != verify.StateThrottled {
		t.Errorf("A state = %q, want throttled (verified->throttled flip)", rec.State)
	}
	if !called[aID] {
		t.Errorf("stub must be called for verified site A")
	}
	// B: attested UNCHANGED and never re-checked / never auto-promoted.
	if rec, _ := db.GetVerification(ctx, bID); rec.State != verify.StateAttested {
		t.Errorf("B state = %q, want attested unchanged", rec.State)
	}
	if called[bID] {
		t.Errorf("stub MUST NOT be called for attested site B (terminal, never re-checked)")
	}
}

// TestReverifyStillVerifiedAdvancesReverifiedAt pins that a CLEAN still-verified
// re-verify pass advances LastReverifiedAt to the loop's now WHILE preserving the
// original VerifiedAt (first-verification time). Without persisting on the
// still-verified path the proof record's LastReverifiedAt would forever show the
// original verify timestamp; saving Verify's raw return would instead clobber
// VerifiedAt to now and lose the first-verification time — neither is correct.
func TestReverifyStillVerifiedAdvancesReverifiedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// origVerifiedAt is the first-verification time; later is the loop's clock for
	// this re-verify pass. They are distinct so the advance is observable and the
	// preservation of VerifiedAt is a real assertion (not the same value twice).
	origVerifiedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	aID := seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: origVerifiedAt, LastReverifiedAt: origVerifiedAt,
	})

	// Token still present: Verify returns a FRESH verified record with both
	// VerifiedAt and LastReverifiedAt set to opts.Now (the loop clock) — mirroring
	// the real verify.Verify success path. The loop must NOT save this raw.
	key := []byte("test-instance-key")
	stub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateVerified, VerifiedAt: opts.Now, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonVerified,
		}, nil
	}

	if _, err := reverifyAll(ctx, db, stub, key, later, testLogger()); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}

	rec, gerr := db.GetVerification(ctx, aID)
	if gerr != nil {
		t.Fatalf("GetVerification: %v", gerr)
	}
	if rec.State != verify.StateVerified {
		t.Errorf("A state = %q, want verified (still-verified pass)", rec.State)
	}
	if !rec.LastReverifiedAt.Equal(later) {
		t.Errorf("A LastReverifiedAt = %v, want %v (must advance to loop now)", rec.LastReverifiedAt, later)
	}
	if !rec.VerifiedAt.Equal(origVerifiedAt) {
		t.Errorf("A VerifiedAt = %v, want %v (original first-verification time must be preserved)", rec.VerifiedAt, origVerifiedAt)
	}
	// Method/Token must survive the re-verify write unchanged.
	if rec.Method != verify.MethodWellKnown || rec.Token != "rab_a" {
		t.Errorf("A method/token = %q/%q, want well_known/rab_a (unchanged)", rec.Method, rec.Token)
	}
}

// TestReverifyTransientErrorDoesNotDemote pins that an inconclusive transport/DNS
// error does NOT flip a verified site to throttled (no flapping on a blip).
func TestReverifyTransientErrorDoesNotDemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	aID := seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})

	key := []byte("test-instance-key")
	stub := func(_ context.Context, _ verify.Request, _ verify.Options) (verify.Outcome, error) {
		return verify.Outcome{Reason: verify.ReasonUnreachable}, errors.New("dial tcp: timeout")
	}

	if _, err := reverifyAll(ctx, db, stub, key, now, testLogger()); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}
	if rec, _ := db.GetVerification(ctx, aID); rec.State != verify.StateVerified {
		t.Errorf("A state = %q, want verified (transient error must NOT demote)", rec.State)
	}
}

// TestReverifyDemotesVerifiedWithoutMethodToken pins the demote-on-doubt contract
// (spec D5): a StateVerified record that has lost its method/token is
// un-recheckable, so leaving it verified would run it at full speed forever.
// Re-verify must DEMOTE it to throttled. An attested record stays terminal
// (untouched, never re-checked) even alongside the demotion.
func TestReverifyDemotesVerifiedWithoutMethodToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	// A verified record with NO method/token: un-recheckable, must demote.
	aID := seedSite(t, db, "https://a.example", verify.ProofRecord{
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	})
	// An attested record: terminal, must NOT be touched.
	bID := seedSite(t, db, "https://b.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_b", State: verify.StateAttested,
		LastReverifiedAt: now,
	})

	key := []byte("test-instance-key")
	var calls int
	stub := func(_ context.Context, _ verify.Request, _ verify.Options) (verify.Outcome, error) {
		calls++
		return verify.Outcome{}, nil
	}
	if _, err := reverifyAll(ctx, db, stub, key, now, testLogger()); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}
	// A: verified-but-empty -> throttled (demote-on-doubt), verifier never invoked.
	if rec, _ := db.GetVerification(ctx, aID); rec.State != verify.StateThrottled {
		t.Errorf("A state = %q, want throttled (un-recheckable verified must demote)", rec.State)
	}
	if calls != 0 {
		t.Errorf("stub called %d times, want 0 (no method/token means nothing to re-check)", calls)
	}
	// B: attested unchanged, terminal.
	if rec, _ := db.GetVerification(ctx, bID); rec.State != verify.StateAttested {
		t.Errorf("B state = %q, want attested unchanged (terminal)", rec.State)
	}
}

// TestReverifyBestEffortOnSaveError pins that a SaveVerification failure for one
// site does NOT abort the whole pass: every other per-site op in the daemon's
// side-timer loops is best-effort (log + continue), and re-verify must match. The
// first site's persist fails, but a later verified site is still re-checked and
// its verified->throttled demotion is still persisted.
func TestReverifyBestEffortOnSaveError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	aID := seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})
	bID := seedSite(t, db, "https://b.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_b", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})

	// Inject a store whose SaveVerification errors for site A only.
	failing := &saveFailingStore{DB: db, failSiteID: aID}

	var mu sync.Mutex
	called := map[int64]bool{}
	key := []byte("test-instance-key")
	stub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		mu.Lock()
		called[req.SiteID] = true
		mu.Unlock()
		// Token vanished for both: a clean miss yields StateThrottled.
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateThrottled, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonNotFound,
		}, nil
	}

	if _, err := reverifyAll(ctx, failing, stub, key, now, testLogger()); err != nil {
		t.Fatalf("reverifyAll returned error, want nil (best-effort): %v", err)
	}

	// A's save failed and was swallowed: it is still verified (no demotion landed).
	if rec, _ := db.GetVerification(ctx, aID); rec.State != verify.StateVerified {
		t.Errorf("A state = %q, want verified (its save failed and was skipped)", rec.State)
	}
	// B is processed anyway: re-checked AND its demotion persisted.
	if !called[bID] {
		t.Errorf("B must still be re-checked after A's save failure (best-effort, not abort)")
	}
	if rec, _ := db.GetVerification(ctx, bID); rec.State != verify.StateThrottled {
		t.Errorf("B state = %q, want throttled (demotion must land despite A's save failure)", rec.State)
	}
}

// saveFailingStore wraps a real *store.DB but forces SaveVerification to error
// for a single site id, so the best-effort pass can be exercised without a live
// DB fault. ListSites/GetVerification delegate to the real store.
type saveFailingStore struct {
	*store.DB
	failSiteID int64
}

func (s *saveFailingStore) SaveVerification(ctx context.Context, siteID int64, rec verify.ProofRecord) error {
	if siteID == s.failSiteID {
		return errors.New("injected SaveVerification failure")
	}
	return s.DB.SaveVerification(ctx, siteID, rec)
}

// TestPeriodicReverifyDemotionWidensURLCadence pins that the PERIODIC re-verify
// path widens a demoted site's per-URL scheduling cadence to the throttled tier,
// not just the HTTP per-host floor. A verified site is seeded (via reconcileSites)
// at the verified cadence (10m => urls.interval 600s); its token then vanishes, so
// a periodic re-verify demotes it. reverifyAll alone only rewrites the proof record
// — it never touches SetSiteThrottle or urls.interval — so without the post-reverify
// reconcile the URL would stay due every ~10m instead of the throttled ~30m, burning
// scheduler slots until a manual reload. reconcileAfterReverify (the periodic
// sequence) must widen both sites.min_interval AND the seeded urls.interval to the
// throttled MinInterval (>= 30m), mirroring the startup path.
func TestPeriodicReverifyDemotionWidensURLCadence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "test-agent",
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	front := frontier.New(frontier.Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 2})
	logger := testLogger()

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	// Seed the site, then flip its proof to verified and reconcile so its base URL
	// is seeded at the VERIFIED cadence (10m => 600s), exactly as a verified site
	// would have been seeded while its token was present.
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (initial): %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	if err := db.SaveVerification(ctx, site.ID, verify.ProofRecord{
		SiteID: site.ID, Method: verify.MethodWellKnown, Token: "rab_x",
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerification (verified): %v", err)
	}
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (verified): %v", err)
	}

	verifiedMin := int64(cfg.MinIntervalDuration().Seconds()) // 600 (10m)
	baseURL, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (verified): %v", err)
	}
	if baseURL.Interval != verifiedMin {
		t.Fatalf("precondition: verified urls.interval = %ds, want %ds", baseURL.Interval, verifiedMin)
	}

	// Periodic re-verify: the token has vanished, so the stub returns a clean
	// StateThrottled (verified->throttled demotion). reverifyAll persists ONLY the
	// proof record.
	key := []byte("test-instance-key")
	demoteStub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateThrottled, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonNotFound,
		}, nil
	}
	if _, err := reverifyAll(ctx, db, demoteStub, key, time.Now().UTC(), logger); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}
	// Sanity: the proof did demote.
	if rec, _ := db.GetVerification(ctx, site.ID); rec.State != verify.StateThrottled {
		t.Fatalf("proof state = %q, want throttled (demotion must land)", rec.State)
	}

	// The fix under test: the periodic post-reverify sequence must widen the
	// demoted site's URL scheduling cadence to the throttled tier.
	reconcileAfterReverify(ctx, db, &cfg, "0.0.1", fetch, nil, front, logger, nil)

	throttledMin := int64((30 * time.Minute).Seconds()) // 1800 (throttle floor)

	// sites.min_interval widened to the throttled tier.
	siteAfter, err := db.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite (after): %v", err)
	}
	if siteAfter.MinInterval < throttledMin {
		t.Errorf("sites.min_interval = %ds, want >= %ds (throttled tier) after periodic demotion", siteAfter.MinInterval, throttledMin)
	}

	// The per-URL scheduling cadence (the actual bug) widened to the throttled tier:
	// without the fix it would still read the verified 600s.
	urlAfter, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (after): %v", err)
	}
	if urlAfter.Interval < throttledMin {
		t.Errorf("urls.interval = %ds, want >= %ds (throttled cadence) after periodic demotion; "+
			"still at verified cadence means the scheduler keeps the demoted site due too often", urlAfter.Interval, throttledMin)
	}
}

// TestStartupReverifyDemotionWidensURLCadence pins that the STARTUP re-verify
// path widens a demoted site's per-URL scheduling cadence to the throttled tier
// — the same guarantee the periodic path makes (see
// TestPeriodicReverifyDemotionWidensURLCadence). It drives the exact startup
// sequence from runDaemon: reconcileSites (seeds the verified site at the
// verified cadence) -> reverifyAll (the token has vanished, so the site demotes
// verified->throttled, writing ONLY the proof record) -> reconcileAfterReverify
// (re-seeds urls.interval through the verification-aware resolver, now reading
// the freshly-demoted StateThrottled proof, AND re-installs the HTTP floor).
//
// Before the fix the startup path called a bare installThrottleFloors after the
// reverify, which installs the per-host HTTP floor but does NOT re-seed
// urls.interval — so a startup demotion kept the verified (faster) cadence until
// the first periodic reconcileAfterReverify (~1h). This test would fail on that
// old sequence because urls.interval would still read the verified 600s.
func TestStartupReverifyDemotionWidensURLCadence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "test-agent",
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	front := frontier.New(frontier.Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 2})
	logger := testLogger()

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	// Simulate the pre-shutdown state: the site was verified, so its base URL was
	// seeded at the VERIFIED cadence (10m => 600s). reconcileSites seeds the row;
	// we then flip the proof to verified and reconcile again so urls.interval lands
	// at the verified tier, exactly as it would have while the token was present.
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (initial): %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	if err := db.SaveVerification(ctx, site.ID, verify.ProofRecord{
		SiteID: site.ID, Method: verify.MethodWellKnown, Token: "rab_x",
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerification (verified): %v", err)
	}
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (verified): %v", err)
	}

	verifiedMin := int64(cfg.MinIntervalDuration().Seconds()) // 600 (10m)
	baseURL, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (verified): %v", err)
	}
	if baseURL.Interval != verifiedMin {
		t.Fatalf("precondition: verified urls.interval = %ds, want %ds", baseURL.Interval, verifiedMin)
	}

	// ── The STARTUP sequence under test ──────────────────────────────────────
	// startup reverify: the token vanished while the daemon was down, so the stub
	// returns a clean StateThrottled (verified->throttled demotion). reverifyAll
	// persists ONLY the proof record (no SetSiteThrottle / urls.interval write).
	key := []byte("test-instance-key")
	demoteStub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateThrottled, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonNotFound,
		}, nil
	}
	if _, err := reverifyAll(ctx, db, demoteStub, key, time.Now().UTC(), logger); err != nil {
		t.Fatalf("startup reverifyAll: %v", err)
	}
	if rec, _ := db.GetVerification(ctx, site.ID); rec.State != verify.StateThrottled {
		t.Fatalf("proof state = %q, want throttled (startup demotion must land)", rec.State)
	}

	// The fix: the startup path must now mirror the periodic path — call
	// reconcileAfterReverify (not a bare installThrottleFloors) so the demoted
	// site's URL cadence widens to the throttled tier on the same startup pass.
	reconcileAfterReverify(ctx, db, &cfg, "0.0.1", fetch, nil, front, logger, nil)

	throttledMin := int64((30 * time.Minute).Seconds()) // 1800 (throttle floor)

	// sites.min_interval widened to the throttled tier.
	siteAfter, err := db.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite (after): %v", err)
	}
	if siteAfter.MinInterval < throttledMin {
		t.Errorf("sites.min_interval = %ds, want >= %ds (throttled tier) after startup demotion", siteAfter.MinInterval, throttledMin)
	}

	// The per-URL scheduling cadence (the actual bug) widened to the throttled tier:
	// on the OLD startup sequence (bare installThrottleFloors) it would still read 600s.
	urlAfter, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (after): %v", err)
	}
	if urlAfter.Interval < throttledMin {
		t.Errorf("urls.interval = %ds, want >= %ds (throttled cadence) after startup demotion; "+
			"still at verified cadence means the scheduler keeps the demoted site due too often", urlAfter.Interval, throttledMin)
	}
}

// TestReverifyReportsDemotionCount pins the PR31 #2 contract: reverifyAll REPORTS
// how many sites it actually DEMOTED this pass, so the daemon can call the
// destructive reconcileAfterReverify only when something changed. A clean
// token-loss flip (verified->throttled) and an un-recheckable verified record
// (no method/token) each count as one demotion; a still-verified site, an
// attested record, and a never-attempted throttled record contribute zero.
func TestReverifyReportsDemotionCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	// A: verified with token -> the stub returns clean throttled => 1 demotion.
	seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})
	// B: verified with NO method/token -> demote-on-doubt => 1 demotion.
	seedSite(t, db, "https://b.example", verify.ProofRecord{
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	})
	// C: verified with token -> the stub keeps it verified => 0 demotions.
	seedSite(t, db, "https://c.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_c", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})
	// D: attested (terminal) => never re-checked => 0.
	seedSite(t, db, "https://d.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_d", State: verify.StateAttested,
		LastReverifiedAt: now,
	})
	// E: never-attempted throttled => skipped => 0.
	seedSite(t, db, "https://e.example", verify.ProofRecord{State: verify.StateThrottled})

	// The stub demotes A (token vanished) but keeps C verified, keyed by host.
	key := []byte("test-instance-key")
	stub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		st := verify.StateThrottled
		reason := verify.ReasonNotFound
		if req.Host == "c.example" {
			st = verify.StateVerified
			reason = verify.ReasonVerified
		}
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: st, VerifiedAt: opts.Now, LastReverifiedAt: opts.Now,
			},
			Reason: reason,
		}, nil
	}

	demoted, rerr := reverifyAll(ctx, db, stub, key, now, testLogger())
	if rerr != nil {
		t.Fatalf("reverifyAll: %v", rerr)
	}
	if demoted != 2 {
		t.Errorf("demoted = %d, want 2 (A clean token-loss + B un-recheckable)", demoted)
	}
}

// TestReverifyNoDemotionReportsZero pins that a pass in which NOTHING demotes
// reports zero (the gate for skipping the destructive periodic reconcile, PR31
// #2). Every site stays verified, so the count must be 0 even though writes
// (LastReverifiedAt advances) still happen.
func TestReverifyNoDemotionReportsZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	seedSite(t, db, "https://a.example", verify.ProofRecord{
		Method: verify.MethodWellKnown, Token: "rab_a", State: verify.StateVerified,
		VerifiedAt: now, LastReverifiedAt: now,
	})
	key := []byte("test-instance-key")
	stub := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateVerified, VerifiedAt: opts.Now, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonVerified,
		}, nil
	}
	demoted, rerr := reverifyAll(ctx, db, stub, key, now, testLogger())
	if rerr != nil {
		t.Fatalf("reverifyAll: %v", rerr)
	}
	if demoted != 0 {
		t.Errorf("demoted = %d, want 0 (no site demoted)", demoted)
	}
}

// runPeriodicReverifyPass mirrors the daemon's periodic re-verify gate (run.go):
// re-verify the fleet, and reconcile ONLY when a site actually demoted (PR31 #2).
// The test drives it directly so the gate's effect on the adaptive schedule is
// asserted without spinning the whole side-timer goroutine.
func runPeriodicReverifyPass(t *testing.T, ctx context.Context, db *store.DB, cfg *config.Config, vf reverifyFn, key []byte, f fetcher.Fetcher, front *frontier.Frontier, logger *slog.Logger) {
	t.Helper()
	demoted, err := reverifyAll(ctx, db, vf, key, time.Now().UTC(), logger)
	if err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}
	if demoted > 0 {
		reconcileAfterReverify(ctx, db, cfg, "0.0.1", f, nil, front, logger, nil)
	}
}

// TestPeriodicReverifyNoDemotionLeavesScheduleUntouched pins PR31 #2: a periodic
// re-verify pass in which NOTHING demotes must NOT run the destructive
// reconcileAfterReverify, so a homepage's adaptively-grown next_check_at and
// interval survive the hourly pass. Before the fix reconcileAfterReverify ran
// unconditionally and re-seeded the base URL due-now at minInterval, resetting the
// grown schedule every ~1h even when no verification state changed.
func TestPeriodicReverifyNoDemotionLeavesScheduleUntouched(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent: "test-agent", Timeout: 10 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true,
	})
	front := frontier.New(frontier.Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 2})
	logger := testLogger()

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (initial): %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	// Verify the site so a re-verify pass keeps it verified (no demotion).
	if err := db.SaveVerification(ctx, site.ID, verify.ProofRecord{
		SiteID: site.ID, Method: verify.MethodWellKnown, Token: "rab_x",
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerification: %v", err)
	}

	// Simulate an adaptively-grown schedule: a wider interval and a FUTURE
	// next_check_at, exactly what a stable crawl writes via UpdateURLSchedule.
	base, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	grownInterval := int64(3600)                       // 1h, wider than the seeded 600s
	grownNext := now.Add(45 * time.Minute).Truncate(0) // a future due time
	if err := db.UpdateURLSchedule(ctx, base.ID, grownNext, grownInterval, model.FetchOK, "", ""); err != nil {
		t.Fatalf("UpdateURLSchedule: %v", err)
	}

	// A periodic pass where the token is STILL present => no demotion => gate skips
	// reconcile. The stub keeps the site verified.
	key := []byte("test-instance-key")
	keepVerified := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateVerified, VerifiedAt: opts.Now, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonVerified,
		}, nil
	}
	runPeriodicReverifyPass(t, ctx, db, &cfg, keepVerified, key, fetch, front, logger)

	after, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (after): %v", err)
	}
	if after.Interval != grownInterval {
		t.Errorf("urls.interval = %ds, want %ds (a no-demotion pass must NOT reset the grown interval)", after.Interval, grownInterval)
	}
	if !after.NextCheckAt.Equal(grownNext) {
		t.Errorf("urls.next_check_at = %v, want %v (a no-demotion pass must NOT reset the schedule to due-now)", after.NextCheckAt, grownNext)
	}
}

// TestPeriodicReverifyDemotionDoesReconcile pins the other half of PR31 #2: when a
// site DOES demote on a periodic pass, the gate runs reconcileAfterReverify so the
// demoted site's schedule widens to the throttled tier (the schedule reset is
// intentional and correct here — the demotion changed its tier).
func TestPeriodicReverifyDemotionDoesReconcile(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent: "test-agent", Timeout: 10 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true,
	})
	front := frontier.New(frontier.Options{PerHostRate: 2 * time.Second, PerHostConcurrency: 2})
	logger := testLogger()

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (initial): %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	if err := db.SaveVerification(ctx, site.ID, verify.ProofRecord{
		SiteID: site.ID, Method: verify.MethodWellKnown, Token: "rab_x",
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerification: %v", err)
	}
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (verified): %v", err)
	}

	// The token vanished => the pass demotes => the gate reconciles.
	key := []byte("test-instance-key")
	demote := func(_ context.Context, req verify.Request, opts verify.Options) (verify.Outcome, error) {
		return verify.Outcome{
			Record: verify.ProofRecord{
				SiteID: req.SiteID, Method: req.Method,
				State: verify.StateThrottled, LastReverifiedAt: opts.Now,
			},
			Reason: verify.ReasonNotFound,
		}, nil
	}
	runPeriodicReverifyPass(t, ctx, db, &cfg, demote, key, fetch, front, logger)

	throttledMin := int64((30 * time.Minute).Seconds())
	after, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL (after): %v", err)
	}
	if after.Interval < throttledMin {
		t.Errorf("urls.interval = %ds, want >= %ds (a demotion pass MUST reconcile to the throttled cadence)", after.Interval, throttledMin)
	}
}

// TestReverifyNeverAttemptedSkipped pins that a throttled site with an empty
// method/token is skipped entirely (the stub is never called) — there is nothing
// to re-check, and a throttled site is lifted only by an explicit verify.
func TestReverifyNeverAttemptedSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	// A bare throttled record: empty method/token (never attempted).
	cID := seedSite(t, db, "https://c.example", verify.ProofRecord{State: verify.StateThrottled})

	key := []byte("test-instance-key")
	var calls int
	stub := func(_ context.Context, _ verify.Request, _ verify.Options) (verify.Outcome, error) {
		calls++
		return verify.Outcome{}, nil
	}
	if _, err := reverifyAll(ctx, db, stub, key, now, testLogger()); err != nil {
		t.Fatalf("reverifyAll: %v", err)
	}
	if calls != 0 {
		t.Errorf("stub called %d times, want 0 (never-attempted throttled site is skipped)", calls)
	}
	if rec, _ := db.GetVerification(ctx, cID); rec.State != verify.StateThrottled {
		t.Errorf("C state = %q, want throttled unchanged", rec.State)
	}
}
