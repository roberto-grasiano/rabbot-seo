package wizard

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

func TestIsUserCancel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"huh user aborted", huh.ErrUserAborted, true},
		{"huh user aborted wrapped", fmt.Errorf("form: %w", huh.ErrUserAborted), true},
		{"program killed", tea.ErrProgramKilled, true},
		{"program killed wrapped (ctx)", fmt.Errorf("%w: %w", tea.ErrProgramKilled, errors.New("context canceled")), true},
		{"unrelated error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUserCancel(tc.err); got != tc.want {
				t.Errorf("isUserCancel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRunCancelCleanExit asserts the abort-classification path used by Run:
// a user-cancel error is mapped to the ErrCancelled sentinel + a friendly
// "setup cancelled." line on d.Out, never surfaced as a bare failure.
func TestRunCancelCleanExit(t *testing.T) {
	var out strings.Builder
	d := Deps{Out: &out}

	err := d.cancel(huh.ErrUserAborted)

	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel(huh.ErrUserAborted) = %v, want ErrCancelled", err)
	}
	if got := out.String(); !strings.Contains(got, "setup cancelled.") {
		t.Errorf("cancel output = %q, want it to contain %q", got, "setup cancelled.")
	}
}

// TestLiveScreenQuitMapsToCancel covers the HIGH fix: a quit (Ctrl-C / Esc) on a
// live precheck/proof screen makes Program.Run return (model, nil), so Run guards
// on the model's done flag and funnels the quit through d.cancel(tea.ErrProgramKilled).
// This asserts that cancel cause is classified as a user-cancel (so cli/init.go
// exits quietly) AND maps to the ErrCancelled sentinel — i.e. a mid-screen quit is
// treated as cancel, never as success. The Program.Run TTY seam itself is
// untestable without a real terminal; this asserts the cancel-cause LOGIC it feeds.
func TestLiveScreenQuitMapsToCancel(t *testing.T) {
	var out strings.Builder
	d := Deps{Out: &out}

	err := d.cancel(tea.ErrProgramKilled)

	if !isUserCancel(tea.ErrProgramKilled) {
		t.Fatalf("isUserCancel(tea.ErrProgramKilled) = false, want true (quit must classify as user-cancel)")
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel(tea.ErrProgramKilled) = %v, want errors.Is(..., ErrCancelled)", err)
	}
}

// TestLiveScreenDoneGuard covers the pure predicate behind the HIGH fix's done
// guard: a live screen that completed (done=true) proceeds; one quit before
// completion (done=false) is treated as cancelled.
func TestLiveScreenDoneGuard(t *testing.T) {
	if screenCancelled(true) {
		t.Error("screenCancelled(done=true) = true, want false (completed screen must proceed)")
	}
	if !screenCancelled(false) {
		t.Error("screenCancelled(done=false) = false, want true (quit before completion is a cancel)")
	}
}

// TestAuthorizationDeclinedMapsToCancel covers the MEDIUM fix: the intro
// authorization Confirm's Negative("Cancel") sets authorized=false WITHOUT
// huh.ErrUserAborted, so Run must catch it explicitly and short-circuit through
// d.cancel before the live screens. This asserts the declined-auth cancel maps to
// the ErrCancelled sentinel (→ quiet exit in cli/init.go). The intro.Run TTY seam
// is untestable without a real terminal; this asserts the cancel-cause LOGIC.
func TestAuthorizationDeclinedMapsToCancel(t *testing.T) {
	var out strings.Builder
	d := Deps{Out: &out}

	err := d.cancel(fmt.Errorf("authorization declined"))

	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel(authorization declined) = %v, want errors.Is(..., ErrCancelled)", err)
	}
}

// TestAuthorizationDeclinedGuard covers the pure predicate behind the MEDIUM fix:
// a declined attestation (authorized=false) short-circuits; an accepted one
// (authorized=true) proceeds.
func TestAuthorizationDeclinedGuard(t *testing.T) {
	if authorizationDeclined(true) {
		t.Error("authorizationDeclined(true) = true, want false (authorized operator proceeds)")
	}
	if !authorizationDeclined(false) {
		t.Error("authorizationDeclined(false) = false, want true (declined attestation cancels)")
	}
}

func TestValidateContactField(t *testing.T) {
	if err := validateContact("ops@example.com"); err != nil {
		t.Errorf("email contact rejected: %v", err)
	}
	if err := validateContact("https://example.com/contact"); err == nil {
		t.Error("a URL (not an email) contact should be rejected")
	}
	if err := validateContact("ftp://example.com"); err == nil {
		t.Error("non-email contact should be rejected")
	}
	if err := validateContact(""); err == nil {
		t.Error("empty contact should be rejected")
	}
}

func TestValidateSiteField(t *testing.T) {
	if err := validateSite("https://example.com"); err != nil {
		t.Errorf("https site rejected: %v", err)
	}
	if err := validateSite("ftp://example.com"); err == nil {
		t.Error("ftp site should be rejected")
	}
	if err := validateSite("http://127.0.0.1"); err == nil {
		t.Error("loopback site should be rejected (fetcher.ValidateSiteURL)")
	}
}
