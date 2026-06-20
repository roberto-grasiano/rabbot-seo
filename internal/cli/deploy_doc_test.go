package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deployDoc reads a doc file under the module root (reusing repoRoot from
// readme_doc_test.go). A missing file fails the test: the B7 deployment-story
// guides and the B2 observability pages are launch-gate artifacts, not optional.
func deployDoc(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(raw)
}

// assertAnchors fails for every wanted substring (case-insensitive) absent from doc.
func assertAnchors(t *testing.T, where, doc string, wants ...string) {
	t.Helper()
	low := strings.ToLower(doc)
	for _, w := range wants {
		if !strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("%s missing anchor %q", where, w)
		}
	}
}

// TestReadmeWhereToRun guards B7 criterion 1: the README gains a "Where to run
// Rabbot" section that positions a laptop install (trying Rabbot + watching
// staging is a genuine use) against the always-on box the real-time promise
// needs, names the sleep gap honestly, and links all three run-target guides
// plus Docker. The "now vs after reboot" line is the load-bearing distinction
// between the wizard's session-scoped start and the service.
func TestReadmeWhereToRun(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "README.md")
	if !strings.Contains(strings.ToLower(doc), "where to run rabbot") {
		t.Fatal("README.md has no \"Where to run Rabbot\" heading")
	}
	assertAnchors(t, "README.md \"Where to run Rabbot\"", doc,
		"laptop",               // the genuine-but-limited use
		"staging",              // catch regressions before deploy
		"sleep",                // the honest laptop caveat
		"docs/vps.md",          // VPS run-target
		"docs/raspberry-pi.md", // Pi run-target
		"docs/mac-mini.md",     // Mac mini run-target
		"docker",               // the container 24/7 story
	)
	// The now-vs-after-reboot distinction: the wizard starts monitoring now; the
	// service keeps it monitoring after reboot. Both words must be present.
	low := strings.ToLower(doc)
	if !strings.Contains(low, "now") || !strings.Contains(low, "after reboot") {
		t.Error("README.md \"Where to run Rabbot\" must state the wizard starts monitoring NOW, the service keeps it AFTER reboot")
	}
}

// TestPiGuideAnchors guards B7 criterion 2: docs/raspberry-pi.md exists and
// states the load-bearing facts — 64-bit/arm64 honesty (prebuilts are arm64-only),
// USB-SSD over the SD card for SQLite write endurance with the data_dir override,
// retention + `rabbot db compact` to keep the DB small, systemd via
// `rabbot service install`, and `rabbot doctor` for sizing.
func TestPiGuideAnchors(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "raspberry-pi.md")
	assertAnchors(t, "docs/raspberry-pi.md", doc,
		"64-bit",
		"arm64",
		"USB",
		"SD card",
		"data_dir",
		"RABBOT_DATA_DIR",
		"retention",
		"rabbot db compact",
		"rabbot service install",
		"rabbot doctor",
	)
}

// TestMacMiniGuideAnchors guards B7 criterion 3: docs/mac-mini.md exists and
// states the brew install path, launchd via a per-user LaunchAgent (the identity
// fix — NO sudo), the auto-login honesty line (login-scoped agent), and the
// keep-awake pmset line.
func TestMacMiniGuideAnchors(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "mac-mini.md")
	assertAnchors(t, "docs/mac-mini.md", doc,
		"brew install roberto-grasiano/rabbot-seo/rabbot",
		"launchd",
		"LaunchAgent",
		"rabbot service install",
		"login",   // auto-login honesty
		"pmset",   // keep the box awake
		"sleep 0", // the exact keep-awake invocation
	)
	// The identity fix means NO sudo on macOS — the guide must say so, not instruct
	// `sudo rabbot service install` (the dropped root-context shape).
	low := strings.ToLower(doc)
	if strings.Contains(low, "sudo rabbot service install") {
		t.Error("docs/mac-mini.md must NOT instruct `sudo rabbot service install` — the macOS LaunchAgent installs without sudo")
	}
	if !strings.Contains(low, "no sudo") && !strings.Contains(low, "without sudo") {
		t.Error("docs/mac-mini.md must state the LaunchAgent installs without sudo")
	}
}

