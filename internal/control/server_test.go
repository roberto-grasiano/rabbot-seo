package control

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

func newTestServer(hooks Hooks) *httptest.Server {
	srv := NewServer(ServerOptions{Token: "tok", Version: "0.1.0", Hooks: hooks})
	return httptest.NewServer(srv.Handler())
}

func TestHealthRequiresToken(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token", "Bearer tok", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/health", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

func TestReloadInvokesHook(t *testing.T) {
	called := false
	ts := newTestServer(Hooks{Reload: func() error { called = true; return nil }})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/reload", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Error("reload hook was not invoked")
	}
}

func TestStatusHookReturnsPayload(t *testing.T) {
	ts := newTestServer(Hooks{
		Status: func(ctx context.Context) (StatusResponse, error) {
			return StatusResponse{Version: "0.1.0", SiteCount: 2}, nil
		},
	})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"site_count":2`) {
		t.Errorf("status body %q missing site_count:2", body)
	}
}

func TestNilHookReturns501(t *testing.T) {
	// All §4 mutation routes must exist and return 501 when their hook is nil.
	ts := newTestServer(Hooks{}) // no hooks wired
	t.Cleanup(ts.Close)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"status", http.MethodGet, "/v1/status", ""},
		{"add site", http.MethodPost, "/v1/sites", `{"url":"https://x.example"}`},
		{"remove site", http.MethodDelete, "/v1/sites/1", ""},
		{"crawl", http.MethodPost, "/v1/crawl", `{"target":"https://x.example"}`},
		{"ignore issue", http.MethodPost, "/v1/issues/1/ignore", ""},
		{"pause", http.MethodPost, "/v1/pause", ""},
		{"resume", http.MethodPost, "/v1/resume", ""},
		{"notify test", http.MethodPost, "/v1/notify/test", `{"notifier":"slack"}`},
		{"config set", http.MethodPost, "/v1/config", `{"key":"log.level","value":"debug"}`},
		{"verify", http.MethodPost, "/v1/verify", `{"site_id":1,"method":"meta","action":"begin"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rdr io.Reader
			if tc.body != "" {
				rdr = strings.NewReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, rdr)
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("%s %s status = %d, want 501", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

// newTestServerWithShutdown wires a control server (via Handler) whose Shutdown
// option signals the returned channel. Shutdown is invoked asynchronously by the
// handler, so tests must select on the channel rather than read a plain bool.
func newTestServerWithShutdown(t *testing.T) (*httptest.Server, *atomic.Int32, chan struct{}) {
	t.Helper()
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	srv := NewServer(ServerOptions{
		Token:   "tok",
		Version: "0.1.0",
		Shutdown: func() {
			calls.Add(1)
			done <- struct{}{}
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, &calls, done
}

func TestShutdownRequiresToken(t *testing.T) {
	ts, calls, done := newTestServerWithShutdown(t)

	// No/invalid token => 401 and the Shutdown callback must NOT fire.
	for _, hdr := range []string{"", "Bearer nope"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/shutdown", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth=%q status = %d, want 401", hdr, resp.StatusCode)
		}
	}

	// Give any (incorrectly-fired) async callback a moment to run before asserting.
	select {
	case <-done:
		t.Fatal("Shutdown callback fired on an unauthorized request")
	case <-time.After(100 * time.Millisecond):
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("Shutdown calls on unauthorized request = %d, want 0", got)
	}
}

func TestShutdownInvokesCallbackOnce(t *testing.T) {
	ts, calls, done := newTestServerWithShutdown(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}

	// The handler writes the response FIRST, then triggers Shutdown asynchronously.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown callback was not invoked")
	}
	// It must fire exactly once; wait briefly to ensure no duplicate.
	select {
	case <-done:
		t.Fatal("Shutdown callback fired more than once")
	case <-time.After(100 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Shutdown calls = %d, want 1", got)
	}
}

func TestShutdownNilHookReturns501(t *testing.T) {
	// A nil Shutdown hook must return 501 like every other unwired route.
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestPauseResumeInvokeHook(t *testing.T) {
	var lastPaused bool
	calls := 0
	ts := newTestServer(Hooks{
		Pause: func(ctx context.Context, paused bool) error { calls++; lastPaused = paused; return nil },
	})
	t.Cleanup(ts.Close)

	do := func(path string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if code := do("/v1/pause"); code != http.StatusOK {
		t.Errorf("pause status = %d, want 200", code)
	}
	if !lastPaused {
		t.Error("pause should call hook with paused=true")
	}
	if code := do("/v1/resume"); code != http.StatusOK {
		t.Errorf("resume status = %d, want 200", code)
	}
	if lastPaused {
		t.Error("resume should call hook with paused=false")
	}
	if calls != 2 {
		t.Errorf("pause hook calls = %d, want 2", calls)
	}
}

func TestConfigSetGuardRejectsDeniedKeyNoWrite(t *testing.T) {
	// The production SetConfig hook calls config.AllowConfigKey before writing.
	// This test wires a hook with that exact shape and asserts: a denied key
	// produces a non-2xx response AND the "write" branch is never reached.
	wrote := false
	ts := newTestServer(Hooks{
		SetConfig: func(_ context.Context, req ConfigSetRequest) error {
			if err := config.AllowConfigKey(req.Key); err != nil {
				return err
			}
			wrote = true // stands in for SetKeyYAML
			return nil
		},
	})
	t.Cleanup(ts.Close)

	body := `{"key":"notifiers.0.url","value":"https://hooks.slack.com/services/SECRET"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200, want a rejection for a denied key")
	}
	if wrote {
		t.Error("write branch was reached for a denied key — guard did not block the write")
	}
	respBody, _ := io.ReadAll(resp.Body)
	// The rejection must NOT echo the secret value back to the caller.
	if strings.Contains(string(respBody), "SECRET") {
		t.Errorf("response leaked the secret value: %s", respBody)
	}
}

func TestConfigSetGuardAllowsAllowedKey(t *testing.T) {
	wrote := false
	ts := newTestServer(Hooks{
		SetConfig: func(_ context.Context, req ConfigSetRequest) error {
			if err := config.AllowConfigKey(req.Key); err != nil {
				return err
			}
			wrote = true
			return nil
		},
	})
	t.Cleanup(ts.Close)

	body := `{"key":"log.level","value":"debug"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200 (body=%s)", resp.StatusCode, respBody)
	}
	if !wrote {
		t.Error("write branch was not reached for an allowed key")
	}
}
