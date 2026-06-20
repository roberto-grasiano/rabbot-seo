package control_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestClientPauseResume(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		_ = writeOK(w)
	}))
	defer srv.Close()

	c := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if err := c.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v1/pause" || paths[1] != "/v1/resume" {
		t.Errorf("paths = %v, want [/v1/pause /v1/resume]", paths)
	}
}

func TestClientStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.0","due_count":5,"queue_depth":3,"egress_ip":["203.0.113.7"]}`))
	}))
	defer srv.Close()

	c := control.NewClientWithBaseURL(srv.URL, "tok")
	resp, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if resp.DueCount != 5 || resp.QueueDepth != 3 || len(resp.EgressIP) != 1 {
		t.Errorf("Status() = %+v, want due=5 queue=3 egress=[203.0.113.7]", resp)
	}
}

func TestClientRemoveSiteAndSetConfig(t *testing.T) {
	var rmPath, rmQuery, cfgPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			rmPath = r.URL.Path
			rmQuery = r.URL.RawQuery
		case r.URL.Path == "/v1/config":
			cfgPath = r.URL.Path
		}
		_ = writeOK(w)
	}))
	defer srv.Close()

	c := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := c.RemoveSite(context.Background(), "7", true); err != nil {
		t.Fatalf("RemoveSite() error = %v", err)
	}
	if rmPath != "/v1/sites/7" || rmQuery != "purge=true" {
		t.Errorf("remove path=%q query=%q, want /v1/sites/7 purge=true", rmPath, rmQuery)
	}
	if err := c.SetConfig(context.Background(), "log.level", "debug"); err != nil {
		t.Fatalf("SetConfig() error = %v", err)
	}
	if cfgPath != "/v1/config" {
		t.Errorf("config path = %q, want /v1/config", cfgPath)
	}
}

func writeOK(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write([]byte(`{"ok":true}`))
	return err
}
