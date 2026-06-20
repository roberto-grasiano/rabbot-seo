package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	mcpsrv "github.com/roberto-grasiano/rabbot-seo/internal/mcp"
)

// controlReadinessInput carries the already-probed facts renderControlReadiness
// turns into a ✓/✗ report. Keeping the probes out of the renderer makes the
// report pure and table-testable without a live daemon (mirrors how runDoctor
// splits precheck.Run from the errWriter rendering).
type controlReadinessInput struct {
	goos       string      // runtime.GOOS seam: on "windows" the 0600 perm check is reported as not-applicable
	binPath    string      // the resolved launch binary path
	binOK      bool        // binPath resolved to a real, non-empty path
	tokenPath  string      // <config-dir>/control.token
	tokenFound bool        // the token file exists and is non-empty
	tokenMode  os.FileMode // the token file's permission bits (only meaningful when tokenFound)
	healthErr  error       // result of client.Health(ctx): nil | ErrDaemonNotRunning | ErrUnauthorized | other
}

// renderControlReadiness writes the three-check control-plane readiness report:
// binary path · control.token present/0600 · daemon reachable AND token
// authenticates. Each failing check prints concrete remediation. It returns only
// the first write error (errWriter); a failed *check* is reported in the text, not
// as a Go error, so `doctor` always prints a full report.
func renderControlReadiness(w io.Writer, in controlReadinessInput) error {
	ew := &errWriter{w: w}
	ew.println("\nControl-plane readiness:")

	// 1. Binary path.
	ew.printf("  [%s] binary path\n", mark(in.binOK))
	if in.binOK {
		ew.printf("        %s\n", in.binPath)
	} else {
		ew.println("        could not resolve the rabbot binary path " +
			"(reinstall, or ensure 'rabbot' is on PATH)")
	}

	// 2. control.token present + 0600. The 0600 enforcement is POSIX-only: Go
	// maps NTFS attributes onto the mode bits (typically 0666), so a 0600 check
	// on Windows would always false-fail and recommend an inapplicable `chmod`.
	// On Windows we report presence and call the perm check not applicable (the
	// chmod-tighten in token.go is itself a harmless no-op there). POSIX output
	// is unchanged.
	windows := in.goos == "windows"
	tokenOK := in.tokenFound && (windows || in.tokenMode.Perm() == 0o600)
	ew.printf("  [%s] control.token present (0600)\n", mark(tokenOK))
	switch {
	case !in.tokenFound:
		ew.printf("        no token at %s — it is created on the daemon's first "+
			"start ('rabbot run' or 'rabbot service start')\n", in.tokenPath)
	case windows:
		ew.printf("        0600 perm check not applicable on Windows "+
			"(NTFS ACLs, not POSIX mode bits); token present at %s\n", in.tokenPath)
	case in.tokenMode.Perm() != 0o600:
		ew.printf("        %s is %o, want 0600 — run: chmod 600 %s\n",
			in.tokenPath, in.tokenMode.Perm(), in.tokenPath)
	}

	// 3. Daemon reachable AND token authenticates (the Hop-2 proof).
	healthOK := in.healthErr == nil
	ew.printf("  [%s] daemon reachable and token authenticates\n", mark(healthOK))
	switch {
	case errors.Is(in.healthErr, control.ErrDaemonNotRunning):
		ew.println("        daemon not running — start it with " +
			"'rabbot service start' (or 'rabbot run')")
	case errors.Is(in.healthErr, control.ErrUnauthorized):
		ew.println("        token mismatch — this client and the daemon disagree on " +
			"the data-dir/config (so they read different control.token files); " +
			"run doctor with the same --data-dir/--config the daemon uses")
	case in.healthErr != nil && in.tokenFound:
		ew.printf("        control check failed: %v\n", in.healthErr)
	}

	return ew.err
}

// healthChecker is the narrow seam probeControlReadiness needs from a control
// client — satisfied by *control.Client — so the probe is unit-testable with an
// httptest-backed client (no real daemon).
type healthChecker interface {
	Health(ctx context.Context) error
}

// probeControlReadiness assembles a controlReadinessInput from the live facts:
// the resolved binary, the token file's existence+mode (passed in, already
// stat'd by the caller), and a single Health() round-trip. It never returns an
// error: a failed probe is recorded in the struct for the renderer to surface.
func probeControlReadiness(
	ctx context.Context,
	binPath, tokenPath string,
	tokenFound bool,
	tokenMode os.FileMode,
	client healthChecker,
) controlReadinessInput {
	return controlReadinessInput{
		goos:       runtime.GOOS,
		binPath:    binPath,
		binOK:      binPath != "",
		tokenPath:  tokenPath,
		tokenFound: tokenFound,
		tokenMode:  tokenMode,
		healthErr:  client.Health(ctx),
	}
}

// runDoctorControl resolves the live control-plane facts (binary path, token file
// stat, daemon Health) for cfg and renders the readiness report to w. It is
// best-effort: a config-dir resolution failure is reported in-line, never fatal,
// so the doctor command always finishes.
func runDoctorControl(ctx context.Context, w io.Writer, cfg *config.Config) error {
	bin := mcpsrv.ResolveBinary()

	dir, derr := config.ResolveConfigDir()
	tokenPath := ""
	tokenFound := false
	var tokenMode os.FileMode
	if derr == nil {
		tokenPath = filepath.Join(dir, "control.token")
		if fi, err := os.Stat(tokenPath); err == nil && fi.Size() > 0 {
			tokenFound = true
			tokenMode = fi.Mode()
		}
	}

	client, cerr := newControlClient(cfg)
	if cerr != nil {
		// No client: client construction failed (e.g. the control.token file is
		// present but unreadable — permission/IO error). Surface the ACTUAL error so
		// the operator gets the right remediation, instead of masking it as
		// "daemon not running" (a misleading verdict that points at the wrong fix).
		// renderControlReadiness prints "control check failed: <cerr>" for a non-
		// sentinel healthErr when a token file exists. We still render the binary +
		// token checks above it so the report stays complete.
		in := controlReadinessInput{
			goos:    runtime.GOOS,
			binPath: bin, binOK: bin != "",
			tokenPath: tokenPath, tokenFound: tokenFound, tokenMode: tokenMode,
			healthErr: cerr,
		}
		return renderControlReadiness(w, in)
	}
	in := probeControlReadiness(ctx, bin, tokenPath, tokenFound, tokenMode, client)
	return renderControlReadiness(w, in)
}
