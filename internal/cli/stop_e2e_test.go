package cli

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestRunDaemonShutdownOverWire starts a real daemon (with a bound loopback
// control server) and stops it through the new POST /v1/shutdown route via the
// typed client — the exact path `rabbot stop` drives. It asserts the daemon
// exits cleanly (no error), proving Shutdown: cancel in run.go produces the same
// graceful teardown the SIGINT/SIGTERM path runs. This is the load-bearing
// integration test that the new wire (handler -> cancel -> drain/checkpoint)
// actually stops the process end to end.
func TestRunDaemonShutdownOverWire(t *testing.T) {
	// Grab a free port, then release it so the control server can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // safety net if Shutdown-over-wire fails to stop the daemon
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

	// Wait until the control server is actually listening before stopping it over
	// the wire. A flat sleep races net.Listen+Serve under -race or on a loaded CI
	// host and would surface as ErrDaemonNotRunning; poll /v1/health until ready.
	client := control.NewClient(port, "tok")
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if herr := client.Health(context.Background()); herr == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("control server did not become ready within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("client.Shutdown over wire: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon returned error after wire shutdown: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runDaemon did not exit within 6s of a POST /v1/shutdown")
	}
}
