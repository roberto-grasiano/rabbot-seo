package mcpsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// daemonVerifyHook reproduces run.go's production daemonVerify closure (begin/check
// over the real store + a fixed instance key) so the e2e test exercises the actual
// single-writer path without importing internal/cli.
func daemonVerifyHook(db *store.DB, key []byte, baseOverride string) func(context.Context, control.VerifyRequest) (control.VerifyResponse, error) {
	return func(ctx context.Context, req control.VerifyRequest) (control.VerifyResponse, error) {
		method := verify.Method(req.Method)
		site, gerr := db.GetSite(ctx, req.SiteID)
		if gerr != nil {
			return control.VerifyResponse{}, gerr
		}
		host := hostOnly(site.BaseURL)
		switch req.Action {
		case "begin":
			res, _ := verify.Begin(req.SiteID, host, method, key)
			return control.VerifyResponse{SiteID: req.SiteID, Method: req.Method, Token: res.Token, State: "throttled", Instructions: res.Instructions, Throttled: true}, nil
		case "check":
			out, cerr := verify.Check(ctx, db, req.SiteID, host, method, verify.Options{Now: time.Now().UTC(), Key: key, AllowPrivate: true, BaseOverride: baseOverride})
			if cerr != nil && out.Record.State == "" {
				return control.VerifyResponse{}, cerr
			}
			return control.VerifyResponse{SiteID: req.SiteID, Method: req.Method, Token: out.Record.Token, State: string(out.Record.State), Reason: string(out.Reason), Throttled: out.Record.State != verify.StateVerified}, nil
		}
		return control.VerifyResponse{}, control.ErrBadRequest
	}
}

func hostOnly(base string) string {
	// strip scheme; the e2e surface is loopback host:port
	const p = "http://"
	if len(base) > len(p) {
		return base[len(p):]
	}
	return base
}

func TestVerifyE2E_BeginNoWrite_CheckFlipsTier(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 0x2a

	dbPath := filepath.Join(t.TempDir(), "k.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// A loopback surface that serves the DERIVED token at the well-known path.
	var siteHost string
	surface := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/rabbot-verify.txt" {
			_, _ = w.Write([]byte(verify.DeriveToken(key, siteHost)))
			return
		}
		http.NotFound(w, r)
	}))
	defer surface.Close()
	siteHost = surface.Listener.Addr().String()

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "http://" + siteHost, Name: "E2E", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// Real control server with the production-shaped verify hook.
	ctrl := control.NewServer(control.ServerOptions{
		Token: "tok", Version: "test",
		Hooks: control.Hooks{Verify: daemonVerifyHook(db, key, surface.URL)},
	})
	cts := httptest.NewServer(ctrl.Handler())
	defer cts.Close()

	client := control.NewClientWithBaseURL(cts.URL, "tok")
	bridge := NewControlBridge(client)
	srv := NewServer(bridge, "test")

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	mc := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := mc.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	// (1) verify_begin writes NOTHING: the stored tier stays throttled afterward.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "verify_begin", Arguments: map[string]any{"site_id": siteID, "method": "well_known"},
	}); err != nil {
		t.Fatalf("verify_begin call: %v", err)
	}
	rec, err := db.GetVerification(ctx, siteID)
	if err != nil {
		t.Fatalf("GetVerification after begin: %v", err)
	}
	if rec.State == verify.StateVerified {
		t.Fatal("verify_begin must not write — site is verified after begin")
	}

	// (2) verify_check flips the LIVING tier to verified in the store (single writer).
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "verify_check", Arguments: map[string]any{"site_id": siteID, "method": "well_known"},
	})
	if err != nil {
		t.Fatalf("verify_check call: %v", err)
	}
	if res.IsError {
		t.Fatalf("verify_check IsError: %+v", res.Content)
	}
	rec2, err := db.GetVerification(ctx, siteID)
	if err != nil {
		t.Fatalf("GetVerification after check: %v", err)
	}
	if rec2.State != verify.StateVerified {
		t.Fatalf("after verify_check the stored tier = %q, want verified (living state)", rec2.State)
	}
}

func TestVerifyE2E_MissStaysThrottled_NoSpoof(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 0x2a

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Surface serves an ATTACKER-CHOSEN token, never the derived one — no spoof.
	surface := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("rab_ATTACKERCHOSENVALUE"))
	}))
	defer surface.Close()
	host := surface.Listener.Addr().String()

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "http://" + host, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	ctrl := control.NewServer(control.ServerOptions{
		Token: "tok", Version: "test",
		Hooks: control.Hooks{Verify: daemonVerifyHook(db, key, surface.URL)},
	})
	cts := httptest.NewServer(ctrl.Handler())
	defer cts.Close()

	bridge := NewControlBridge(control.NewClientWithBaseURL(cts.URL, "tok"))
	srv := NewServer(bridge, "test")
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, _ := srv.Connect(ctx, serverT, nil)
	defer func() { _ = ss.Close() }()
	mc := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, _ := mc.Connect(ctx, clientT, nil)
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "verify_check", Arguments: map[string]any{"site_id": siteID, "method": "well_known"},
	})
	if err != nil {
		t.Fatalf("verify_check call: %v", err)
	}
	// A mismatch is DATA (not a tool error): throttled tier, reason mismatch.
	var got VerifyView
	_ = json.Unmarshal([]byte(textOf(t, res)), &got)
	if got.State == string(verify.StateVerified) {
		t.Fatal("attacker-chosen token verified the site — SPOOF")
	}
	rec, _ := db.GetVerification(ctx, siteID)
	if rec.State == verify.StateVerified {
		t.Fatal("stored tier verified by a non-derived token — SPOOF persisted")
	}
}
