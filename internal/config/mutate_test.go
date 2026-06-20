package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// seedConfig writes a config.yaml that exercises a leading comment, a nested
// control.port key, and an empty sites sequence — the surface the mutators must
// preserve across edits.
func seedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const seed = `# rabbot config — keep this comment
control:
  port: 7777
sites: []
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	return path
}

// readDoc unmarshals the file into a permissive struct for assertions.
type testDoc struct {
	Control struct {
		Port int `yaml:"port"`
	} `yaml:"control"`
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
	Sites []struct {
		URL         string `yaml:"url"`
		Name        string `yaml:"name"`
		MinInterval string `yaml:"min_interval"`
		MaxInterval string `yaml:"max_interval"`
		Speed       int    `yaml:"speed"`
	} `yaml:"sites"`
}

func readDoc(t *testing.T, path string) testDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var d testDoc
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return d
}

func TestAddSiteYAML_AppendsAndPreserves(t *testing.T) {
	path := seedConfig(t)

	err := AddSiteYAML(path, SiteConfig{
		URL:         "https://example.com",
		Name:        "Example",
		MinInterval: "5m",
		MaxInterval: "1h",
		Speed:       50,
	})
	if err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}

	d := readDoc(t, path)
	if len(d.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(d.Sites))
	}
	s := d.Sites[0]
	if s.URL != "https://example.com" || s.Name != "Example" ||
		s.MinInterval != "5m" || s.MaxInterval != "1h" || s.Speed != 50 {
		t.Fatalf("site fields not round-tripped: %+v", s)
	}

	// Unrelated key and comment survive.
	if d.Control.Port != 7777 {
		t.Fatalf("control.port lost: %d", d.Control.Port)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost:\n%s", raw)
	}
}

func TestSetSiteVerificationYAML(t *testing.T) {
	path := seedConfig(t)
	// Two sites + an unrelated top-level comment must survive verbatim.
	if err := AddSiteYAML(path, SiteConfig{URL: "https://one.test", Name: "One"}); err != nil {
		t.Fatalf("AddSiteYAML one: %v", err)
	}
	if err := AddSiteYAML(path, SiteConfig{URL: "https://two.test", Name: "Two"}); err != nil {
		t.Fatalf("AddSiteYAML two: %v", err)
	}

	found, err := SetSiteVerificationYAML(path, "https://one.test", VerificationConfig{
		Method:     "well_known",
		Token:      "rab_TOKENONE",
		VerifiedAt: "2026-06-05T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("SetSiteVerificationYAML: %v", err)
	}
	if !found {
		t.Fatal("SetSiteVerificationYAML found = false, want true")
	}

	dir := filepath.Dir(path)
	cfg, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Locate site one and assert its verification block.
	var got *VerificationConfig
	for i := range cfg.Sites {
		if cfg.Sites[i].URL == "https://one.test" {
			got = &cfg.Sites[i].Verification
		}
	}
	if got == nil {
		t.Fatal("site one missing after edit")
	}
	if got.Method != "well_known" || got.Token != "rab_TOKENONE" || got.VerifiedAt != "2026-06-05T12:00:00Z" {
		t.Fatalf("verification not round-tripped: %+v", got)
	}
	// The second site survives.
	if len(cfg.Sites) != 2 {
		t.Fatalf("want 2 sites, got %d", len(cfg.Sites))
	}
	// The leading comment survives.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost:\n%s", raw)
	}
	// Site two's url is still present verbatim.
	if !strings.Contains(string(raw), "https://two.test") {
		t.Fatalf("second site lost:\n%s", raw)
	}
}

// TestSetSiteVerificationYAMLClearsStaleVerifiedAt covers issue #35: after a
// verified intent (verified_at written) is followed by a non-verified intent
// (VerifiedAt == ""), the stale verified_at must be actively REMOVED from the
// site's verification mapping rather than left behind. The DB is authoritative,
// but a stale config.verified_at misleads anyone reading config.yaml directly.
func TestSetSiteVerificationYAMLClearsStaleVerifiedAt(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://one.test", Name: "One"}); err != nil {
		t.Fatalf("AddSiteYAML one: %v", err)
	}

	// 1) Verified intent: method + token + verified_at all written.
	found, err := SetSiteVerificationYAML(path, "https://one.test", VerificationConfig{
		Method:     "well_known",
		Token:      "rab_TOKENONE",
		VerifiedAt: "2026-06-05T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("SetSiteVerificationYAML verified: %v", err)
	}
	if !found {
		t.Fatal("verified intent found = false, want true")
	}

	// 2) Throttled / non-verified intent: same method+token, but VerifiedAt == "".
	found, err = SetSiteVerificationYAML(path, "https://one.test", VerificationConfig{
		Method: "well_known",
		Token:  "rab_TOKENONE",
	})
	if err != nil {
		t.Fatalf("SetSiteVerificationYAML throttled: %v", err)
	}
	if !found {
		t.Fatal("throttled intent found = false, want true")
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got *VerificationConfig
	for i := range cfg.Sites {
		if cfg.Sites[i].URL == "https://one.test" {
			got = &cfg.Sites[i].Verification
		}
	}
	if got == nil {
		t.Fatal("site one missing after edits")
	}
	if got.VerifiedAt != "" {
		t.Fatalf("stale verified_at not cleared: %q", got.VerifiedAt)
	}
	// Method (and token) are untouched by the clear.
	if got.Method != "well_known" {
		t.Fatalf("method clobbered by clear: %q", got.Method)
	}
	if got.Token != "rab_TOKENONE" {
		t.Fatalf("token clobbered by clear: %q", got.Token)
	}

	// The verified_at key must be gone from the raw file (not merely empty),
	// and the leading comment + structure must survive.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "verified_at") {
		t.Fatalf("verified_at key still present in file:\n%s", raw)
	}
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost:\n%s", raw)
	}
	if !strings.Contains(string(raw), "method: well_known") {
		t.Fatalf("method line lost:\n%s", raw)
	}
}

func TestSetSiteVerificationYAMLSiteAbsent(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://present.test"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}
	found, err := SetSiteVerificationYAML(path, "https://missing.test", VerificationConfig{Method: "dns", Token: "rab_x"})
	if err != nil {
		t.Fatalf("SetSiteVerificationYAML: %v", err)
	}
	if found {
		t.Fatal("found = true for a site that does not exist, want false")
	}
}

func TestAddSiteYAML_OmitsZeroFields(t *testing.T) {
	path := seedConfig(t)

	if err := AddSiteYAML(path, SiteConfig{URL: "https://only-url.test"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "url: https://only-url.test") {
		t.Fatalf("url not written:\n%s", out)
	}
	// Zero-valued optional fields must be omitted entirely.
	for _, k := range []string{"name:", "min_interval:", "max_interval:", "speed:"} {
		if strings.Contains(out, k) {
			t.Fatalf("zero field %q should be omitted:\n%s", k, out)
		}
	}
}

func TestAddSiteYAML_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.yaml")

	if err := AddSiteYAML(path, SiteConfig{URL: "https://fresh.test", Name: "Fresh"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}

	d := readDoc(t, path)
	if len(d.Sites) != 1 || d.Sites[0].URL != "https://fresh.test" {
		t.Fatalf("fresh file not seeded with site: %+v", d.Sites)
	}
}

func TestAddSiteYAML_AppendsToExistingSequence(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://a.test"}); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := AddSiteYAML(path, SiteConfig{URL: "https://b.test"}); err != nil {
		t.Fatalf("add b: %v", err)
	}
	d := readDoc(t, path)
	if len(d.Sites) != 2 {
		t.Fatalf("want 2 sites, got %d: %+v", len(d.Sites), d.Sites)
	}
	if d.Sites[0].URL != "https://a.test" || d.Sites[1].URL != "https://b.test" {
		t.Fatalf("order not preserved: %+v", d.Sites)
	}
}

// TestAddNotifierYAMLRoundTrip asserts a Slack notifier is appended to the
// top-level notifiers sequence and round-trips through config.Load verbatim,
// including the webhook URL value (which IS the secret for an Incoming Webhook).
func TestAddNotifierYAMLRoundTrip(t *testing.T) {
	path := seedConfig(t)

	const webhook = "https://hooks.slack.com/services/T/B/XXX"
	if err := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: webhook}); err != nil {
		t.Fatalf("AddNotifierYAML: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("want 1 notifier, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	n := cfg.Notifiers[0]
	if n.Name != "slack" || n.Type != "slack-webhook" || n.URL != webhook {
		t.Fatalf("notifier fields not round-tripped: %+v", n)
	}
}

// TestAddNotifierYAMLPreservesEnvInterp asserts that an ${ENV} interpolation
// token is written to disk LITERALLY (not expanded at write time) so koanf can
// expand it from the environment at Load. This is how the webhook secret stays
// out of the file on disk.
func TestAddNotifierYAMLPreservesEnvInterp(t *testing.T) {
	path := seedConfig(t)

	const token = "${RABBOT_SLACK_WEBHOOK}"
	if err := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: token}); err != nil {
		t.Fatalf("AddNotifierYAML: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), token) {
		t.Fatalf("env-interpolation token not preserved literally on disk:\n%s", raw)
	}
}

// TestAddNotifierYAMLPreservesComments seeds a config with an unrelated top-level
// key, a leading comment, and an existing notifier, then appends a second
// notifier. Both notifiers must be present, the comment must survive, and
// unrelated keys must be untouched.
func TestAddNotifierYAMLPreservesComments(t *testing.T) {
	path := seedConfig(t)

	if err := AddNotifierYAML(path, NotifierConfig{Name: "first", Type: "slack-webhook", URL: "https://hooks.slack.com/services/A"}); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if err := AddNotifierYAML(path, NotifierConfig{Name: "second", Type: "slack-webhook", URL: "https://hooks.slack.com/services/B"}); err != nil {
		t.Fatalf("add second: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 2 {
		t.Fatalf("want 2 notifiers, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	if cfg.Notifiers[0].Name != "first" || cfg.Notifiers[1].Name != "second" {
		t.Fatalf("notifier order/names not preserved: %+v", cfg.Notifiers)
	}
	// Unrelated key survives.
	if cfg.Control.Port != 7777 {
		t.Fatalf("control.port lost: %d", cfg.Control.Port)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost:\n%s", raw)
	}
}

// TestAddNotifierYAMLPromotesNullSeq exercises the seq-promotion branches: a
// "notifiers:" null value and a "notifiers: []" flow sequence must both accept
// the append and render as a block-style sequence (mirrors AddSiteYAML).
func TestAddNotifierYAMLPromotesNullSeq(t *testing.T) {
	cases := []struct {
		name string
		seed string
	}{
		{"null", "notifiers:\n"},
		{"flow-empty", "notifiers: []\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.seed), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: "https://hooks.slack.com/x"}); err != nil {
				t.Fatalf("AddNotifierYAML: %v", err)
			}
			cfg, err := Load(path, nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Notifiers) != 1 || cfg.Notifiers[0].Name != "slack" {
				t.Fatalf("append failed: %+v", cfg.Notifiers)
			}
			// Block style: the appended mapping renders line-per-key (a leading
			// "- name:" or "- url:") rather than the inline flow "[{...}]".
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "[{") {
				t.Fatalf("notifier rendered in flow style, want block:\n%s", raw)
			}
		})
	}
}

// TestAddNotifierYAMLDoesNotLogWebhook is the load-bearing secret guard: the
// writer must never print/log the webhook URL. We capture process stdout+stderr
// around the call (config has no injectable logger seam) and assert the sentinel
// secret never appears there — while it DOES appear on disk, proving the value
// was persisted and only the LOGS are clean.
func TestAddNotifierYAMLDoesNotLogWebhook(t *testing.T) {
	path := seedConfig(t)
	const webhook = "https://hooks.slack.com/services/SECRET-DO-NOT-LEAK"

	// Swap process stdout+stderr for a pipe so we capture anything the writer
	// might emit. config has no logger seam, so capture at the OS level.
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe out: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe err: %v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr

	callErr := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: webhook})

	// Restore and drain.
	os.Stdout, os.Stderr = origOut, origErr
	_ = wOut.Close()
	_ = wErr.Close()
	var outBuf, errBuf bytes.Buffer
	_, _ = outBuf.ReadFrom(rOut)
	_, _ = errBuf.ReadFrom(rErr)

	if callErr != nil {
		t.Fatalf("AddNotifierYAML: %v", callErr)
	}
	if strings.Contains(outBuf.String(), "SECRET-DO-NOT-LEAK") {
		t.Fatalf("webhook leaked to stdout:\n%s", outBuf.String())
	}
	if strings.Contains(errBuf.String(), "SECRET-DO-NOT-LEAK") {
		t.Fatalf("webhook leaked to stderr:\n%s", errBuf.String())
	}
	// Distinguish "didn't write it" from "wrote it but didn't log it": the value
	// MUST be on disk.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "SECRET-DO-NOT-LEAK") {
		t.Fatalf("webhook was not persisted to disk:\n%s", raw)
	}
}

// TestAddNotifierYAMLIdempotentByName pins the re-run guarantee: appending a
// notifier whose name already exists must replace it in place (not append a
// duplicate), so re-running `rabbot init --slack-webhook X` never grows the
// notifiers list. The replacement also picks up an updated URL/type.
func TestAddNotifierYAMLIdempotentByName(t *testing.T) {
	path := seedConfig(t)

	if err := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: "https://hooks.slack.com/services/OLD"}); err != nil {
		t.Fatalf("add first: %v", err)
	}
	// Re-run with the same name but a new URL: must replace, not append.
	if err := AddNotifierYAML(path, NotifierConfig{Name: "slack", Type: "slack-webhook", URL: "https://hooks.slack.com/services/NEW"}); err != nil {
		t.Fatalf("add second: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Notifiers) != 1 {
		t.Fatalf("re-run appended a duplicate notifier; want 1, got %d: %+v", len(cfg.Notifiers), cfg.Notifiers)
	}
	if cfg.Notifiers[0].URL != "https://hooks.slack.com/services/NEW" {
		t.Fatalf("re-run did not update the URL in place: %+v", cfg.Notifiers[0])
	}
	// Comment/unrelated keys survive the in-place replace.
	if cfg.Control.Port != 7777 {
		t.Fatalf("control.port lost: %d", cfg.Control.Port)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost across idempotent replace:\n%s", raw)
	}
}

// TestAddRouteYAMLRoundTrip pins that AddRouteYAML appends a route to the
// top-level "routes" sequence and the loaded config exposes it. A fallback route
// carries an empty Match map and the notifier name.
func TestAddRouteYAMLRoundTrip(t *testing.T) {
	path := seedConfig(t)

	if err := AddRouteYAML(path, RouteConfig{Notifier: "slack"}); err != nil {
		t.Fatalf("AddRouteYAML: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("want 1 route, got %d: %+v", len(cfg.Routes), cfg.Routes)
	}
	if cfg.Routes[0].Notifier != "slack" {
		t.Fatalf("route notifier not round-tripped: %+v", cfg.Routes[0])
	}
	if len(cfg.Routes[0].Match) != 0 {
		t.Fatalf("fallback route must have an empty Match map: %+v", cfg.Routes[0])
	}
	// Unrelated key survives.
	if cfg.Control.Port != 7777 {
		t.Fatalf("control.port lost: %d", cfg.Control.Port)
	}
}

// TestAddRouteYAMLPromotesNullSeq exercises the seq-promotion branches: a null
// "routes:" value and a "routes: []" flow sequence must both accept the append
// and render as a block-style sequence (mirrors AddNotifierYAML).
func TestAddRouteYAMLPromotesNullSeq(t *testing.T) {
	cases := []struct {
		name string
		seed string
	}{
		{"null", "routes:\n"},
		{"flow-empty", "routes: []\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.seed), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := AddRouteYAML(path, RouteConfig{Notifier: "slack"}); err != nil {
				t.Fatalf("AddRouteYAML: %v", err)
			}
			cfg, err := Load(path, nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Routes) != 1 || cfg.Routes[0].Notifier != "slack" {
				t.Fatalf("append failed: %+v", cfg.Routes)
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "[{") {
				t.Fatalf("route rendered in flow style, want block:\n%s", raw)
			}
		})
	}
}

// TestAddRouteYAMLIdempotentByNotifier pins that re-adding a fallback route for a
// notifier that already has one does not append a duplicate, so a re-run of the
// onboarding alerts step keeps exactly one route.
func TestAddRouteYAMLIdempotentByNotifier(t *testing.T) {
	path := seedConfig(t)

	if err := AddRouteYAML(path, RouteConfig{Notifier: "slack"}); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if err := AddRouteYAML(path, RouteConfig{Notifier: "slack"}); err != nil {
		t.Fatalf("add second: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("re-run appended a duplicate route; want 1, got %d: %+v", len(cfg.Routes), cfg.Routes)
	}
}

func TestSetKeyYAML_StringLeaf(t *testing.T) {
	path := seedConfig(t)

	if err := SetKeyYAML(path, "log.level", "debug"); err != nil {
		t.Fatalf("SetKeyYAML: %v", err)
	}
	d := readDoc(t, path)
	if d.Log.Level != "debug" {
		t.Fatalf("log.level not set: %q", d.Log.Level)
	}
	// Existing keys/comment untouched.
	if d.Control.Port != 7777 {
		t.Fatalf("control.port lost: %d", d.Control.Port)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("comment lost:\n%s", raw)
	}
}

func TestSetKeyYAML_IntLeafIsInteger(t *testing.T) {
	path := seedConfig(t)

	if err := SetKeyYAML(path, "control.port", "9001"); err != nil {
		t.Fatalf("SetKeyYAML: %v", err)
	}

	// Assert it round-trips as an int (not a quoted string).
	d := readDoc(t, path)
	if d.Control.Port != 9001 {
		t.Fatalf("control.port not int 9001: %d", d.Control.Port)
	}
	raw, _ := os.ReadFile(path)
	out := string(raw)
	if strings.Contains(out, `"9001"`) || strings.Contains(out, "'9001'") {
		t.Fatalf("port written as quoted string:\n%s", out)
	}
	if !strings.Contains(out, "port: 9001") {
		t.Fatalf("port not written as bare int:\n%s", out)
	}
}

func TestSetKeyYAML_BoolLeaf(t *testing.T) {
	path := seedConfig(t)

	if err := SetKeyYAML(path, "crawler.egress_check_enabled", "false"); err != nil {
		t.Fatalf("SetKeyYAML: %v", err)
	}
	raw, _ := os.ReadFile(path)
	out := string(raw)
	if !strings.Contains(out, "egress_check_enabled: false") {
		t.Fatalf("bool not written bare:\n%s", out)
	}
	if strings.Contains(out, `"false"`) || strings.Contains(out, "'false'") {
		t.Fatalf("bool written as quoted string:\n%s", out)
	}
}

func TestSetKeyYAML_CreatesNestedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.yaml")
	if err := os.WriteFile(path, []byte("# top comment\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SetKeyYAML(path, "alerting.digest.schedule", "30m"); err != nil {
		t.Fatalf("SetKeyYAML deep: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Alerting struct {
			Digest struct {
				Schedule string `yaml:"schedule"`
			} `yaml:"digest"`
		} `yaml:"alerting"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got.Alerting.Digest.Schedule != "30m" {
		t.Fatalf("deep key not set:\n%s", raw)
	}
	if !strings.Contains(string(raw), "# top comment") {
		t.Fatalf("comment lost:\n%s", raw)
	}
}

