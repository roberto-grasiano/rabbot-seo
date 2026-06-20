package config

import (
	"testing"
	"time"
)

func TestDefaultsPopulateRetention(t *testing.T) {
	d := Defaults()
	if !d.Retention.Enabled {
		t.Errorf("Retention.Enabled = false, want true")
	}
	if d.Retention.RawHTMLKeep != 1 {
		t.Errorf("RawHTMLKeep = %d, want 1", d.Retention.RawHTMLKeep)
	}
	if d.Retention.FileSnapshotsKeep != 10 {
		t.Errorf("FileSnapshotsKeep = %d, want 10", d.Retention.FileSnapshotsKeep)
	}
	if d.Retention.SweepInterval != "6h" {
		t.Errorf("SweepInterval = %q, want 6h", d.Retention.SweepInterval)
	}
	if d.Retention.SnapshotMaxAge != "720h" {
		t.Errorf("SnapshotMaxAge = %q, want 720h", d.Retention.SnapshotMaxAge)
	}
}

func TestRetentionAccessors(t *testing.T) {
	c := Defaults()
	if got := c.RetentionSweepInterval(); got != 6*time.Hour {
		t.Errorf("RetentionSweepInterval = %v, want 6h", got)
	}
	if got := c.RetentionSnapshotMaxAge(); got != 720*time.Hour {
		t.Errorf("RetentionSnapshotMaxAge = %v, want 720h", got)
	}
	// "0" means Layer 2 disabled, and must parse to 0 (not the fallback).
	c.Retention.SnapshotMaxAge = "0"
	if got := c.RetentionSnapshotMaxAge(); got != 0 {
		t.Errorf("RetentionSnapshotMaxAge(\"0\") = %v, want 0", got)
	}
}

func TestValidateRejectsBadRetention(t *testing.T) {
	base := Defaults()
	base.Crawler.ContactEmail = "ops@example.com" // make the rest of Validate pass

	bad := base
	bad.Retention.RawHTMLKeep = 0
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate accepted RawHTMLKeep=0, want error")
	}

	bad2 := base
	bad2.Retention.FileSnapshotsKeep = 1
	if err := bad2.Validate(); err == nil {
		t.Errorf("Validate accepted FileSnapshotsKeep=1, want error")
	}

	// Disabled retention with otherwise-bad values must NOT fail validation.
	off := base
	off.Retention.Enabled = false
	off.Retention.RawHTMLKeep = 0
	if err := off.Validate(); err != nil {
		t.Errorf("Validate rejected a disabled retention block: %v", err)
	}
}
