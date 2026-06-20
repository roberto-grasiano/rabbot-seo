package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsArePopulated(t *testing.T) {
	c := Defaults()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"control port", c.Control.Port, 7777},
		{"log level", c.Log.Level, "info"},
		{"min interval", c.Defaults.MinInterval, "10m"},
		{"max interval", c.Defaults.MaxInterval, "24h"},
		{"per host concurrency", c.Defaults.PerHostConcurrency, 2},
		{"per host rate", c.Defaults.PerHostRate, "2s"},
		{"speed scale", c.Defaults.SpeedScale, 100},
		{"egress enabled", c.Crawler.EgressCheckEnabled, true},
		{"egress endpoint", c.Crawler.EgressCheckEndpoint, "https://api.ipify.org"},
		{"dedup window", c.Alerting.DedupWindow, "5m"},
		{"hourly cap", c.Alerting.PerRecipientHourlyCap, 30},
		{"incident autoclose", c.Alerting.IncidentAutoCloseAfter, "24h"},
		{"digest schedule", c.Alerting.Digest.Schedule, "1h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// TestGraphDefaults pins the A9 link-graph LITE defaults: feature ON, a 6h BFS
// sweep, and the three bounded caps. A config file that omits the `graph:` section
// must inherit Enabled=true (NOT the koanf zero-value false), so this guards the
// default-true seed in Defaults().
func TestGraphDefaults(t *testing.T) {
	c := Defaults()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"enabled", c.Graph.Enabled, true},
		{"sweep interval", c.Graph.SweepInterval, "6h"},
		{"max outlinks per page", c.Graph.MaxOutlinksPerPage, 500},
		{"export max nodes", c.Graph.ExportMaxNodes, 100},
		{"export max edges", c.Graph.ExportMaxEdges, 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
	if got := c.GraphSweepInterval(); got != 6*time.Hour {
		t.Errorf("GraphSweepInterval() = %v, want 6h", got)
	}
}

// TestGraphValidate exercises the graph-knob validation arms in Validate(): a bad
// sweep_interval and a negative cap each fail, but only when graph.enabled is true.
func TestGraphValidate(t *testing.T) {
	base := func() Config {
		c := Defaults()
		c.Crawler.ContactEmail = "ops@example.com" // satisfy the mandatory-email gate
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}

	bad := base()
	bad.Graph.SweepInterval = "not-a-duration"
	if err := bad.Validate(); err == nil {
		t.Error("bad graph.sweep_interval should fail Validate()")
	}

	neg := base()
	neg.Graph.MaxOutlinksPerPage = -1
	if err := neg.Validate(); err == nil {
		t.Error("negative graph.max_outlinks_per_page should fail Validate()")
	}

	// A bad sweep_interval is tolerated when the feature is OFF (the knob is dead).
	off := base()
	off.Graph.Enabled = false
	off.Graph.SweepInterval = "not-a-duration"
	if err := off.Validate(); err != nil {
		t.Errorf("graph disabled should skip graph validation: %v", err)
	}
}

func TestUnverifiedThrottleDefaults(t *testing.T) {
	c := Defaults()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"per host rate", c.Defaults.UnverifiedThrottle.PerHostRate, "60s"},
		{"per host concurrency", c.Defaults.UnverifiedThrottle.PerHostConcurrency, 1},
		{"max pages", c.Defaults.UnverifiedThrottle.MaxPages, 50},
		{"min interval", c.Defaults.UnverifiedThrottle.MinInterval, "30m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

func TestSetupConfigField(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := SetKeyYAML(cfgPath, "setup.attested_at", "2026-06-04T00:00:00Z"); err != nil {
		t.Fatalf("SetKeyYAML: %v", err)
	}
	cfg, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Setup.AttestedAt != "2026-06-04T00:00:00Z" {
		t.Errorf("Setup.AttestedAt = %q, want round-tripped value", cfg.Setup.AttestedAt)
	}
}

func TestSiteVerificationConfigField(t *testing.T) {
	var s SiteConfig
	s.Verification.Method = "well_known"
	s.Verification.Token = "rab_abc"
	s.Verification.VerifiedAt = "2026-06-05T12:00:00Z"
	if s.Verification.Method != "well_known" || s.Verification.Token != "rab_abc" ||
		s.Verification.VerifiedAt != "2026-06-05T12:00:00Z" {
		t.Fatalf("SiteConfig.Verification not settable/readable: %+v", s.Verification)
	}
}

func TestValidateRequiresContactEmail(t *testing.T) {
	tests := []struct {
		name    string
		contact string
		wantErr error
	}{
		{"empty contact fails", "", ErrContactEmailRequired},
		{"non-email fails", "https://github.com/roberto-grasiano/rabbot-seo", ErrContactEmailRequired},
		{"present email ok", "ops@example.com", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Crawler.ContactEmail = tc.contact
			err := c.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate() err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		email string
		ok    bool
	}{
		{"ops@example.com", true},
		{"a.b+tag@sub.example.co.uk", true},
		{"name@lottie.org", true},
		{"", false},                 // empty
		{"plainaddress", false},     // no @
		{"@example.com", false},     // empty local part
		{"ops@", false},             // empty domain
		{"ops@localhost", false},    // domain has no dot
		{"ops@@example.com", false}, // two @
		{"o ps@example.com", false}, // space in local part
		{"ops@exa mple.com", false}, // space in domain
		{" ops@example.com", false}, // leading space
		{"ops@example.com ", false}, // trailing space
		{"ops@.com", false},         // domain dot at edge → empty label
		{"ops@example.", false},     // trailing dot in domain
	}
	for _, tc := range tests {
		t.Run(tc.email, func(t *testing.T) {
			if got := ValidateEmail(tc.email); (got == nil) != tc.ok {
				t.Errorf("ValidateEmail(%q) = %v, want ok=%v", tc.email, got, tc.ok)
			}
		})
	}
}
