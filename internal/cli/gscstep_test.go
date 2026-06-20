package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/wizard"
)

// The "Connect Search Console" menu step writes the per-site gsc block from the
// operator's choices, then offers the doctor connectivity check. The pure write path
// (configureGSCConnect) is unit-tested here; the huh.Select / huh.Input collectors
// and the live doctor probe are TTY/network seams exercised only by an integration
// `rabbot init`.

// newGSCTestCmd returns a cobra.Command wired to in/out/err buffers + a context, the
// shape configureGSCConnect consumes (mirrors the alerts step tests).
func newGSCTestCmd(out, errOut *bytes.Buffer) *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetIn(bytes.NewReader(nil))
	c.SetContext(context.Background())
	return c
}

// seedGSCConfig writes a config.yaml with one site so configureGSCConnect can write a
// gsc block onto it.
func seedGSCConfig(t *testing.T, siteURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("sites: []\n"), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := config.AddSiteYAML(path, config.SiteConfig{URL: siteURL}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}
	return path
}

func TestConfigureGSCConnect_ServiceAccountWritesBlock(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)

	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)

	// Stub the doctor-check offer so the unit test does no network; record that it was
	// offered with the right site.
	var checkedSite string
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, baseURL string) {
		checkedSite = baseURL
	}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	err := configureGSCConnect(cmd, path, site, gscConnectInput{
		mode:     wizard.GSCAuthService,
		property: "sc-domain:whatthehellai.com",
		credPath: "/etc/rabbot/sa-key.json",
	})
	if err != nil {
		t.Fatalf("configureGSCConnect: %v", err)
	}

	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gc, ok := cfg.GSCForBaseURL(site)
	if !ok {
		t.Fatal("no active GSC block written")
	}
	if gc.Property != "sc-domain:whatthehellai.com" || gc.Auth != config.GSCAuthServiceAccount ||
		gc.ServiceAccountKeyFile != "/etc/rabbot/sa-key.json" {
		t.Fatalf("gsc block not written as expected: %+v", gc)
	}
	if checkedSite != site {
		t.Fatalf("doctor check offered for %q, want %q", checkedSite, site)
	}
	// The success line must NOT echo the credential path body (only the property /
	// generic confirmation).
	if strings.Contains(out.String(), "/etc/rabbot/sa-key.json") {
		t.Fatalf("success output leaked the credential path:\n%s", out.String())
	}
}

func TestConfigureGSCConnect_OAuthWritesBlock(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) {}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	err := configureGSCConnect(cmd, path, site, gscConnectInput{
		mode:     wizard.GSCAuthOAuth,
		property: "https://example.com/",
		credPath: "/home/op/.config/rabbot/gsc-oauth.json",
	})
	if err != nil {
		t.Fatalf("configureGSCConnect: %v", err)
	}
	cfg, _ := config.Load(path, nil)
	gc, ok := cfg.GSCForBaseURL(site)
	if !ok || gc.Auth != config.GSCAuthOAuth2 || gc.OAuthTokenFile != "/home/op/.config/rabbot/gsc-oauth.json" {
		t.Fatalf("oauth gsc block not written: ok=%v %+v", ok, gc)
	}
	if gc.ServiceAccountKeyFile != "" {
		t.Fatalf("service_account_key_file should be empty for oauth2: %q", gc.ServiceAccountKeyFile)
	}
}

func TestConfigureGSCConnect_SkipWritesNothing(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	offered := false
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) { offered = true }
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	err := configureGSCConnect(cmd, path, site, gscConnectInput{mode: wizard.GSCAuthSkip})
	if err != nil {
		t.Fatalf("skip should not error: %v", err)
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("skip must write NO gsc block")
	}
	if offered {
		t.Fatal("skip must not run the doctor check")
	}
}

func TestConfigureGSCConnect_BadPropertyErrors(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) {}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	err := configureGSCConnect(cmd, path, site, gscConnectInput{
		mode:     wizard.GSCAuthService,
		property: "not-a-property",
		credPath: "/etc/rabbot/sa.json",
	})
	if err == nil {
		t.Fatal("a malformed property must surface an error")
	}
	// And it must NOT have written a (broken) block.
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("a failed build must not write a partial gsc block")
	}
}

func TestConfigureGSCConnect_NoSiteURLIsCleanNoOp(t *testing.T) {
	// If we somehow have no site URL to attach to (an edge the menu shouldn't hit),
	// the step must not crash and must write nothing.
	path := seedGSCConfig(t, "https://example.com")
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) {}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	if err := configureGSCConnect(cmd, path, "", gscConnectInput{
		mode: wizard.GSCAuthService, property: "https://example.com/", credPath: "/k.json",
	}); err != nil {
		t.Fatalf("empty site URL should be a clean skip, got: %v", err)
	}
}

// ── connectGSCUpgrade dispatch loop (the runConnectGSCUpgrade core) ───────────

