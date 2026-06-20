package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestStopCallsControlShutdown verifies runStop hits POST /v1/shutdown and prints
// a "stopping" message on a 202 Accepted.
func TestStopCallsControlShutdown(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(control.OKResponse{OK: true})
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "tok")
	var out bytes.Buffer
	if err := runStop(context.Background(), client, &out); err != nil {
		t.Fatalf("runStop() error = %v", err)
	}
	if len(paths) != 1 || paths[0] != "POST /v1/shutdown" {
		t.Errorf("paths = %v, want [POST /v1/shutdown]", paths)
	}
	if !strings.Contains(strings.ToLower(out.String()), "stopping") {
		t.Errorf("output %q missing 'stopping' message", out.String())
	}
}

// TestStopDaemonNotRunning verifies that when the daemon is down (connection
// refused), runStop prints a friendly "nothing to stop" message, returns nil
// (exit 0), and does not panic.
func TestStopDaemonNotRunning(t *testing.T) {
	// Port 1 has nothing listening => connection refused => ErrDaemonNotRunning.
	client := control.NewClientWithBaseURL("http://127.0.0.1:1", "tok")
	var out bytes.Buffer
	if err := runStop(context.Background(), client, &out); err != nil {
		t.Fatalf("runStop() on down daemon should not error, got %v", err)
	}
	got := strings.ToLower(out.String())
	if !strings.Contains(got, "not running") || !strings.Contains(got, "nothing to stop") {
		t.Errorf("output %q missing friendly daemon-not-running message", out.String())
	}
}

// TestStopUnauthorizedSurfacesError verifies a real (non-connection) error such
// as a bad token is surfaced, not swallowed as "nothing to stop".
func TestStopUnauthorizedSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(control.ErrorResponse{Error: "unauthorized"})
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "wrong")
	var out bytes.Buffer
	err := runStop(context.Background(), client, &out)
	if err == nil {
		t.Fatal("runStop() with bad token should return an error, got nil")
	}
}