func TestRemoveSiteYAML_RemovesMatch(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://keep.test", Name: "Keep"}); err != nil {
		t.Fatalf("add keep: %v", err)
	}
	if err := AddSiteYAML(path, SiteConfig{URL: "https://drop.test", Name: "Drop"}); err != nil {
		t.Fatalf("add drop: %v", err)
	}

	found, err := RemoveSiteYAML(path, "https://drop.test")
	if err != nil {
		t.Fatalf("RemoveSiteYAML: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}

	d := readDoc(t, path)
	if len(d.Sites) != 1 || d.Sites[0].URL != "https://keep.test" {
		t.Fatalf("wrong site removed: %+v", d.Sites)
	}
	// Unrelated comment still present.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("comment lost:\n%s", raw)
	}
}

func TestRemoveSiteYAML_NoMatchNotError(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://present.test"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	found, err := RemoveSiteYAML(path, "https://absent.test")
	if err != nil {
		t.Fatalf("RemoveSiteYAML: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for absent url")
	}
	d := readDoc(t, path)
	if len(d.Sites) != 1 {
		t.Fatalf("site count changed unexpectedly: %d", len(d.Sites))
	}
}

func TestRemoveSiteYAML_EmptySites(t *testing.T) {
	path := seedConfig(t)
	found, err := RemoveSiteYAML(path, "https://anything.test")
	if err != nil {
		t.Fatalf("RemoveSiteYAML on empty: %v", err)
	}
	if found {
		t.Fatalf("expected found=false on empty sites")
	}
}

