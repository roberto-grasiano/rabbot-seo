package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestServeShutdownConcurrent drives the real production path: ListenAndServe in
// one goroutine, Shutdown in another, on a real ephemeral port (ControlPort>0).
// Under -race this exposes any unsynchronized access to the internal *http.Server
// pointer shared by the two methods. ListenAndServe must return cleanly
// (http.ErrServerClosed) and Shutdown must return nil — no leaked listener.
func TestServeShutdownConcurrent(t *testing.T) {
	for i := 0; i < 50; i++ {
		srv := NewServer(ServerOptions{Token: "tok", Version: "0.1.0"})

		var wg sync.WaitGroup
		var serveErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Port 0 => OS-assigned ephemeral loopback port (real bound listener).
			serveErr = srv.ListenAndServe(0)
		}()

		// Race Shutdown against the assignment/Serve in the goroutine.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutErr := srv.Shutdown(ctx)
		cancel()
		if shutErr != nil {
			t.Fatalf("iter %d: Shutdown returned %v, want nil", i, shutErr)
		}

		wg.Wait()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatalf("iter %d: ListenAndServe returned %v, want nil or ErrServerClosed", i, serveErr)
		}
	}
}

// TestShutdownBeforeServe asserts the lost-shutdown / leaked-listener hazard is
// closed: a Shutdown that arrives before the serve goroutine binds must mark the
// server as shutting down so a subsequent ListenAndServe returns without leaving
// a bound listener serving forever.
func TestShutdownBeforeServe(t *testing.T) {
	srv := NewServer(ServerOptions{Token: "tok", Version: "0.1.0"})

	// Shutdown first, before ListenAndServe is ever called.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before serve: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(0) }()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe after prior Shutdown returned %v, want nil or ErrServerClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListenAndServe did not return after a prior Shutdown — listener leaked")
	}
}

// TestBodyTooLargeRejected asserts the http.MaxBytesReader cap (F20): a
// body-consuming handler must reject an oversized payload rather than buffer it
// unbounded. The decode trips the reader's limit, which decodeBody maps to 413
// Request Entity Too Large via errors.As(*http.MaxBytesError) (finding Low-413).
func TestBodyTooLargeRejected(t *testing.T) {
	srv := NewServer(ServerOptions{
		Token:   "tok",
		Version: "0.1.0",
		Hooks: Hooks{
			AddSite: func(ctx context.Context, req AddSiteRequest) (AddSiteResponse, error) {
				t.Error("hook must not run for an oversized body")
				return AddSiteResponse{}, nil
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 2 MiB of valid JSON whitespace + payload, comfortably over the 1 MiB cap.
	big := strings.Repeat(" ", 2<<20) + `{"url":"https://x.example"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sites", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", resp.StatusCode)
	}
}
