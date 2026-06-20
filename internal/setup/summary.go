package setup

import (
	"fmt"
	"io"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// SiteSummary is one monitored site's onboarding-summary line: its URL/name and
// a human state word derived from the config verification INTENT.
type SiteSummary struct {
	URL   string
	Name  string
	State string
	// MaxPages is the resolved per-site page cap surfaced at onboarding (0 =
	// unlimited). The monitored count is unknown pre-crawl, so the summary shows
	// the cap ceiling, not a monitored-vs-cap ratio (that lands in `status` /
	// `sites show` once the daemon has crawled).
	MaxPages int
	// CapPerSite is true when the cap comes from this site's own
	// discovery.max_pages_per_site (a deliberate per-site choice) rather than the
	// global default. A per-site cap overrides the default, so `config set
	// defaults.…` would NOT change it — the remedy must point at the per-site key.
	CapPerSite bool
}

// Summary is the UI-free data for the onboarding summary (step 11). It carries
// NO webhook field by design: the renderer can never surface a notifier URL — it
// only knows whether Slack alerts are configured (SlackConfigured). This keeps the
// secret out of the summary by construction (guarded by TestRenderSummaryNoSecret).
type Summary struct {
	ConfigPath            string
	DataPath              string
	Sites                 []SiteSummary
	SlackConfigured       bool
	ConnectClaudeReminder string
}

// RenderSummary writes a plain-text onboarding summary to w. It is UI-FREE (no
// cobra/charm import) so BOTH the headless cli path and the wizard runner reuse
// one renderer (spec §H: the TUI is a front-end over the same core). It NEVER
// emits a notifier URL — Summary has no such field.
func RenderSummary(w io.Writer, s Summary) error {
	if _, err := fmt.Fprintln(w, "Setup complete."); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  config: %s\n", s.ConfigPath); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  data:   %s\n", s.DataPath); err != nil {
		return err
	}

	if len(s.Sites) > 0 {
		if _, err := fmt.Fprintln(w, "\nSites:"); err != nil {
			return err
		}
		for _, site := range s.Sites {
			label := site.URL
			if site.Name != "" {
				label = fmt.Sprintf("%s (%s)", site.URL, site.Name)
			}
			if _, err := fmt.Fprintf(w, "  %s — %s\n", label, site.State); err != nil {
				return err
			}
			switch {
			case site.MaxPages > 0:
				remedy := "raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>' (0 = all)"
				if site.CapPerSite {
					remedy = "raise/remove this site's discovery.max_pages_per_site in config.yaml (0 = all)"
				}
				if _, err := fmt.Fprintf(w,
					"      monitoring up to %d pages — %s\n", site.MaxPages, remedy); err != nil {
					return err
				}
			default:
				// MaxPages == 0 is an explicit "unlimited" choice (ResolveDiscovery
				// returns 0 for no cap). State the decision back affirmatively rather
				// than leaving the monitor-all choice silent.
				if _, err := fmt.Fprintln(w, "      monitoring all pages (no cap)"); err != nil {
					return err
				}
			}
		}
	}

	slack := "none"
	if s.SlackConfigured {
		slack = "configured"
	}
	if _, err := fmt.Fprintf(w, "\nSlack alerts: %s\n", slack); err != nil {
		return err
	}

	if s.ConnectClaudeReminder != "" {
		if _, err := fmt.Fprintf(w, "%s\n", s.ConnectClaudeReminder); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "\nNext:"); err != nil {
		return err
	}
	for _, line := range []string{
		"  rabbot status            # daemon health and queue depth",
		"  rabbot stop              # gracefully stop the running daemon",
		"  rabbot sites list        # the sites you are monitoring",
		"  rabbot history <url>     # recorded changes for a page",
		"  rabbot verify <site>     # lift the unverified throttle",
	} {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// SummaryFromConfig derives a Summary from a loaded config, the resolved paths,
// whether Slack is configured, and a Connect-Claude reminder line. Per-site State
// reflects the config VERIFICATION INTENT, NOT the store: at onboarding the daemon
// may not yet hold the site, so we read intent and the daemon re-verifies
// authoritatively later (spec §E "living state").
//
//   - verified_at set            => "verified"
//   - method/token present only  => "attested (throttled)"
//   - nothing                    => "throttled"
//
// It MUST NOT read or include any notifier URL — only the SlackConfigured bool.
func SummaryFromConfig(cfg config.Config, configPath, dataPath string, slackConfigured bool, reminder string) Summary {
	sites := make([]SiteSummary, 0, len(cfg.Sites))
	for _, sc := range cfg.Sites {
		sites = append(sites, SiteSummary{
			URL:        sc.URL,
			Name:       sc.Name,
			State:      siteState(sc.Verification),
			MaxPages:   cfg.ResolveDiscovery(sc).MaxPages,
			CapPerSite: sc.Discovery.MaxPagesPerSite != nil,
		})
	}
	return Summary{
		ConfigPath:            configPath,
		DataPath:              dataPath,
		Sites:                 sites,
		SlackConfigured:       slackConfigured,
		ConnectClaudeReminder: reminder,
	}
}

// siteState maps a config verification block to the onboarding state word. It is
// intent-only — the daemon's DB proof record is the authoritative living state.
func siteState(v config.VerificationConfig) string {
	switch {
	case v.VerifiedAt != "":
		return "verified"
	case v.Method != "" || v.Token != "":
		return "attested (throttled)"
	default:
		return "throttled"
	}
}