// TestWriteDocRoot_DurableOverwrite exercises the crash-atomic write path when
// the target already exists: the temp+sync+rename dance must replace the file
// with the new, complete content, leave no stray temp file in the directory,
// and keep owner-only (0600) perms. This guards the F38 fix (fsync of the temp
// file before close + parent-dir fsync after rename) by confirming the write
// path still produces correct, complete output after those syncs are added —
// the fsync calls themselves cannot be fault-injected from a normal test, but a
// botched Sync ordering (e.g. syncing a closed fd) would surface here as a
// write error or truncated content.
func TestWriteDocRoot_DurableOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Pre-existing file with 0644 perms — writeDocRoot must tighten to 0600.
	if err := os.WriteFile(path, []byte("control:\n  port: 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root := newMappingNode()
	ctrl := newMappingNode()
	setMapValue(ctrl, "port", scalarNode("!!int", "9999"))
	setMapValue(root, "control", ctrl)

	if err := writeDocRoot(path, root); err != nil {
		t.Fatalf("writeDocRoot: %v", err)
	}

	// New content is complete and replaced the old.
	d := readDoc(t, path)
	if d.Control.Port != 9999 {
		t.Fatalf("overwrite content wrong: port=%d", d.Control.Port)
	}

	// Perms tightened to owner-only even though the prior file was 0644.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows maps NTFS attrs onto Go's mode bits, so an exact 0600
	// assertion can't hold there; assert perms only where they exist.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("want 0600, got %o", perm)
		}
	}

	// No temp file leaked into the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".rabbot-config-") {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
}