// scriptedPrompter is a gscPrompter that returns pre-scripted answers and records
// which collectors were reached, so the dispatch loop's routing (connect → property →
// credPath) and abort short-circuits are asserted without a TTY.
type scriptedPrompter struct {
	mode     wizard.GSCAuthMode
	modeOK   bool
	property string
	propOK   bool
	credPath string
	credOK   bool

	calledMode int
	calledProp int
	calledCred int
	credMode   wizard.GSCAuthMode
}

func (s *scriptedPrompter) AuthMode() (wizard.GSCAuthMode, bool) {
	s.calledMode++
	return s.mode, s.modeOK
}

func (s *scriptedPrompter) Property() (string, bool) {
	s.calledProp++
	return s.property, s.propOK
}

func (s *scriptedPrompter) CredPath(mode wizard.GSCAuthMode) (string, bool) {
	s.calledCred++
	s.credMode = mode
	return s.credPath, s.credOK
}

// A full connect path collects mode → property → credPath and writes the block.
func TestConnectGSCUpgrade_HappyPath_WritesBlock(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) {}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	p := &scriptedPrompter{
		mode: wizard.GSCAuthService, modeOK: true,
		property: "sc-domain:whatthehellai.com", propOK: true,
		credPath: "/etc/rabbot/sa-key.json", credOK: true,
	}
	connectGSCUpgrade(cmd, path, site, p)

	if p.calledMode != 1 || p.calledProp != 1 || p.calledCred != 1 {
		t.Fatalf("collector call counts = (mode %d, prop %d, cred %d), want 1/1/1",
			p.calledMode, p.calledProp, p.calledCred)
	}
	if p.credMode != wizard.GSCAuthService {
		t.Fatalf("CredPath got mode %v, want the chosen GSCAuthService", p.credMode)
	}
	cfg, _ := config.Load(path, nil)
	gc, ok := cfg.GSCForBaseURL(site)
	if !ok || gc.Property != "sc-domain:whatthehellai.com" || gc.ServiceAccountKeyFile != "/etc/rabbot/sa-key.json" {
		t.Fatalf("connect path did not write the expected gsc block: ok=%v %+v", ok, gc)
	}
}

// An abort at the auth-mode select is a quiet skip: no property/cred collection, no
// block written.
func TestConnectGSCUpgrade_AbortAtMode_QuietSkip(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)

	p := &scriptedPrompter{modeOK: false}
	connectGSCUpgrade(cmd, path, site, p)

	if p.calledProp != 0 || p.calledCred != 0 {
		t.Fatalf("a mode abort must not reach property/cred collectors (prop %d cred %d)", p.calledProp, p.calledCred)
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("a mode abort must write no gsc block")
	}
}

// The deliberate Skip mode is a connect=false terminal state: it does NOT collect a
// property/cred and writes no block, but prints the lossless-skip acknowledgment.
func TestConnectGSCUpgrade_SkipMode_NoCollectNoBlock(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)

	p := &scriptedPrompter{mode: wizard.GSCAuthSkip, modeOK: true}
	connectGSCUpgrade(cmd, path, site, p)

	if p.calledProp != 0 || p.calledCred != 0 {
		t.Fatalf("skip mode must not collect property/cred (prop %d cred %d)", p.calledProp, p.calledCred)
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("skip mode must write no gsc block")
	}
	if !strings.Contains(out.String(), wizard.GSCSkipAcknowledged) {
		t.Fatalf("skip mode must print the lossless-skip acknowledgment, got:\n%s", out.String())
	}
}

// An abort at the property prompt (after choosing a connect mode) short-circuits
// before the cred prompt and writes nothing.
func TestConnectGSCUpgrade_AbortAtProperty_QuietSkip(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)

	p := &scriptedPrompter{mode: wizard.GSCAuthService, modeOK: true, propOK: false}
	connectGSCUpgrade(cmd, path, site, p)

	if p.calledProp != 1 || p.calledCred != 0 {
		t.Fatalf("property abort must short-circuit before cred (prop %d cred %d)", p.calledProp, p.calledCred)
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("property abort must write no gsc block")
	}
}

// An abort at the credential prompt writes nothing.
func TestConnectGSCUpgrade_AbortAtCred_QuietSkip(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)

	p := &scriptedPrompter{
		mode: wizard.GSCAuthOAuth, modeOK: true,
		property: "https://example.com/", propOK: true,
		credOK: false,
	}
	connectGSCUpgrade(cmd, path, site, p)

	if p.calledCred != 1 {
		t.Fatalf("cred collector should be reached once, got %d", p.calledCred)
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("cred abort must write no gsc block")
	}
}

