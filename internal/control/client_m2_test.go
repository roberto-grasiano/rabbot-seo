package control_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestClientIgnoreIssue(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		_ = writeOK(w)
	}))
	defer srv.Close()

	c := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := c.IgnoreIssue(context.Background(), 42); err != nil {
		t.Fatalf("IgnoreIssue() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/issues/42/ignore" {
		t.Errorf("request = %s %s, want POST /v1/issues/42/ignore", gotMethod, gotPath)
	}
}

func TestClientNotifyTest(t *testing.T) {
	var gotPath, gotMethod, gotNotifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		var req control.NotifyTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotNotifier = req.Notifier
		_ = writeOK(w)
	}))
	defer srv.Close()

	c := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := c.NotifyTest(context.Background(), "slack-critical"); err != nil {
		t.Fatalf("NotifyTest() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/notify/test" {
		t.Errorf("request = %s %s, want POST /v1/notify/test", gotMethod, gotPath)
	}
	if gotNotifier != "slack-critical" {
		t.Errorf("notifier = %q, want slack-critical", gotNotifier)
	}
}
