package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// daemonVerifyReloadServer is a healthy control server that promotes on
// POST /v1/verify and records every POST /v1/reload. reloadStatus is the HTTP
// status the reload endpoint returns (200 => reload succeeds; >=400 => it fails),
// letting a test exercise both the happy path and the reload-failed fallback.
func daemonVerifyReloadServer(t *testing.T, reloadStatus int, reloadCalls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/verify":
			var req control.VerifyRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(control.VerifyResponse{
				SiteID: req.SiteID, Method: req.Method,
				Token: "rab_DAEMON", State: "verified", Throttled: false,
			})
		case "/v1/reload":
			atomic.AddInt32(reloadCalls, 1)
			w.WriteHeader(reloadStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runDaemonRoutedVerify wires a temp DB+config with one site and runs the
// daemon-routed verify against client, returning the captured stdout.
func runDaemonRoutedVerify(t *testing.T, client *control.Client) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "k.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://acme.test", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	}); err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.AddSiteYAML(cfgPath, config.SiteConfig{URL: "https://acme.test", Name: "Acme"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}

	var buf bytes.Buffer
	err = runVerify(ctx, &buf, verifyDeps{
		db:           db,
		configPath:   cfgPath,
		cfg:          &config.Config{},
		target:       "https://acme.test",
		method:       verify.MethodWellKnown,
		key:          testInstanceKey(),
		now:          time.Now().UTC(),
		client:       client,
		allowPrivate: true,
	})
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	return buf.String()
}

// TestRunVerify_DaemonUp_PromotionTriggersReload is the regression guard for the
// live-rate bug: a successful daemon-routed promotion only writes the proof
// record — the live frontier keeps the unverified throttle until something runs
// reconcileSites + installThrottleFloors. The verify command must trigger the
// existing control reload so the host's rate actually drops live, and only then
// is the "FULL SPEED" copy truthful. Before the fix, /v1/reload was never called
// yet the output still promised FULL SPEED.
func TestRunVerify_DaemonUp_PromotionTriggersReload(t *testing.T) {
	var reloadCalls int32
	srv := daemonVerifyReloadServer(t, http.StatusOK, &reloadCalls)
	client := control.NewClientWithBaseURL(srv.URL, "tok")

	out := runDaemonRoutedVerify(t, client)

	if got := atomic.LoadInt32(&reloadCalls); got != 1 {
		t.Fatalf("daemon-routed promotion called /v1/reload %d times, want 1 "+
			"(the live frontier rate never drops without a reload)", got)
	}
	if !strings.Contains(strings.ToLower(out), "full speed") {
		t.Fatalf("verified output should promise FULL SPEED once the reload applied:\n%s", out)
	}
}

// TestRunVerify_DaemonUp_ReloadFailureKeepsCopyHonest guards the failure leg: if
// the reload does not succeed, the live rate has NOT moved, so the copy must not
// claim an instant FULL SPEED change — it must say the rate applies on the next
// reconcile. The promotion itself (proof record) still succeeds.
func TestRunVerify_DaemonUp_ReloadFailureKeepsCopyHonest(t *testing.T) {
	var reloadCalls int32
	srv := daemonVerifyReloadServer(t, http.StatusInternalServerError, &reloadCalls)
	client := control.NewClientWithBaseURL(srv.URL, "tok")

	out := runDaemonRoutedVerify(t, client)

	if got := atomic.LoadInt32(&reloadCalls); got != 1 {
		t.Fatalf("promotion should still attempt /v1/reload once, got %d", got)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "verified") {
		t.Fatalf("the promotion (proof record) must still be reported as verified:\n%s", out)
	}
	if !strings.Contains(lower, "next reconcile") {
		t.Fatalf("when reload did not apply, the copy must be honest that FULL SPEED "+
			"takes effect on the next reconcile, not instantly:\n%s", out)
	}
}
