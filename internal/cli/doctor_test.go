package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

func ssrServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Real Page</title>` +
			`<meta name="description" content="d"></head><body><h1>Hi</h1>` +
			`<p>Plenty of genuine server rendered prose here so this clearly reads ` +
			`as a server rendered page for the precheck detector heuristics involved.</p>` +
			`</body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientShellServer(t *testing.T) *httptest.Server {
	t.Helper()
	shell := `<html><head></head><body><div id="root"></div><script>` +
		strings.Repeat("var x=1;/*pad*/", 2000) + `</script></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shell))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunDoctorServerRendered(t *testing.T) {
	site := ssrServer(t)
	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, site.URL, precheck.Options{
		UserAgent:    "Rabbot-SEO/test (+https://example.test)",
		AllowPrivate: true,
	}, nil)
	if err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "GREEN") {
		t.Errorf("output missing GREEN verdict:\n%s", out)
	}
	if !strings.Contains(out, "server_rendered") {
		t.Errorf("output missing server_rendered render mode:\n%s", out)
	}
	if !containsFold(out, "present in the server") {
		t.Errorf("output missing reassuring 'present in the server' message:\n%s", out)
	}
}

func TestRunDoctorClientShellWarns(t *testing.T) {
	site := clientShellServer(t)
	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, site.URL, precheck.Options{
		UserAgent:    "Rabbot-SEO/test (+https://example.test)",
		AllowPrivate: true,
	}, nil)
	if err != nil {
		t.Fatalf("runDoctor() error = %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "RED") {
		t.Errorf("output missing RED verdict for client shell:\n%s", out)
	}
	if !strings.Contains(out, "client_shell") {
		t.Errorf("output missing client_shell render mode:\n%s", out)
	}
	// The mandatory honest warning (the user's #1 requirement).
	for _, phrase := range []string{
		"reads the server",
		"may not see",
		"cannot fully verify",
	} {
		if !containsFold(out, phrase) {
			t.Errorf("output missing mandatory warning phrase %q:\n%s", phrase, out)
		}
	}
	// The signal list must be present.
	if !strings.Contains(out, "empty_framework_root") {
		t.Errorf("output missing signal list:\n%s", out)
	}
	// The "this is a hint, confirm via View Source" line.
	if !containsFold(out, "view source") {
		t.Errorf("output missing 'confirm via View Source' guidance:\n%s", out)
	}
}

// TestDoctorCommandRegistered asserts `rabbot doctor --help` is reachable from the
// root command tree (registration check for T6).
func TestDoctorCommandRegistered(t *testing.T) {
	root := NewRootCmd(BuildInfo{Version: "test"})
	cmd, _, err := root.Find([]string{"doctor"})
	if err != nil {
		t.Fatalf("Find(doctor) error = %v", err)
	}
	if cmd.Name() != "doctor" {
		t.Fatalf("resolved command = %q, want doctor", cmd.Name())
	}

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"doctor", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor --help error = %v", err)
	}
	if !strings.Contains(buf.String(), "doctor <url>") {
		t.Errorf("doctor --help output missing usage:\n%s", buf.String())
	}
}

// TestDoctorEgressFlagDefaultsOff asserts the doctor command exposes a --check-egress
// flag that defaults to false, so a one-shot diagnostic does not silently call the
// third-party egress endpoint (e.g. api.ipify.org) just to diagnose a user-supplied URL.
func TestDoctorEgressFlagDefaultsOff(t *testing.T) {
	cmd := newDoctorCmd(BuildInfo{Version: "test"})
	f := cmd.Flags().Lookup("check-egress")
	if f == nil {
		t.Fatalf("doctor command missing --check-egress flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--check-egress default = %q, want false (egress probe off by default)", f.DefValue)
	}
}

// TestDoctorPagesFlagDefaultsZero asserts the doctor command exposes a --pages flag
// defaulting to 0 (auto-count from the sitemap), per the coverage estimator (Phase 4).
func TestDoctorPagesFlagDefaultsZero(t *testing.T) {
	cmd := newDoctorCmd(BuildInfo{Version: "test"})
	f := cmd.Flags().Lookup("pages")
	if f == nil {
		t.Fatalf("doctor command missing --pages flag")
	}
	if f.DefValue != "0" {
		t.Errorf("--pages default = %q, want 0 (auto-count from sitemap)", f.DefValue)
	}
}

// TestRunDoctorCoverageLineWithPages proves an explicit page count produces the coverage
// estimate line in the report. The httptest server is server-rendered (so the precheck
// reaches the estimator block, after the rendering check and before control readiness),
// and pagesOverride supplies the count so no sitemap is needed.
func TestRunDoctorCoverageLineWithPages(t *testing.T) {
	site := ssrServer(t)
	var buf bytes.Buffer
	err := runDoctorWithPages(context.Background(), &buf, site.URL, precheck.Options{
		UserAgent:    "Rabbot-SEO/test (+https://example.test)",
		AllowPrivate: true,
	}, nil, 10000)
	if err != nil {
		t.Fatalf("runDoctorWithPages() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Coverage:") {
		t.Errorf("output missing Coverage line:\n%s", out)
	}
	if !strings.Contains(out, "10000 pages") {
		t.Errorf("output missing page count from --pages:\n%s", out)
	}
}

// TestDoctorEgressEndpoint proves the egress endpoint is only resolved when the user
// opts in via --check-egress AND config enables it: off-by-default keeps doctor
// self-contained, on lets the probe run.
func TestDoctorEgressEndpoint(t *testing.T) {
	const endpoint = "https://api.example.test"
	tests := []struct {
		name        string
		checkEgress bool
		cfgEnabled  bool
		want        string
	}{
		{name: "off_by_default", checkEgress: false, cfgEnabled: true, want: ""},
		{name: "opted_in_and_enabled", checkEgress: true, cfgEnabled: true, want: endpoint},
		{name: "opted_in_but_cfg_disabled", checkEgress: true, cfgEnabled: false, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := doctorEgressEndpoint(tc.checkEgress, tc.cfgEnabled, endpoint)
			if got != tc.want {
				t.Errorf("doctorEgressEndpoint(%t, %t, %q) = %q, want %q", tc.checkEgress, tc.cfgEnabled, endpoint, got, tc.want)
			}
		})
	}
}

// TestDoctorRejectsInvalidURL proves the command validates the URL at the boundary
// (fetcher.ValidateSiteURL) like every other input path, rejecting a non-http(s) scheme
// up front rather than failing late in the transport. The assertion targets the specific
// scheme error so it stays load-bearing (a missing guard would not produce it).
func TestDoctorRejectsInvalidURL(t *testing.T) {
	cmd := newDoctorCmd(BuildInfo{Version: "test"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"gopher://example.com/x"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a non-http(s) URL scheme, got nil")
	}
	if !strings.Contains(err.Error(), "scheme must be http") {
		t.Errorf("expected a scheme validation error, got: %v", err)
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
