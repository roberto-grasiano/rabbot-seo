package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// fakeReadiness lets us drive runDoctorControl without a live daemon: it supplies
// the binary path, the token-file probe result, and the Health() outcome.
func TestRunDoctorControl_AllGreen(t *testing.T) {
	var buf bytes.Buffer
	in := controlReadinessInput{
		binPath:    "/opt/rabbot",
		binOK:      true,
		tokenPath:  "/cfg/control.token",
		tokenFound: true,
		tokenMode:  0o600,
		healthErr:  nil, // daemon up + token authenticates
	}
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("renderControlReadiness: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Control-plane readiness",
		"[x] binary path",
		"[x] control.token present (0600)",
		"[x] daemon reachable and token authenticates",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunDoctorControl_DaemonDown(t *testing.T) {
	var buf bytes.Buffer
	in := controlReadinessInput{
		binPath: "/opt/rabbot", binOK: true,
		tokenPath: "/cfg/control.token", tokenFound: true, tokenMode: 0o600,
		healthErr: control.ErrDaemonNotRunning,
	}
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("renderControlReadiness: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[ ] daemon reachable") {
		t.Errorf("down daemon must render an unchecked daemon line:\n%s", out)
	}
	// Remediation must point at starting the service, never at Claude.
	if !containsFold(out, "rabbot service start") && !containsFold(out, "rabbot run") {
		t.Errorf("missing start-the-daemon remediation:\n%s", out)
	}
}

func TestRunDoctorControl_TokenMismatch(t *testing.T) {
	var buf bytes.Buffer
	in := controlReadinessInput{
		binPath: "/opt/rabbot", binOK: true,
		tokenPath: "/cfg/control.token", tokenFound: true, tokenMode: 0o600,
		healthErr: control.ErrUnauthorized,
	}
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("renderControlReadiness: %v", err)
	}
	out := buf.String()
	if !containsFold(out, "token mismatch") {
		t.Errorf("401 must surface a token-mismatch / dir-coherence message:\n%s", out)
	}
	if !containsFold(out, "data-dir") && !containsFold(out, "config") {
		t.Errorf("token-mismatch remediation must mention dir coherence:\n%s", out)
	}
}

func TestRunDoctorControl_TokenWrongMode(t *testing.T) {
	var buf bytes.Buffer
	in := controlReadinessInput{
		binPath: "/opt/rabbot", binOK: true,
		tokenPath: "/cfg/control.token", tokenFound: true, tokenMode: 0o644, // too open
		healthErr: nil,
	}
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("renderControlReadiness: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[ ] control.token present (0600)") {
		t.Errorf("a 0644 token must fail the 0600-mode check:\n%s", out)
	}
}

func TestRunDoctorControl_TokenMissing(t *testing.T) {
	var buf bytes.Buffer
	in := controlReadinessInput{
		binPath: "/opt/rabbot", binOK: true,
		tokenPath: "/cfg/control.token", tokenFound: false,
		healthErr: errors.New("unused: no token to send"),
	}
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("renderControlReadiness: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[ ] control.token present") {
		t.Errorf("missing token must fail its check:\n%s", out)
	}
}

// TestRenderControlReadiness_TokenPermGOOS proves the token-perm check is
// Windows-honest: on Windows the 0600 check is reported as not-applicable (no
// chmod advice — Go can't express POSIX perms on NTFS, and token.go's
// chmod-tighten is a harmless no-op there), while on POSIX the output is
// byte-identical to today (the 0600 check + chmod remediation). This runs on
// Linux by injecting goos="windows" through the seam.
func TestRenderControlReadiness_TokenPermGOOS(t *testing.T) {
	// chmodAdvice is the exact remediation string the POSIX arm must emit and
	// the Windows arm must NOT emit.
	const chmodAdvice = "chmod 600"

	tests := []struct {
		name          string
		goos          string
		tokenMode     os.FileMode
		wantChecked   bool   // the "[x] control.token present" mark
		wantSubstr    string // must appear
		wantNotSubstr string // must NOT appear
	}{
		{
			name:        "linux loose perms gives chmod advice",
			goos:        "linux",
			tokenMode:   0o644,
			wantChecked: false,
			wantSubstr:  chmodAdvice,
		},
		{
			name:        "darwin loose perms gives chmod advice",
			goos:        "darwin",
			tokenMode:   0o644,
			wantChecked: false,
			wantSubstr:  chmodAdvice,
		},
		{
			name:          "windows loose perms reports not applicable, no chmod",
			goos:          "windows",
			tokenMode:     0o666, // what Go reports for a normal file on NTFS
			wantChecked:   true,  // not a failure on Windows
			wantSubstr:    "not applicable",
			wantNotSubstr: chmodAdvice,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			in := controlReadinessInput{
				goos:       tt.goos,
				binPath:    "/opt/rabbot",
				binOK:      true,
				tokenPath:  "/cfg/control.token",
				tokenFound: true,
				tokenMode:  tt.tokenMode,
				healthErr:  nil,
			}
			if err := renderControlReadiness(&buf, in); err != nil {
				t.Fatalf("renderControlReadiness: %v", err)
			}
			out := buf.String()

			mark := "[x] control.token present"
			if !tt.wantChecked {
				mark = "[ ] control.token present"
			}
			if !strings.Contains(out, mark) {
				t.Errorf("want token-check mark %q in:\n%s", mark, out)
			}
			if tt.wantSubstr != "" && !strings.Contains(out, tt.wantSubstr) {
				t.Errorf("want substring %q in:\n%s", tt.wantSubstr, out)
			}
			if tt.wantNotSubstr != "" && strings.Contains(out, tt.wantNotSubstr) {
				t.Errorf("must NOT contain %q (Windows is honest, not chmod-advising):\n%s", tt.wantNotSubstr, out)
			}
		})
	}
}

