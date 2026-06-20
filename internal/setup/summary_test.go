package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// TestRenderSummaryContainsPathsAndNextCommands asserts the plain-text summary
// surfaces the config + data paths, each site URL with its state word, the
// Connect-Claude reminder, and the exact next commands.
func TestRenderSummaryContainsPathsAndNextCommands(t *testing.T) {
	s := Summary{
		ConfigPath: "/etc/rabbot/config.yaml",
		DataPath:   "/var/lib/rabbot",
		Sites: []SiteSummary{
			{URL: "https://verified.test", State: "verified"},
			{URL: "https://throttled.test", State: "throttled"},
		},
		SlackConfigured:       true,
		ConnectClaudeReminder: "Connect Claude later with `rabbot init --connect-claude`",
	}

	var b strings.Builder
	if err := RenderSummary(&b, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"/etc/rabbot/config.yaml",
		"/var/lib/rabbot",
		"https://verified.test",
		"verified",
		"https://throttled.test",
		"throttled",
		"Connect Claude later with `rabbot init --connect-claude`",
		"rabbot status",
		"rabbot stop",
		"rabbot sites list",
		"rabbot history",
		"rabbot verify",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// TestRenderSummaryNoSecret pins the no-secret guarantee: the Summary type has no
// webhook field by design, so a notifier URL can never reach the renderer. We feed
// SummaryFromConfig a config whose notifier URL carries a sentinel and assert the
// rendered output never contains it, only a neutral "Slack alerts: configured".
func TestRenderSummaryNoSecret(t *testing.T) {
	cfg := config.Config{
		Sites: []config.SiteConfig{{URL: "https://site.test"}},
		Notifiers: []config.NotifierConfig{
			{Name: "slack", Type: "slack-webhook", URL: "https://hooks.slack.com/SECRET"},
		},
	}
	s := SummaryFromConfig(cfg, "/cfg.yaml", "/data", true, "reminder")

	var b strings.Builder
	if err := RenderSummary(&b, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("notifier URL leaked into summary:\n%s", out)
	}
	if !strings.Contains(out, "Slack alerts: configured") {
		t.Fatalf("expected a neutral Slack-configured line, got:\n%s", out)
	}
}

// TestSummaryFromConfigVerifiedVsThrottled pins that per-site state is derived
// from the config VERIFICATION INTENT at onboarding (the store may not hold the
// site yet; the daemon re-verifies authoritatively later — spec §E).
func TestSummaryFromConfigVerifiedVsThrottled(t *testing.T) {
	cfg := config.Config{
		Sites: []config.SiteConfig{
			{
				URL: "https://a.test",
				Verification: config.VerificationConfig{
					Method: "well_known", Token: "rab_x", VerifiedAt: "2026-06-05T12:00:00Z",
				},
			},
			{URL: "https://b.test"}, // no verification block at all
		},
	}
	s := SummaryFromConfig(cfg, "/cfg.yaml", "/data", false, "")

	if len(s.Sites) != 2 {
		t.Fatalf("want 2 site summaries, got %d", len(s.Sites))
	}
	byURL := map[string]string{}
	for _, ss := range s.Sites {
		byURL[ss.URL] = ss.State
	}
	if byURL["https://a.test"] != "verified" {
		t.Errorf("site A state = %q, want verified", byURL["https://a.test"])
	}
	if byURL["https://b.test"] != "throttled" {
		t.Errorf("site B state = %q, want throttled", byURL["https://b.test"])
	}
	if s.SlackConfigured {
		t.Errorf("SlackConfigured should reflect the passed bool (false)")
	}
}

// TestSummaryFromConfigAttestedThrottled pins the middle tier: a site with a
// method/token but no verified_at is attested intent — throttled until verified.
func TestSummaryFromConfigAttestedThrottled(t *testing.T) {
	cfg := config.Config{
		Sites: []config.SiteConfig{{
			URL:          "https://attested.test",
			Verification: config.VerificationConfig{Method: "dns", Token: "rab_y"},
		}},
	}
	s := SummaryFromConfig(cfg, "/cfg.yaml", "/data", false, "")
	if len(s.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(s.Sites))
	}
	st := s.Sites[0].State
	if !strings.Contains(st, "throttled") {
		t.Fatalf("attested-intent site state = %q, want a throttled variant", st)
	}
}

func TestRenderSummaryShowsFiniteCap(t *testing.T) {
	var buf bytes.Buffer
	s := Summary{
		ConfigPath: "/c.yaml",
		DataPath:   "/d",
		Sites: []SiteSummary{
			{URL: "https://a.test", State: "throttled", MaxPages: 2000},
		},
	}
	if err := RenderSummary(&buf, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "up to 2000 pages") {
		t.Fatalf("missing finite-cap notice:\n%s", out)
	}
	if !strings.Contains(out, "max_pages_per_site") {
		t.Fatalf("cap notice must name the knob:\n%s", out)
	}
}

func TestRenderSummaryUnlimitedCapNoNotice(t *testing.T) {
	var buf bytes.Buffer
	s := Summary{
		ConfigPath: "/c.yaml",
		DataPath:   "/d",
		Sites:      []SiteSummary{{URL: "https://a.test", State: "throttled", MaxPages: 0}},
	}
	if err := RenderSummary(&buf, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	if strings.Contains(buf.String(), "up to") {
		t.Fatalf("unlimited cap should print no page-cap notice:\n%s", buf.String())
	}
}

// TestRenderSummaryUnlimitedCapAffirmsAllPages pins that the "monitor all"
// decision (MaxPages == 0) is stated back affirmatively, not left silent — an
// operator who deliberately chose unlimited sees it confirmed (no "up to N"
// ceiling, but an explicit "monitoring all pages (no cap)" line).
func TestRenderSummaryUnlimitedCapAffirmsAllPages(t *testing.T) {
	var buf bytes.Buffer
	s := Summary{
		ConfigPath: "/c.yaml",
		DataPath:   "/d",
		Sites:      []SiteSummary{{URL: "https://a.test", State: "throttled", MaxPages: 0}},
	}
	if err := RenderSummary(&buf, s); err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "monitoring all pages (no cap)") {
		t.Fatalf("unlimited cap should affirm 'monitoring all pages (no cap)':\n%s", out)
	}
	// The affirming line must NOT resurface the finite-cap ceiling phrasing.
	if strings.Contains(out, "up to") {
		t.Fatalf("unlimited affirmation must not print a finite-cap notice:\n%s", out)
	}
}

// writeSiteCapConfig writes a minimal one-site config to a temp file, then sets the
// per-site cap EXACTLY as the Spec B planner does (config.SetSiteMaxPagesYAML — NEVER
// SetKeyYAML), and returns the path. cap < 0 means "leave the cap unset" (inherit the
// 2000 default — the keep-default choice).
func writeSiteCapConfig(t *testing.T, cap int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := "" +
		"crawler:\n" +
		"  contact_email: ops@me.example\n" +
		"sites:\n" +
		"  - url: https://a.test\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatalf("write base config: %v", err)
	}
	if cap >= 0 {
		if err := config.SetSiteMaxPagesYAML(path, "https://a.test", cap); err != nil {
			t.Fatalf("SetSiteMaxPagesYAML cap=%d: %v", cap, err)
		}
	}
	return path
}

