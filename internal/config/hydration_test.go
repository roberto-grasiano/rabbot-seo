package config

import (
	"path/filepath"
	"testing"
)

// TestHydrationDefaults pins the A8 crawler.hydration defaults: recovery ON with a
// 2 MiB per-payload decode cap. A regression to false/0 would silently disable
// hydration recovery (the back-fill/compose path) for every default install.
func TestHydrationDefaults(t *testing.T) {
	c := Defaults()
	if !c.Crawler.Hydration.Enabled {
		t.Errorf("Defaults().Crawler.Hydration.Enabled = false, want true (recovery on by default)")
	}
	if got, want := c.Crawler.Hydration.MaxPayloadBytes, 2*1024*1024; got != want {
		t.Errorf("Defaults().Crawler.Hydration.MaxPayloadBytes = %d, want %d (2 MiB)", got, want)
	}
}

// TestHydrationOmittedKeyInheritsDefault pins the merge-order trap: a config file
// that does NOT mention crawler.hydration must inherit Enabled=true from Defaults()
// (Load seeds the struct from Defaults() then merges file/env over it), NOT the
// koanf zero-value false. A back-compatible upgrade (old config + new binary) must
// keep recovery on.
func TestHydrationOmittedKeyInheritsDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// A valid config that says nothing about crawler.hydration.
	writeFile(t, cfgPath, "crawler:\n  contact_email: ops@example.com\n")

	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !c.Crawler.Hydration.Enabled {
		t.Errorf("hydration.enabled = false after loading a config that omits it; want true inherited from Defaults()")
	}
	if got, want := c.Crawler.Hydration.MaxPayloadBytes, 2*1024*1024; got != want {
		t.Errorf("hydration.max_payload_bytes = %d, want %d inherited from Defaults()", got, want)
	}
}

// TestHydrationDisableViaConfig proves an operator CAN turn recovery off in the
// config file (the explicit false must survive the merge over the true default).
func TestHydrationDisableViaConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "crawler:\n  contact_email: ops@example.com\n  hydration:\n    enabled: false\n")

	c, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.Crawler.Hydration.Enabled {
		t.Errorf("hydration.enabled = true, want false (explicit config disable must win over the default)")
	}
}

// TestHydrationEnabledIsControlPlaneSettable pins the allow-list deliverable:
// crawler.hydration.enabled is settable over the control plane (an agent can toggle
// recovery), while the resource-bounding max_payload_bytes is NOT (a DoS guard, like
// metrics.addr).
func TestHydrationEnabledIsControlPlaneSettable(t *testing.T) {
	if err := AllowConfigKey("crawler.hydration.enabled"); err != nil {
		t.Errorf("AllowConfigKey(crawler.hydration.enabled) = %v, want nil (must be settable)", err)
	}
	if err := AllowConfigKey("crawler.hydration.max_payload_bytes"); err == nil {
		t.Error("AllowConfigKey(crawler.hydration.max_payload_bytes) = nil, want an error (DoS guard, not control-plane settable)")
	}
}
