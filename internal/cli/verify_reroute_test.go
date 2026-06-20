package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// daemonUpServer is an httptest control server that reports healthy and handles
// POST /v1/verify by recording the request and returning a verified response.
func daemonUpServer(t *testing.T, captured *control.VerifyRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/verify":
			_ = json.NewDecoder(r.Body).Decode(captured)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(control.VerifyResponse{
				SiteID: captured.SiteID, Method: captured.Method,
				Token: "rab_DAEMON", State: "verified", Throttled: false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunVerify_DaemonUp_RoutesThroughEndpoint(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "k.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://acme.test", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	_ = siteID

	var captured control.VerifyRequest
	ts := daemonUpServer(t, &captured)
	client := control.NewClientWithBaseURL(ts.URL, "tok")

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
		client:       client, // NEW seam: when non-nil and healthy, route here
		allowPrivate: true,
	})
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	// The DB write went through the daemon endpoint, so the endpoint saw the request.
	if captured.Action != "check" {
		t.Fatalf("daemon endpoint action = %q, want check (DB write routed through daemon)", captured.Action)
	}
	if captured.SiteID == 0 {
		t.Fatalf("daemon endpoint never received the verify request: %+v", captured)
	}
}