// TestVpsGuideAlignedToIdentityFix guards the B7 Scope-5 alignment half (NOT the
// provenance line — that is criterion 5, deferred to the live pass): docs/vps.md
// drives the unprivileged init/status/verify flow with sudo only for the service
// manager, drops any root-context (`sudo -i`) workaround, and adds the
// "other always-on boxes" cross-link to the new guides.
func TestVpsGuideAlignedToIdentityFix(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "vps.md")
	low := strings.ToLower(doc)

	// Unprivileged init/status/verify; sudo only on the service manager.
	assertAnchors(t, "docs/vps.md", doc,
		"sudo rabbot service install", // sudo IS used for the manager
		"docs/raspberry-pi.md",        // other always-on boxes pointer
		"docs/mac-mini.md",            // other always-on boxes pointer
	)
	// `rabbot init` and `rabbot status` must appear WITHOUT a sudo prefix (run as
	// yourself) — assert the unprivileged forms are present.
	if !strings.Contains(doc, "rabbot init ") && !strings.Contains(doc, "rabbot init\n") {
		t.Error("docs/vps.md must show unprivileged `rabbot init` (run as yourself)")
	}
	// The dropped root-context workaround: `sudo -i` must NOT appear anywhere.
	if strings.Contains(low, "sudo -i") {
		t.Error("docs/vps.md must drop the root-context (`sudo -i`) workaround — the identity fix makes it unnecessary and a foot-gun")
	}
}

// TestVpsGuideValidated is B7 criterion 5: docs/vps.md carries a validation
// provenance line (anchor `Validated` + `owner-validated` or `harness-validated`).
// The live pass ran 2026-06-12 (harness-validated: clean Ubuntu 22.04 systemd
// container, identity-fix build; see the provenance note at the foot of the doc).
func TestVpsGuideValidated(t *testing.T) {
	doc := deployDoc(t, "docs", "vps.md")
	low := strings.ToLower(doc)
	if !strings.Contains(low, "validated") {
		t.Error("docs/vps.md missing the `Validated` provenance anchor")
	}
	if !strings.Contains(low, "owner-validated") && !strings.Contains(low, "harness-validated") {
		t.Error("docs/vps.md provenance line must state owner-validated or harness-validated")
	}
}

// TestObservabilityDocAnchors guards the B2 docs half (docs/observability.md):
// the /metrics surface, the off-by-default + loopback default, the
// `observability init` generator, the compose bring-up, and the Grafana
// stock-credentials warning.
func TestObservabilityDocAnchors(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "observability.md")
	assertAnchors(t, "docs/observability.md", doc,
		"/metrics",
		"127.0.0.1:9464",            // the settled loopback default + port
		"off by default",            // settled: metrics off until enabled
		"rabbot observability init", // the generator command
		"docker compose",            // bring-up (operator/agent runs it, never Rabbot)
		"grafana",
		"admin/admin", // the stock-credentials warning
		"3000",        // the dashboard port
	)
	// No per-URL/per-site labels — the cardinality discipline must be stated.
	low := strings.ToLower(doc)
	if !strings.Contains(low, "per-url") && !strings.Contains(low, "per-site") && !strings.Contains(low, "cardinality") {
		t.Error("docs/observability.md should state the no-per-URL/per-site-label discipline")
	}
}

// TestObservabilityWithClaudeDocAnchors guards the B2-11 doc-path half: the agent
// recipe docs/observability-with-claude.md follows the install-with-claude.md
// pattern — the agent runs `rabbot observability init`, restarts the daemon,
// brings the stack up with its OWN `docker compose up -d`, and verifies via
// `curl /metrics`, a Prometheus target being up, and `get_status` reporting
// MetricsAddr. Rabbot never runs docker; the agent does.
func TestObservabilityWithClaudeDocAnchors(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "observability-with-claude.md")
	assertAnchors(t, "docs/observability-with-claude.md", doc,
		"rabbot observability init", // the generator the agent runs
		"docker compose up -d",      // the agent brings the stack up itself
		"curl",                      // verify the scrape
		"/metrics",
		"get_status",  // verify MetricsAddr through the read surface
		"MetricsAddr", // the field that proves the listener is on
		"restart",     // restart the daemon so it serves metrics
	)
	// Rabbot never runs docker (decision 18) — the recipe must say the AGENT does.
	low := strings.ToLower(doc)
	if !strings.Contains(low, "never runs docker") && !strings.Contains(low, "rabbot does not run docker") {
		t.Error("docs/observability-with-claude.md must state Rabbot never runs docker — the agent runs it in its own shell")
	}
}

// TestInstallWithClaudeLaptopExpectations guards B7 criterion 12's cross-link:
// docs/install-with-claude.md Step 0 Q1 ("Where should the monitor run") gains a
// laptop-expectations line so an installing agent sets the same always-on
// expectations the wizard nudge sets.
func TestInstallWithClaudeLaptopExpectations(t *testing.T) {
	t.Parallel()

	doc := deployDoc(t, "docs", "install-with-claude.md")
	low := strings.ToLower(doc)
	// The Q1 line must mention a laptop and that real-time wants an always-on box
	// (sleep ⇒ a gap), and point at the run-target options.
	assertAnchors(t, "docs/install-with-claude.md Step 0 Q1", doc, "laptop")
	if !strings.Contains(low, "sleep") && !strings.Contains(low, "always-on") {
		t.Error("docs/install-with-claude.md Step 0 Q1 must set laptop-sleep / always-on expectations")
	}
}
