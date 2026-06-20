package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeControlBackend struct {
	ignored  []int64
	notified []string
}

func (f *fakeControlBackend) IgnoreIssue(ctx context.Context, id int64) error {
	f.ignored = append(f.ignored, id)
	return nil
}
func (f *fakeControlBackend) NotifyTest(ctx context.Context, notifier string) error {
	f.notified = append(f.notified, notifier)
	return nil
}

// newTestServerBE builds the M0 canonical server (§B) wired with the two M2
// hooks supplied by the fake backend. (newTestServer is already defined in
// server_test.go with a different signature, hence the distinct name.)
func newTestServerBE(be *fakeControlBackend) *Server {
	return NewServer(ServerOptions{
		Token:   "secret-token",
		Version: "test",
		Hooks:   Hooks{IgnoreIssue: be.IgnoreIssue, NotifyTest: be.NotifyTest},
	})
}

func TestIgnoreIssueHandler(t *testing.T) {
	be := &fakeControlBackend{}
	srv := newTestServerBE(be) // M0 canonical constructor (§B); M2 supplies the two hooks
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/issues/42/ignore", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(be.ignored) != 1 || be.ignored[0] != 42 {
		t.Errorf("expected issue 42 ignored, got %v", be.ignored)
	}
}

func TestNotifyTestHandler(t *testing.T) {
	be := &fakeControlBackend{}
	srv := newTestServerBE(be)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(NotifyTestRequest{Notifier: "slack-critical"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/notify/test", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(be.notified) != 1 || be.notified[0] != "slack-critical" {
		t.Errorf("expected notify test for slack-critical, got %v", be.notified)
	}
}

func TestIgnoreIssueRequiresAuth(t *testing.T) {
	be := &fakeControlBackend{}
	srv := newTestServerBE(be)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/issues/42/ignore", nil)
	// no Authorization header
	resp, _ := http.DefaultClient.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token must 401, got %d", resp.StatusCode)
	}
}
