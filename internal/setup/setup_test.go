package setup

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

func TestPlanValidate(t *testing.T) {
	valid := Plan{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites:        []SiteInput{{URL: "https://example.com"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	// A plan with well-formed overrides (parseable intervals with max >= min and
	// a speed inside 1..100) must still validate.
	validOverrides := Plan{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites:        []SiteInput{{URL: "https://example.com", MinInterval: "10m", MaxInterval: "24h", Speed: 100}},
	}
	if err := validOverrides.Validate(); err != nil {
		t.Fatalf("plan with valid overrides rejected: %v", err)
	}

	cases := []struct {
		name string
		plan Plan
		want error
	}{
		{"no contact", Plan{Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}, ErrContactEmailRequired},
		{"contact is a url not email", Plan{ContactEmail: "https://example.com/contact", Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}, ErrContactEmailInvalid},
		{"contact has no at", Plan{ContactEmail: "example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}, ErrContactEmailInvalid},
		{"contact has no domain dot", Plan{ContactEmail: "ops@localhost", Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}, ErrContactEmailInvalid},
		{"contact has a space", Plan{ContactEmail: "ops @example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}, ErrContactEmailInvalid},
		{"not authorized", Plan{ContactEmail: "ops@example.com", Sites: []SiteInput{{URL: "https://example.com"}}}, ErrNotAuthorized},
		{"no sites", Plan{ContactEmail: "ops@example.com", Authorized: true}, ErrNoSites},
		{"unparseable min interval", Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com", MinInterval: "15"}}}, ErrIntervalInvalid},
		{"unparseable max interval", Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com", MaxInterval: "abc"}}}, ErrIntervalInvalid},
		{"max less than min", Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com", MinInterval: "1h", MaxInterval: "10m"}}}, ErrIntervalOrder},
		{"speed too low", Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com", Speed: -1}}}, ErrSpeedOutOfRange},
		{"speed too high", Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com", Speed: 101}}}, ErrSpeedOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPlanValidateRejectsPrivateSite(t *testing.T) {
	p := Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "http://127.0.0.1"}}}
	if err := p.Validate(); err == nil {
		t.Fatal("expected private/loopback site to be rejected")
	}
}

func TestApplyWritesConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	p := Plan{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites:        []SiteInput{{URL: "https://example.com", Name: "Example", MinInterval: "15m"}},
	}
	res, err := p.Apply(Options{ConfigPath: cfgPath, Version: "9.9.9", Now: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Crawler.ContactEmail != "ops@example.com" {
		t.Errorf("contact_email = %q", cfg.Crawler.ContactEmail)
	}
	if len(cfg.Sites) != 1 || cfg.Sites[0].URL != "https://example.com" || cfg.Sites[0].MinInterval != "15m" {
		t.Errorf("sites = %+v", cfg.Sites)
	}
	if cfg.Setup.AttestedAt == "" {
		t.Error("attested_at not written")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("written config invalid: %v", err)
	}
	if res.UserAgent != "Rabbot-SEO/9.9.9 (+mailto:ops@example.com)" {
		t.Errorf("UA = %q", res.UserAgent)
	}
	if len(res.SitesAdded) != 1 {
		t.Errorf("SitesAdded = %v", res.SitesAdded)
	}
}

func TestApplyDefaultsZeroNow(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	p := Plan{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites:        []SiteInput{{URL: "https://example.com"}},
	}
	before := time.Now()
	// Options{} with a zero Now must not write a bogus "0001-01-01T00:00:00Z".
	if _, err := p.Apply(Options{ConfigPath: cfgPath, Version: "1"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := time.Now()

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Setup.AttestedAt == "" {
		t.Fatal("attested_at not written")
	}
	ts, err := time.Parse(time.RFC3339, cfg.Setup.AttestedAt)
	if err != nil {
		t.Fatalf("attested_at %q is not parseable RFC3339: %v", cfg.Setup.AttestedAt, err)
	}
	if ts.IsZero() {
		t.Fatalf("attested_at is the zero value %q; zero Now should default to time.Now()", cfg.Setup.AttestedAt)
	}
	// Sanity: the defaulted timestamp falls within the call window (allow a
	// second of slack on each side for clock granularity).
	if ts.Before(before.Add(-time.Second)) || ts.After(after.Add(time.Second)) {
		t.Errorf("attested_at %v not within [%v, %v]", ts, before, after)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	p := Plan{ContactEmail: "ops@example.com", Authorized: true, Sites: []SiteInput{{URL: "https://example.com"}}}
	opts := Options{ConfigPath: cfgPath, Version: "1", Now: time.Unix(1700000000, 0)}
	if _, err := p.Apply(opts); err != nil {
		t.Fatal(err)
	}
	res, err := p.Apply(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SitesSkipped) != 1 {
		t.Errorf("second apply should skip the existing site, got SitesSkipped=%v added=%v", res.SitesSkipped, res.SitesAdded)
	}
	cfg, _ := config.Load(cfgPath, nil)
	if len(cfg.Sites) != 1 {
		t.Errorf("expected no duplicate site, got %d", len(cfg.Sites))
	}
}

func TestApplyRejectsInvalidPlan(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	p := Plan{ContactEmail: "ops@example.com", Sites: []SiteInput{{URL: "https://example.com"}}} // not authorized
	if _, err := p.Apply(Options{ConfigPath: cfgPath, Now: time.Unix(1700000000, 0)}); err == nil {
		t.Fatal("expected unauthorized plan to be rejected")
	}
}
