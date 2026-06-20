package config

import (
	"strings"
	"testing"
)

func TestAllowConfigKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		wantErr bool
		// wantSubstr is a substring the error must contain (deny reason or
		// the allow-list hint). Only checked when wantErr is true.
		wantSubstr string
	}{
		// ── Allowed exact keys ──
		{"log level", "log.level", false, ""},
		{"defaults min_interval", "defaults.min_interval", false, ""},
		{"defaults max_interval", "defaults.max_interval", false, ""},
		{"defaults discovery max_pages_per_site", "defaults.discovery.max_pages_per_site", false, ""},
		{"graph enabled", "graph.enabled", false, ""},
		{"graph sweep_interval", "graph.sweep_interval", false, ""},

		// ── A9 graph CAP knobs: advertised as file/env-only, NOT control-settable
		//    (DoS surface). The asymmetry is deliberate (allowlist.go) — these must
		//    be rejected so a caller can never raise the resource bounds. ──
		{"graph max_outlinks (cap)", "graph.max_outlinks_per_page", true, "not settable"},
		{"graph export_max_nodes (cap)", "graph.export_max_nodes", true, "not settable"},
		{"graph export_max_edges (cap)", "graph.export_max_edges", true, "not settable"},

		// ── DENY families: load-bearing, always rejected ──
		{"throttle floor rate", "defaults.unverified_throttle.per_host_rate", true, "throttle floor"},
		{"throttle floor max_pages", "defaults.unverified_throttle.max_pages", true, "throttle floor"},
		{"notifier url (secret)", "notifiers.0.url", true, "secret"},
		{"notifier name", "notifiers.0.name", true, "secret"},
		{"database path", "database.path", true, "database"},
		{"data_dir exact", "data_dir", true, "database"},

		// ── Not allow-listed, not denied: still rejected (closed by default) ──
		{"control port", "control.port", true, "not settable"},
		{"crawler contact_email", "crawler.contact_email", true, "not settable"},
		{"unknown key", "totally.bogus.key", true, "not settable"},
		{"empty key", "", true, "not settable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AllowConfigKey(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("AllowConfigKey(%q) = nil, want error", tc.key)
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("AllowConfigKey(%q) err = %q, want substring %q", tc.key, err.Error(), tc.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AllowConfigKey(%q) = %v, want nil", tc.key, err)
			}
		})
	}
}

// TestAllowConfigKey_ListsAllowedKeysInError ensures a rejected non-denied key
// tells the operator which keys ARE settable — a friendly, actionable failure.
func TestAllowConfigKey_ListsAllowedKeysInError(t *testing.T) {
	t.Parallel()

	err := AllowConfigKey("control.port")
	if err == nil {
		t.Fatal("expected error for non-allow-listed key")
	}
	for _, want := range []string{"log.level", "defaults.min_interval", "defaults.max_interval", "defaults.discovery.max_pages_per_site"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list allowed key %q", err.Error(), want)
		}
	}
}

// TestDenyWinsOverAllow guards the invariant that a DENY-prefixed key is rejected
// even if it would otherwise look allow-listed-adjacent. defaults.* has allowed
// leaves (min_interval) but the unverified_throttle.* subtree is always denied.
func TestDenyWinsOverAllow(t *testing.T) {
	t.Parallel()

	if got := DenyConfigKey("defaults.min_interval"); got != "" {
		t.Errorf("DenyConfigKey(defaults.min_interval) = %q, want \"\" (allowed leaf is not denied)", got)
	}
	if got := DenyConfigKey("defaults.discovery.max_pages_per_site"); got != "" {
		t.Errorf("DenyConfigKey(defaults.discovery.max_pages_per_site) = %q, want \"\" (allowed leaf is not denied)", got)
	}
	if got := DenyConfigKey("defaults.unverified_throttle.min_interval"); got == "" {
		t.Error("DenyConfigKey(defaults.unverified_throttle.min_interval) = \"\", want a deny reason")
	}
	if got := DenyConfigKey("defaults.unverified_throttle.max_pages"); got == "" {
		t.Error("DenyConfigKey(defaults.unverified_throttle.max_pages) = \"\", want a deny reason")
	}
}
