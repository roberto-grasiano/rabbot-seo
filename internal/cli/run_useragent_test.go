package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestPerHostUserAgentFuncTrustSignal pins the daemon's per-host User-Agent
// closure: it resolves host -> site -> live verification state and threads it
// through cfg.UserAgentFor so a VERIFIED host's UA reads "verified for <site>"
// while an UNVERIFIED, non-matching host's UA reads "unverified — confirm or
// block". The email domain (example.com) matches neither site, so the only thing
// that flips the signal is the per-site verification state — exactly the daemon
// fetch-time resolution the spec requires.
func TestPerHostUserAgentFuncTrustSignal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ua.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{
		{URL: "https://verified.test"},
		{URL: "https://unverified.test"},
	}

	logger := obs.NewLogger(nil, "error")
	fetch := fetcher.New(fetcher.Options{AllowPrivate: true})
	if rerr := reconcileSites(ctx, db, &cfg, "9.9.9", fetch, nil, time.Now().UTC(), logger, nil); rerr != nil {
		t.Fatalf("reconcileSites: %v", rerr)
	}
	// Promote only verified.test to StateVerified in the proof record.
	vs, gerr := db.GetSiteByBaseURL(ctx, "https://verified.test")
	if gerr != nil {
		t.Fatalf("GetSiteByBaseURL: %v", gerr)
	}
	seedVerified(t, ctx, db, vs.ID)

	var cfgMu sync.Mutex
	snap := newVerifiedSnapshot()
	snap.refresh(ctx, db)
	uaFunc := perHostUserAgentFunc(&cfgMu, &cfg, snap, "9.9.9")

	gotVerified := uaFunc("verified.test")
	gotUnverified := uaFunc("unverified.test")

	if gotVerified == gotUnverified {
		t.Fatalf("UA must differ by host trust state; both = %q", gotVerified)
	}
	if !strings.Contains(gotVerified, "verified for verified.test") {
		t.Errorf("verified host UA = %q, want it to contain %q", gotVerified, "verified for verified.test")
	}
	if !strings.Contains(gotUnverified, "unverified — confirm or block") {
		t.Errorf("unverified host UA = %q, want it to contain %q", gotUnverified, "unverified — confirm or block")
	}
	// An unknown host (not in the site list) is unverified and non-matching.
	if got := uaFunc("stranger.test"); !strings.Contains(got, "unverified — confirm or block") {
		t.Errorf("unknown host UA = %q, want the confirm-or-block form", got)
	}
}

// TestRunDaemonWarnsOnInvalidContactEmail pins finding #5: the daemon logs a non-fatal
// WARN at startup when crawler.contact_email is empty/invalid (config.Defaults leaves it
// empty), but STILL starts and shuts down cleanly — an unreachable monitor beats no
// monitor, so the empty email never aborts startup.
func TestRunDaemonWarnsOnInvalidContactEmail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:   "", // no file => config.Defaults() (empty contact_email)
			DataDir:      t.TempDir(),
			ControlToken: "tok",
			ControlPort:  0, // skip the control listener
			Version:      "0.0.1",
			LogLevel:     "warn", // ensure the WARN line is emitted
			TickInterval: 5 * time.Millisecond,
		})
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon must NOT fail on an invalid contact_email, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not exit within 2s of cancel")
	}
	if !strings.Contains(out.String(), "contact_email") {
		t.Errorf("expected a contact_email WARN line, got: %q", out.String())
	}
}

// TestVerifiedSnapshotReflectsState pins the hot-path snapshot (findings #1/#2/#6/#12):
// the per-host UA closure must read an O(1) host->verified map (no per-fetch
// db.ListSites/GetVerification scan). The snapshot reflects a verified vs unverified
// host, an unknown host resolves false (fail-safe), a host:port and a case-variant host
// resolve port-/case-insensitively, and a re-verify+refresh flips the cached state.
func TestVerifiedSnapshotReflectsState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "snap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{
		{URL: "https://verified.test"},
		{URL: "https://unverified.test"},
	}

	logger := obs.NewLogger(nil, "error")
	fetch := fetcher.New(fetcher.Options{AllowPrivate: true})
	if rerr := reconcileSites(ctx, db, &cfg, "9.9.9", fetch, nil, time.Now().UTC(), logger, nil); rerr != nil {
		t.Fatalf("reconcileSites: %v", rerr)
	}
	vs, gerr := db.GetSiteByBaseURL(ctx, "https://verified.test")
	if gerr != nil {
		t.Fatalf("GetSiteByBaseURL: %v", gerr)
	}

	snap := newVerifiedSnapshot()
	snap.refresh(ctx, db) // BEFORE any verification: nothing verified yet.
	if snap.verified("verified.test") {
		t.Error("pre-verify snapshot must not report verified.test as verified")
	}

	// Promote verified.test and refresh: the snapshot must now reflect it.
	seedVerified(t, ctx, db, vs.ID)
	snap.refresh(ctx, db)
	if !snap.verified("verified.test") {
		t.Error("after re-verify+refresh, verified.test must read verified")
	}
	if snap.verified("unverified.test") {
		t.Error("unverified.test must read unverified")
	}
	// Unknown host -> false (fail-safe to unverified).
	if snap.verified("stranger.test") {
		t.Error("unknown host must read unverified (fail-safe)")
	}
	// Port- and case-insensitive resolution of the verified host.
	if !snap.verified("verified.test:8080") {
		t.Error("host:port must resolve to the same verified state (port-insensitive)")
	}
	if !snap.verified("VERIFIED.test") {
		t.Error("case-variant host must resolve to the same verified state")
	}
}