// A configure error (a malformed property survives the prompt seam — the live form
// validates, but the dispatch loop must still be defensive) is surfaced to stderr as
// an advisory line, never a crash, and writes no block.
func TestConnectGSCUpgrade_ConfigureError_AdvisoryNoBlock(t *testing.T) {
	const site = "https://example.com"
	path := seedGSCConfig(t, site)
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	gscDoctorOfferFn = func(_ *cobra.Command, _ string, _ string) {}
	t.Cleanup(func() { gscDoctorOfferFn = offerGSCDoctorCheck })

	p := &scriptedPrompter{
		mode: wizard.GSCAuthService, modeOK: true,
		property: "not-a-valid-property", propOK: true,
		credPath: "/k.json", credOK: true,
	}
	connectGSCUpgrade(cmd, path, site, p)

	if !strings.Contains(errOut.String(), "skipping the Search Console step") {
		t.Fatalf("a configure error must surface an advisory line, got stderr:\n%s", errOut.String())
	}
	cfg, _ := config.Load(path, nil)
	if _, ok := cfg.GSCForBaseURL(site); ok {
		t.Fatal("a configure error must leave no gsc block")
	}
}

// ── dispatchGSCDoctorCheck (the offer-step core) ──────────────────────────────

// run=false prints the skip hint and never runs the probe.
func TestDispatchGSCDoctorCheck_Skip_PrintsHintNoProbe(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	probed := false
	dispatchGSCDoctorCheck(cmd, "https://example.com", false, func() error { probed = true; return nil })

	if probed {
		t.Fatal("run=false must not invoke the probe")
	}
	if !strings.Contains(out.String(), "rabbot doctor https://example.com") {
		t.Fatalf("skip must print a re-run-later hint naming the URL, got:\n%s", out.String())
	}
}

// run=true invokes the probe; a nil error prints nothing on stderr.
func TestDispatchGSCDoctorCheck_Run_InvokesProbe(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	probed := false
	dispatchGSCDoctorCheck(cmd, "https://example.com", true, func() error { probed = true; return nil })

	if !probed {
		t.Fatal("run=true must invoke the probe")
	}
	if errOut.String() != "" {
		t.Fatalf("a successful probe must not warn, got stderr:\n%s", errOut.String())
	}
}

// A probe WRITE error is advisory: surfaced to stderr, never fatal.
func TestDispatchGSCDoctorCheck_ProbeError_Advisory(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := newGSCTestCmd(&out, &errOut)
	dispatchGSCDoctorCheck(cmd, "https://example.com", true, func() error {
		return errTestDoctorProbe
	})
	if !strings.Contains(errOut.String(), "could not render the Search Console check") {
		t.Fatalf("a probe error must surface an advisory line, got stderr:\n%s", errOut.String())
	}
}

var errTestDoctorProbe = errTestDoctorProbeT{}

type errTestDoctorProbeT struct{}

func (errTestDoctorProbeT) Error() string { return "probe write failed" }

// ── resolveGSCAuthChoice / gscCredPrompt (prompt-helper cores) ────────────────

func TestResolveGSCAuthChoice(t *testing.T) {
	cases := []struct {
		value    string
		wantMode wizard.GSCAuthMode
		wantOK   bool
	}{
		{config.GSCAuthServiceAccount, wizard.GSCAuthService, true},
		{config.GSCAuthOAuth2, wizard.GSCAuthOAuth, true},
		{"skip", wizard.GSCAuthSkip, true},
		{"nonsense", wizard.GSCAuthUnset, false},
		{"", wizard.GSCAuthUnset, false},
	}
	for _, c := range cases {
		mode, ok := resolveGSCAuthChoice(c.value)
		if mode != c.wantMode || ok != c.wantOK {
			t.Errorf("resolveGSCAuthChoice(%q) = (%v,%v), want (%v,%v)", c.value, mode, ok, c.wantMode, c.wantOK)
		}
	}
}

// gscCredPrompt selects mode-specific copy AND emits the matching help to the writer.
func TestGSCCredPrompt(t *testing.T) {
	t.Run("oauth", func(t *testing.T) {
		var buf bytes.Buffer
		title, placeholder := gscCredPrompt(&buf, wizard.GSCAuthOAuth)
		if !strings.Contains(title, "OAuth token file") {
			t.Errorf("oauth title = %q, want it to mention the OAuth token file", title)
		}
		if !strings.Contains(placeholder, "gsc-oauth.json") {
			t.Errorf("oauth placeholder = %q", placeholder)
		}
		if buf.Len() == 0 || buf.String() != wizard.GSCOAuthCredHelp+"\n" {
			t.Errorf("oauth help not emitted, got %q", buf.String())
		}
	})
	t.Run("service-account-default", func(t *testing.T) {
		var buf bytes.Buffer
		title, placeholder := gscCredPrompt(&buf, wizard.GSCAuthService)
		if !strings.Contains(title, "service-account JSON key") {
			t.Errorf("SA title = %q, want it to mention the service-account JSON key", title)
		}
		if !strings.Contains(placeholder, "gsc-sa.json") {
			t.Errorf("SA placeholder = %q", placeholder)
		}
		if buf.String() != wizard.GSCServiceAccountCredHelp+"\n" {
			t.Errorf("SA help not emitted, got %q", buf.String())
		}
	})
}
