package config

import (
	"path/filepath"
	"testing"
)

// Defaults: metrics is OFF — Addr is the empty string (settled). The listener
// is opened only when a setup path sets metrics.addr.
func TestMetricsDefaultOff(t *testing.T) {
	c := Defaults()
	if c.Metrics.Addr != "" {
		t.Fatalf("Defaults().Metrics.Addr = %q, want \"\" (metrics off by default)", c.Metrics.Addr)
	}
}

// Criterion 7 (env half): Load honors RABBOT_METRICS__ADDR (the "__" -> "."
// delimiter mapping maps to the metrics.addr key).
func TestMetricsAddrEnv(t *testing.T) {
	t.Setenv("RABBOT_METRICS__ADDR", "127.0.0.1:9464")
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.Metrics.Addr != "127.0.0.1:9464" {
		t.Fatalf("metrics.addr = %q, want 127.0.0.1:9464 (RABBOT_METRICS__ADDR)", c.Metrics.Addr)
	}
}

// Validate accepts an empty addr (off), accepts a well-formed host:port, and
// rejects a non-empty addr that fails net.SplitHostPort.
func TestMetricsAddrValidate(t *testing.T) {
	base := Defaults()
	base.Crawler.ContactEmail = "ops@example.com"

	t.Run("empty is valid (off)", func(t *testing.T) {
		c := base
		c.Metrics.Addr = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate with empty metrics.addr: %v", err)
		}
	})
	t.Run("well-formed host:port is valid", func(t *testing.T) {
		c := base
		c.Metrics.Addr = "127.0.0.1:9464"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate with valid metrics.addr: %v", err)
		}
	})
	t.Run("missing port is rejected", func(t *testing.T) {
		c := base
		c.Metrics.Addr = "127.0.0.1"
		if err := c.Validate(); err == nil {
			t.Fatal("Validate accepted metrics.addr with no port; want error")
		}
	})
}

// Criterion 7 (allow-list half): metrics.addr is NOT settable over the control
// plane — it changes network exposure and binds only at startup, so it is
// file/env-only. AllowConfigKey must reject it.
func TestMetricsAddrNotControlSettable(t *testing.T) {
	if err := AllowConfigKey("metrics.addr"); err == nil {
		t.Fatal("AllowConfigKey(\"metrics.addr\") returned nil; metrics.addr must be control-plane rejected")
	}
}
