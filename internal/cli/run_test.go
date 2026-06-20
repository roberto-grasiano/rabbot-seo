package cli

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunFlagParsing(t *testing.T) {
	cmd := newRunCmd(BuildInfo{Version: "0.0.1"})
	if f := cmd.Flags().Lookup("foreground"); f == nil {
		t.Fatal("run command missing --foreground flag")
	}
}

func TestRunDaemonStartsAndStops(t *testing.T) {
	// runDaemon must exit cleanly when its context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:   "", // no file => defaults
			DataDir:      t.TempDir(),
			ControlToken: "tok",
			ControlPort:  0, // 0 => skip the control listener (tests)
			Version:      "0.0.1",
			LogLevel:     "info", // info so the "daemon starting/stopped" lines emit
			TickInterval: 5 * time.Millisecond,
		})
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not exit within 2s of cancel")
	}
	if !strings.Contains(out.String(), "daemon") {
		t.Errorf("expected a daemon log line, got: %q", out.String())
	}
}

// TestRunDaemonControlBindFailureIsFatal asserts that a control-server bind
// failure (the common case: control.port already in use by a stale daemon or
// another process) is surfaced to runDaemon as a fatal error instead of being
// logged from a detached goroutine while the daemon keeps running headless with
// no control plane (F18).
func TestRunDaemonControlBindFailureIsFatal(t *testing.T) {
	// Occupy a port so the control server's net.Listen on the same addr fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         "",
			DataDir:            t.TempDir(),
			ControlToken:       "tok",
			ControlPort:        port, // already bound => control server cannot bind
			Version:            "0.0.1",
			LogLevel:           "info",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runDaemon returned nil on a control-server bind failure; expected a fatal error")
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("runDaemon did not return within 2s on a bind failure (still running headless?)")
	}
}

// TestRunDaemonCleanShutdownNoControlError asserts a normal control-server
// shutdown (ListenAndServe returns http.ErrServerClosed after Shutdown) does NOT
// emit an ERROR-level "control server stopped" log line (F35): http.ErrServerClosed
// is the expected clean-shutdown sentinel, not an error.
func TestRunDaemonCleanShutdownNoControlError(t *testing.T) {
	// Grab a free port, then release it so the control server can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         "",
			DataDir:            t.TempDir(),
			ControlToken:       "tok",
			ControlPort:        port, // free => control server binds and serves
			Version:            "0.0.1",
			LogLevel:           "info",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	// Let the control server bind and serve, then request a clean shutdown.
	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon returned error on clean shutdown: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runDaemon did not exit within 6s of clean shutdown")
	}

	logs := out.String()
	if strings.Contains(logs, `"level":"ERROR"`) && strings.Contains(logs, "control server") {
		t.Errorf("clean shutdown emitted an ERROR-level control-server log line (F35); logs:\n%s", logs)
	}
}