// TestPerHostUserAgentFuncUsesSnapshot pins that the daemon's per-host UA closure reads
// the injected snapshot (not a per-fetch DB scan): flipping the snapshot's cached state
// flips the emitted UA trust signal, with NO further DB read on the fetch path.
func TestPerHostUserAgentFuncUsesSnapshot(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: "https://acme.test"}}

	var cfgMu sync.Mutex
	snap := newVerifiedSnapshot()
	uaFunc := perHostUserAgentFunc(&cfgMu, &cfg, snap, "9.9.9")

	// Snapshot empty -> unverified, non-matching email -> confirm-or-block.
	if got := uaFunc("acme.test"); !strings.Contains(got, "unverified — confirm or block") {
		t.Errorf("empty-snapshot UA = %q, want confirm-or-block", got)
	}
	// Mark verified in the snapshot -> UA flips to "verified for".
	snap.set(map[string]bool{"acme.test": true})
	if got := uaFunc("acme.test"); !strings.Contains(got, "verified for acme.test") {
		t.Errorf("verified-snapshot UA = %q, want 'verified for acme.test'", got)
	}
}

// TestPerHostUserAgentReachesTheWire pins the FULL thread: the daemon's per-host
// UA closure, wired into a real fetcher.New(UserAgentFunc: ...), sets the actual
// User-Agent HTTP header on the request — and that header reflects the target
// host's live verification state. A verified site's fetch carries "verified for
// <host>"; an unverified site's carries "unverified — confirm or block". The
// per-host match is port-insensitive, so a loopback httptest server (host
// 127.0.0.1) registered by its full base URL (127.0.0.1:PORT) still resolves.
func TestPerHostUserAgentReachesTheWire(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, verified bool, wantSubstr string) {
		t.Helper()
		ctx := context.Background()

		var gotUA string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUA = r.Header.Get("User-Agent")
			_, _ = w.Write([]byte("<html><title>ok</title></html>"))
		}))
		defer srv.Close()

		db, err := store.Open(ctx, filepath.Join(t.TempDir(), "wire.db"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		cfg := config.Defaults()
		cfg.Crawler.ContactEmail = "ops@example.com"
		cfg.Sites = []config.SiteConfig{{URL: srv.URL}}

		logger := obs.NewLogger(nil, "error")
		seed := fetcher.New(fetcher.Options{AllowPrivate: true})
		if rerr := reconcileSites(ctx, db, &cfg, "1.2.3", seed, nil, time.Now().UTC(), logger, nil); rerr != nil {
			t.Fatalf("reconcileSites: %v", rerr)
		}
		if verified {
			s, gerr := db.GetSiteByBaseURL(ctx, srv.URL)
			if gerr != nil {
				t.Fatalf("GetSiteByBaseURL: %v", gerr)
			}
			seedVerified(t, ctx, db, s.ID)
		}

		var cfgMu sync.Mutex
		snap := newVerifiedSnapshot()
		snap.refresh(ctx, db)
		uaFunc := perHostUserAgentFunc(&cfgMu, &cfg, snap, "1.2.3")
		f := fetcher.New(fetcher.Options{UserAgentFunc: uaFunc, AllowPrivate: true, Timeout: 5 * time.Second})
		if _, ferr := f.Fetch(ctx, fetcher.Request{URL: srv.URL}); ferr != nil {
			t.Fatalf("Fetch: %v", ferr)
		}
		if !strings.Contains(gotUA, wantSubstr) {
			t.Errorf("wire UA = %q, want it to contain %q", gotUA, wantSubstr)
		}
		if !strings.HasPrefix(gotUA, "Rabbot-SEO/1.2.3 (+mailto:ops@example.com") {
			t.Errorf("wire UA = %q, want the Rabbot-SEO/<v> (+mailto:…) prefix", gotUA)
		}
	}

	t.Run("verified host", func(t *testing.T) {
		t.Parallel()
		// 127.0.0.1 is an IP literal with no registrable domain (eTLD+1), so the
		// siteDomain renders as the cautious fallback "the site" — the verification
		// state (the load-bearing signal here) still flips the UA to "verified for".
		run(t, true, "verified for the site")
	})
	t.Run("unverified non-matching host", func(t *testing.T) {
		t.Parallel()
		run(t, false, "unverified — confirm or block")
	})
}