// Full round-trip through config.Load to ensure mutated files stay loadable.
func TestMutators_LoadableAfterEdits(t *testing.T) {
	path := seedConfig(t)
	if err := SetKeyYAML(path, "crawler.contact_email", "ops@example.com"); err != nil {
		t.Fatalf("set contact_email: %v", err)
	}
	if err := AddSiteYAML(path, SiteConfig{URL: "https://loadable.test", Name: "Loadable", Speed: 75}); err != nil {
		t.Fatalf("add site: %v", err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load after edits: %v", err)
	}
	if cfg.Control.Port != 7777 {
		t.Fatalf("control.port lost through Load: %d", cfg.Control.Port)
	}
	if len(cfg.Sites) != 1 || cfg.Sites[0].URL != "https://loadable.test" || cfg.Sites[0].Speed != 75 {
		t.Fatalf("sites not loaded correctly: %+v", cfg.Sites)
	}
	if cfg.Crawler.ContactEmail != "ops@example.com" {
		t.Fatalf("contact_email not loaded: %q", cfg.Crawler.ContactEmail)
	}
}

// TestSetSiteGSCYAML covers the per-site GSC block writer: it sets gsc.{property,
// auth,service_account_key_file} on the matching site, preserves sibling sites and
// comments, and round-trips through Load + ValidateGSC. The credential field holds a
// PATH only (never a body); the writer must store it verbatim.
func TestSetSiteGSCYAML(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://one.test", Name: "One"}); err != nil {
		t.Fatalf("AddSiteYAML one: %v", err)
	}
	if err := AddSiteYAML(path, SiteConfig{URL: "https://two.test", Name: "Two"}); err != nil {
		t.Fatalf("AddSiteYAML two: %v", err)
	}

	found, err := SetSiteGSCYAML(path, "https://one.test", GSCConfig{
		Property:              "sc-domain:whatthehellai.com",
		Auth:                  GSCAuthServiceAccount,
		ServiceAccountKeyFile: "/etc/rabbot/sa-key.json",
	})
	if err != nil {
		t.Fatalf("SetSiteGSCYAML: %v", err)
	}
	if !found {
		t.Fatal("SetSiteGSCYAML found = false, want true")
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gc, ok := cfg.GSCForBaseURL("https://one.test")
	if !ok {
		t.Fatal("site one has no active GSC block after write")
	}
	if gc.Property != "sc-domain:whatthehellai.com" || gc.Auth != GSCAuthServiceAccount ||
		gc.ServiceAccountKeyFile != "/etc/rabbot/sa-key.json" {
		t.Fatalf("gsc block not round-tripped: %+v", gc)
	}
	if gc.OAuthTokenFile != "" {
		t.Fatalf("oauth_token_file should be empty for a service_account write: %q", gc.OAuthTokenFile)
	}
	// The assembled config must pass the real validator.
	if err := ValidateGSC(cfg.Sites); err != nil {
		t.Fatalf("written GSC block fails ValidateGSC: %v", err)
	}
	// Sibling site + leading comment survive verbatim.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "https://two.test") {
		t.Fatalf("second site lost:\n%s", raw)
	}
	if !strings.Contains(string(raw), "# rabbot config — keep this comment") {
		t.Fatalf("leading comment lost:\n%s", raw)
	}
}

