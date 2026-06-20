package wizard

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

func TestPlainVerdict_Green(t *testing.T) {
	got := PlainVerdict(precheck.Report{Verdict: precheck.VerdictGreen}, "yoursite.com")
	if !strings.Contains(strings.ToLower(got), "looks great") && !strings.Contains(strings.ToLower(got), "read") {
		t.Fatalf("green verdict %q not plain/positive", got)
	}
}

func TestPlainVerdict_RedIsActionable(t *testing.T) {
	rep := precheck.Report{Verdict: precheck.VerdictRed}
	rep.Doctor.Blocked = true
	got := PlainVerdict(rep, "yoursite.com")
	if strings.Contains(got, "VerdictRed") || strings.Contains(got, "FetchUnreachable") {
		t.Fatalf("verdict %q leaks internal enum names", got)
	}
	if got == "" {
		t.Fatal("red verdict produced no message")
	}
}

// A client-rendered SPA always grades VerdictRed in precheck.grade(), so the
// JavaScript caveat copy must win over the generic red message — it is the
// project's #1 honest-warning requirement.
func TestPlainVerdict_ClientShellShowsJavaScriptCaveat(t *testing.T) {
	rep := precheck.Report{Verdict: precheck.VerdictRed}
	rep.JS.Kind = precheck.ClientShell
	got := PlainVerdict(rep, "yoursite.com")
	if !strings.Contains(got, "JavaScript") {
		t.Fatalf("client-shell verdict %q does not mention the JavaScript caveat", got)
	}
}

// A plain (non-client-shell) red must still get the generic "trouble reading"
// message, not the JavaScript caveat.
func TestPlainVerdict_PlainRedIsGeneric(t *testing.T) {
	rep := precheck.Report{Verdict: precheck.VerdictRed}
	got := PlainVerdict(rep, "yoursite.com")
	if strings.Contains(got, "JavaScript") {
		t.Fatalf("plain red verdict %q wrongly shows the JavaScript caveat", got)
	}
	if !strings.Contains(got, "trouble reading") {
		t.Fatalf("plain red verdict %q is not the generic message", got)
	}
}
