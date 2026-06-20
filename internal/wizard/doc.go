// Package wizard is the interactive, terminal-UI onboarding front-end for
// `rabbot init`. It is UI-only: it collects first-run inputs through a Charm
// (huh + bubbletea + lipgloss) flow and assembles a setup.Plan that the existing
// UI-agnostic internal/setup core validates and applies. It depends DOWNWARD on
// internal/{setup,precheck,verify,config,fetcher} and has ZERO cobra dependency,
// so the cobra layer (internal/cli) wires it in behind a TTY check while the
// wizard itself stays a plain library.
//
// The wizard collects the full onboarding flow: welcome + authorization
// attestation (step 1), contact EMAIL with a live identity preview (step 2), first
// monitored site (step 3), LIVE precheck screen (step 4), LIVE proof-of-control
// screen (step 5), scope/cadence prefilled from the defaults (step 6),
// Connect-Claude MCP target (step 7), optional Slack-webhook alerts (step 8),
// and run-now / install-service offer (step 9). The summary (step 10) is rendered
// by the cli runner (internal/cli) using the shared setup.RenderSummary helper so
// the headless and wizard paths share one UI-free renderer.
package wizard