// TestRenderControlReadiness_POSIXOutputUnchanged is the byte-identical guard:
// for a non-Windows goos the full rendered report must equal the output with an
// empty goos (today's behavior), so the seam is a pure no-op on POSIX.
func TestRenderControlReadiness_POSIXOutputUnchanged(t *testing.T) {
	base := controlReadinessInput{
		binPath:    "/opt/rabbot",
		binOK:      true,
		tokenPath:  "/cfg/control.token",
		tokenFound: true,
		tokenMode:  0o644,
		healthErr:  nil,
	}

	var before bytes.Buffer
	if err := renderControlReadiness(&before, base); err != nil {
		t.Fatalf("render (empty goos): %v", err)
	}

	for _, goos := range []string{"linux", "darwin"} {
		in := base
		in.goos = goos
		var got bytes.Buffer
		if err := renderControlReadiness(&got, in); err != nil {
			t.Fatalf("render (%s): %v", goos, err)
		}
		if got.String() != before.String() {
			t.Errorf("goos=%s output differs from today's (empty-goos) output:\n--- today ---\n%s\n--- %s ---\n%s",
				goos, before.String(), goos, got.String())
		}
	}
}

// TestProbeControlReadiness_HealthyServer drives the live probe against an httptest
// server standing in for the daemon's control API, asserting a healthy 200 on
// /v1/health with the right bearer token reports all-green.
func TestProbeControlReadiness_HealthyServer(t *testing.T) {
	const token = "deadbeefcafetoken"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := control.NewClientWithBaseURL(srv.URL, token)
	in := probeControlReadiness(context.Background(), "/opt/rabbot", "/cfg/control.token", true, 0o600, client)
	if !in.binOK {
		t.Errorf("binOK = false, want true for a non-empty bin path")
	}
	if in.healthErr != nil {
		t.Errorf("healthErr = %v, want nil for a healthy authed server", in.healthErr)
	}
}

func TestProbeControlReadiness_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	client := control.NewClientWithBaseURL(srv.URL, "wrong")
	in := probeControlReadiness(context.Background(), "/opt/rabbot", "/cfg/control.token", true, 0o600, client)
	if !errors.Is(in.healthErr, control.ErrUnauthorized) {
		t.Errorf("healthErr = %v, want ErrUnauthorized", in.healthErr)
	}
}

// TestRunDoctorControl_E2E_Healthy stands up an httptest control server that
// authenticates the daemon's bearer token on /v1/health and asserts the rendered
// readiness report is all-green via the live client (not the injected struct).
func TestRunDoctorControl_E2E_Healthy(t *testing.T) {
	const token = "e2etoken"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" && r.Header.Get("Authorization") == "Bearer "+token {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	client := control.NewClientWithBaseURL(srv.URL, token)
	in := probeControlReadiness(context.Background(), "/opt/rabbot", "/cfg/control.token", true, 0o600, client)

	var buf bytes.Buffer
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[x] daemon reachable and token authenticates") {
		t.Errorf("healthy E2E not all-green:\n%s", out)
	}
}

// TestRunDoctorControl_E2E_NoServer proves an unreachable control endpoint
// renders the daemon-not-running remediation (transport error → ErrDaemonNotRunning).
func TestRunDoctorControl_E2E_NoServer(t *testing.T) {
	// A closed server: build the client against a port nothing listens on.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	client := control.NewClientWithBaseURL(url, "tok")
	in := probeControlReadiness(context.Background(), "/opt/rabbot", "/cfg/control.token", true, 0o600, client)

	var buf bytes.Buffer
	if err := renderControlReadiness(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[ ] daemon reachable") {
		t.Errorf("unreachable daemon must render unchecked:\n%s", out)
	}
	if !containsFold(out, "service start") && !containsFold(out, "rabbot run") {
		t.Errorf("missing start remediation:\n%s", out)
	}
}