// TestSetSiteGSCYAMLOAuth writes the oauth2 variant and asserts only the token-file
// path is set (mutual exclusion with the service-account key file).
func TestSetSiteGSCYAMLOAuth(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://one.test"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}
	found, err := SetSiteGSCYAML(path, "https://one.test", GSCConfig{
		Property:       "https://one.test/",
		Auth:           GSCAuthOAuth2,
		OAuthTokenFile: "/home/op/.config/rabbot/gsc-oauth.json",
	})
	if err != nil {
		t.Fatalf("SetSiteGSCYAML: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gc, ok := cfg.GSCForBaseURL("https://one.test")
	if !ok {
		t.Fatal("no active GSC block after write")
	}
	if gc.Auth != GSCAuthOAuth2 || gc.OAuthTokenFile != "/home/op/.config/rabbot/gsc-oauth.json" {
		t.Fatalf("oauth gsc block not round-tripped: %+v", gc)
	}
	if gc.ServiceAccountKeyFile != "" {
		t.Fatalf("service_account_key_file should be empty for an oauth2 write: %q", gc.ServiceAccountKeyFile)
	}
}

// TestSetSiteGSCYAMLSiteAbsent: a missing site URL is a found=false, nil-error no-op.
func TestSetSiteGSCYAMLSiteAbsent(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://present.test"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}
	found, err := SetSiteGSCYAML(path, "https://missing.test", GSCConfig{
		Property: "https://missing.test/", Auth: GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json",
	})
	if err != nil {
		t.Fatalf("SetSiteGSCYAML: %v", err)
	}
	if found {
		t.Fatal("found = true for an absent site, want false")
	}
}

// TestSetSiteGSCYAMLReplacesInPlace: a second write to the same site replaces the
// block in place (re-running the wizard never duplicates the gsc mapping), and
// switching modes clears the now-irrelevant credential key.
func TestSetSiteGSCYAMLReplacesInPlace(t *testing.T) {
	path := seedConfig(t)
	if err := AddSiteYAML(path, SiteConfig{URL: "https://one.test"}); err != nil {
		t.Fatalf("AddSiteYAML: %v", err)
	}
	if _, err := SetSiteGSCYAML(path, "https://one.test", GSCConfig{
		Property: "https://one.test/", Auth: GSCAuthServiceAccount, ServiceAccountKeyFile: "/etc/rabbot/sa.json",
	}); err != nil {
		t.Fatalf("first SetSiteGSCYAML: %v", err)
	}
	// Re-write as oauth2: the service_account_key_file must be GONE so the block
	// validates (mutual exclusion) and there is exactly one gsc mapping.
	if _, err := SetSiteGSCYAML(path, "https://one.test", GSCConfig{
		Property: "https://one.test/", Auth: GSCAuthOAuth2, OAuthTokenFile: "/t.json",
	}); err != nil {
		t.Fatalf("second SetSiteGSCYAML: %v", err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gc, _ := cfg.GSCForBaseURL("https://one.test")
	if gc.Auth != GSCAuthOAuth2 || gc.ServiceAccountKeyFile != "" || gc.OAuthTokenFile != "/t.json" {
		t.Fatalf("mode switch did not clear stale credential: %+v", gc)
	}
	if err := ValidateGSC(cfg.Sites); err != nil {
		t.Fatalf("post-switch block fails ValidateGSC: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "gsc:"); n != 1 {
		t.Fatalf("want exactly one gsc mapping, got %d:\n%s", n, raw)
	}
}