// TestSummaryRoundTripStatesChosenCap proves the onboarding summary reflects the cap the
// Spec B planner just wrote: keep-default (unset → 2000) and set-N both state "up to N
// pages"; monitor-all (0 = unlimited) states no cap notice. End-to-end proof
// (SetSiteMaxPagesYAML → Load → ResolveDiscovery → RenderSummary).
func TestSummaryRoundTripStatesChosenCap(t *testing.T) {
	cases := []struct {
		name       string
		cap        int    // -1 = leave unset (keep default); 0 = unlimited; N = capped
		wantLine   string // substring that MUST appear ("" = none expected)
		wantRemedy string // the source-specific remedy the cap notice must carry
		perSite    bool   // true => remedy points at the per-site key, NOT `config set`
		wantNoCap  bool   // assert no "up to" notice
	}{
		// keep-default: cap comes from the GLOBAL default, so `config set` works.
		{name: "keep-default", cap: -1, wantLine: "up to 2000 pages", wantRemedy: "config set defaults.discovery.max_pages_per_site"},
		// set-N: a PER-SITE cap was written; `config set defaults.…` would not change
		// it, so the remedy must point at the per-site key in config.yaml.
		{name: "set-N", cap: 500, wantLine: "up to 500 pages", wantRemedy: "this site's discovery.max_pages_per_site in config.yaml", perSite: true},
		{name: "monitor-all", cap: 0, wantNoCap: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSiteCapConfig(t, tc.cap)
			cfg, err := config.Load(path, nil)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			s := SummaryFromConfig(cfg, path, "/data", false, "")
			var buf bytes.Buffer
			if err := RenderSummary(&buf, s); err != nil {
				t.Fatalf("RenderSummary: %v", err)
			}
			out := buf.String()
			if tc.wantNoCap {
				if strings.Contains(out, "up to") {
					t.Fatalf("monitor-all should print no cap notice:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantLine) {
				t.Fatalf("missing %q:\n%s", tc.wantLine, out)
			}
			if !strings.Contains(out, "max_pages_per_site") {
				t.Fatalf("cap notice must name the knob:\n%s", out)
			}
			if !strings.Contains(out, tc.wantRemedy) {
				t.Fatalf("cap notice must carry the source-specific remedy %q:\n%s", tc.wantRemedy, out)
			}
			if tc.perSite && strings.Contains(out, "config set") {
				t.Fatalf("a per-site cap must NOT suggest `config set` (no effect):\n%s", out)
			}
		})
	}
}
