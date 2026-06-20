package control

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientHealthOK(t *testing.T) {
	srv := NewServer(ServerOptions{Token: "tok", Version: "9.9.9"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	c := NewClientWithBaseURL(ts.URL, "tok")
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestClientUnauthorized(t *testing.T) {
	srv := NewServer(ServerOptions{Token: "right", Version: "1.0.0"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	c := NewClientWithBaseURL(ts.URL, "wrong")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Health err = %v, want ErrUnauthorized", err)
	}
}

func TestClientDaemonDown(t *testing.T) {
	// Point at a port nothing is listening on.
	c := NewClientWithBaseURL("http://127.0.0.1:1", "tok")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("Health err = %v, want ErrDaemonNotRunning", err)
	}
}

func TestClientShutdownInvokesServer(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := NewServer(ServerOptions{
		Token:    "tok",
		Version:  "1.0.0",
		Shutdown: func() { called <- struct{}{} },
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	c := NewClientWithBaseURL(ts.URL, "tok")
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("server Shutdown callback was not invoked")
	}
}

func TestClientShutdownUnauthorized(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := NewServer(ServerOptions{
		Token:    "right",
		Version:  "1.0.0",
		Shutdown: func() { called <- struct{}{} },
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	c := NewClientWithBaseURL(ts.URL, "wrong")
	if err := c.Shutdown(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Shutdown err = %v, want ErrUnauthorized", err)
	}
	select {
	case <-called:
		t.Fatal("Shutdown callback fired on an unauthorized request")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientShutdownDaemonDown(t *testing.T) {
	c := NewClientWithBaseURL("http://127.0.0.1:1", "tok")
	if err := c.Shutdown(context.Background()); !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("Shutdown err = %v, want ErrDaemonNotRunning", err)
	}
}
