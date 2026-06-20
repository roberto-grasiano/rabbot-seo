package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// syncBuf is a goroutine-safe buffer: the daemon goroutine writes the captured log
// while the test goroutine polls it for the startup warning. A plain bytes.Buffer
// would race under -race (concurrent Write + String).
type syncBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// TestStartupNotifierWarning_FiresAtZeroChannels pins decision #23's run surface: a
// config with ZERO notifiers yields a prominent startup warning that points the
// operator at the wizard, while a config WITH a notifier yields none. The message
// must never be empty when it fires.
func TestStartupNotifierWarning_FiresAtZeroChannels(t *testing.T) {
	// Zero notifiers → warn.
	msg, warn := startupNotifierWarning(config.Config{})
	if !warn {
		t.Fatal("a config with zero notifiers must produce a startup warning")
	}
	if strings.TrimSpace(msg) == "" {
		t.Fatal("the zero-notifier warning message must be non-empty")
	}
	// The warning must guide the operator to a fix without being a secret.
	low := strings.ToLower(msg)
	if !strings.Contains(low, "no") || !strings.Contains(low, "alert") {
		t.Fatalf("warning should mention there are no alert channels: %q", msg)
	}
	if !strings.Contains(low, "rabbot init") {
		t.Fatalf("warning should point at `rabbot init`: %q", msg)
	}

	// With a configured notifier → no warning.
	withOne := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack", Type: config.NotifierTypeSlack, URL: "https://hooks.slack.com/x"},
		},
	}
	if _, warn := startupNotifierWarning(withOne); warn {
		t.Fatal("a config WITH a notifier must NOT produce the zero-notifier warning")
	}
}

// TestRunEmitsZeroNotifierWarning is the integration check: a daemon started from a
// notifier-less config (ConfigPath "" => defaults, which ship notifiers: []) logs
// the zero-channel warning, and does NOT block (it starts and stops cleanly).
// Mirrors the contact-email warning's non-fatal posture and the run_test.go idiom.
func TestRunEmitsZeroNotifierWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, out, daemonOptions{
			ConfigPath:   "", // no file => defaults => zero notifiers
			DataDir:      t.TempDir(),
			ControlToken: "tok",
			ControlPort:  0, // skip the control listener
			Version:      "0.0.1",
			LogLevel:     "info",
			TickInterval: 5 * time.Millisecond,
		})
	}()

	// Poll for the warning instead of sleeping a fixed window (a heavily loaded CI
	// box can blow a fixed margin). Cancel as soon as the line appears — the
	// exactly-once assertion runs after the goroutine returns.
	const wantSub = "no alert channel configured"
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), wantSub) {
		if time.Now().After(deadline) {
			t.Fatalf("startup warning %q did not appear within 2s:\n%s", wantSub, out.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("zero-notifier daemon must start/stop cleanly (non-fatal), got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not exit within 2s of cancel")
	}
	// Decision #23 is explicit that the warning fires EXACTLY ONCE — it lives at
	// daemon startup, never in the per-tick loop. With a 5ms tick a per-tick
	// regression would emit it many times, so assert a count of one rather than mere
	// presence. The message is the only source of this distinctive substring.
	got := strings.Count(out.String(), wantSub)
	if got != 1 {
		t.Fatalf("zero-channel startup warning must fire exactly once, fired %d times:\n%s", got, out.String())
	}
}
