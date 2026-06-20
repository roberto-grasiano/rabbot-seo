package wizard

import (
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// SiteDraft is one monitored site as collected by the wizard. It carries BOTH the
// fields setup.Apply persists (URL/Name/MinInterval/MaxInterval/Speed) AND the
// proof-of-control intent (Method/Token/Verified/VerifiedAt) that Apply CANNOT
// handle today. The two are split on purpose: BuildPlan maps only the
// Apply-handled fields onto setup.Plan, and the runner records the proof intent
// separately via config.SetSiteVerificationYAML AFTER Apply (the daemon
// re-verifies authoritatively later — spec §E "living state"). Keeping proof
// state off setup.Plan is the key seam that lets Phase 5 ship without extending
// the setup core.
type SiteDraft struct {
	URL         string
	Name        string
	MinInterval string
	MaxInterval string
	Speed       int

	// MaxPages is the operator's coverage-cap choice from the Spec B cap step,
	// OUT-OF-BAND like Method/Token: BuildPlan IGNORES it (setup.Apply cannot write a
	// discovery block), and the runner writes it post-Apply via
	// config.SetSiteMaxPagesYAML (by URL — never SetKeyYAML). Three states:
	//   nil  → keep the resolved default (no write),
	//   &0   → monitor all (unlimited),
	//   &N   → cap at N (validated ≥ 0).
	MaxPages *int

	// Proof-of-control intent (out-of-band — NOT carried on setup.Plan).
	Method     verify.Method
	Token      string
	Verified   bool
	VerifiedAt time.Time
}

// Inputs is the full, pure result of the wizard collection — everything the
// runner needs to assemble a setup.Plan AND record per-site proof intent. It has
// no TTY/UI dependency, so the whole mapping is unit-testable.
type Inputs struct {
	ContactEmail string
	Authorized   bool
	AttestedAt   time.Time
	Sites        []SiteDraft

	// Connect-Claude (step 9) intent — OUT-OF-BAND, like proof intent. BuildPlan
	// ignores these (setup.Apply cannot persist them); the runner acts on them
	// AFTER Apply by calling the internal/mcp writer. ConnectMCP records whether
	// the user opted to wire an MCP host config; ConnectTarget is the target enum
	// value (print|project|claude-code|claude-desktop) the runner maps via
	// mcpsrv.ParseTarget. Keeping these off setup.Plan is the seam that lets step 9
	// ship without extending the setup core.
	ConnectMCP    bool
	ConnectTarget string

	// Alerts (step 8) + run/service (step 10) intent — OUT-OF-BAND, exactly like
	// the Connect-Claude fields above. BuildPlan IGNORES all three (setup.Apply
	// cannot persist them); the runner acts on them AFTER Apply:
	//   - SlackWebhook   -> config.AddNotifierYAML + a best-effort test alert.
	//   - StartDaemon     -> start the daemon now.
	//   - InstallService  -> install the OS service (with an elevation notice).
	// SlackWebhook is a SECRET: it flows verbatim into AddNotifierYAML (so an
	// ${ENV} token survives) and is NEVER printed; the wizard collects it with a
	// masked huh input. Keeping these off setup.Plan is the seam that lets steps
	// 8+10 ship without extending the setup core.
	SlackWebhook   string
	StartDaemon    bool
	InstallService bool
}

// BuildPlan maps collected Inputs onto a setup.Plan and validates it. It carries
// ONLY the five fields setup.Apply persists (URL/Name/MinInterval/MaxInterval/
// Speed); Method/Token/Verified are deliberately left off the Plan because
// setup.Apply cannot write them — the runner applies those out-of-band via
// config.SetSiteVerificationYAML after Apply. BuildPlan calls plan.Validate()
// and returns its error verbatim, so a malformed wizard collection (no contact
// URL, not authorized, no sites, or a private/loopback site) fails loudly with
// the matching setup sentinel rather than producing a half-valid Plan.
func BuildPlan(in Inputs) (setup.Plan, error) {
	sites := make([]setup.SiteInput, 0, len(in.Sites))
	for _, s := range in.Sites {
		sites = append(sites, setup.SiteInput{
			URL:         s.URL,
			Name:        s.Name,
			MinInterval: s.MinInterval,
			MaxInterval: s.MaxInterval,
			Speed:       s.Speed,
		})
	}
	plan := setup.Plan{
		ContactEmail: in.ContactEmail,
		Authorized:   in.Authorized,
		Sites:        sites,
	}
	if err := plan.Validate(); err != nil {
		return setup.Plan{}, err
	}
	return plan, nil
}
