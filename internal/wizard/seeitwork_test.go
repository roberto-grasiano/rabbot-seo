package wizard

import (
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

func TestSeeItWorkSummary_ShowsWhatWeRead(t *testing.T) {
	rep := precheck.Report{}
	// fetcher.DoctorReport has no Title field; the served homepage HTML is surfaced
	// verbatim as RawHTML. The summary must extract the <title> from that HTML so a
	// newcomer sees the pipeline read something real off their site.
	rep.Doctor.RawHTML = []byte(`<html><head><title>Acme — Home</title></head><body>hi</body></html>`)
	got := SeeItWorkSummary(rep)
	if !strings.Contains(got, "Acme — Home") {
		t.Fatalf("summary %q did not surface the page title from RawHTML", got)
	}
}

func TestSeeItWorkSummary_NoTitleOmitsTitleLine(t *testing.T) {
	rep := precheck.Report{}
	rep.Doctor.RawHTML = []byte(`<html><head></head><body>no title here</body></html>`)
	got := SeeItWorkSummary(rep)
	// No <title> ⇒ no "Title:" bullet (guarded for empty), but the block is still a
	// friendly, honest summary that never crashes on missing data.
	if strings.Contains(got, "Title:") {
		t.Fatalf("summary %q surfaced a Title line for HTML with no <title>", got)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("summary should never be empty even with no title, got %q", got)
	}
}

func TestSeeItWorkSummary_EmptyRawHTMLIsSafe(t *testing.T) {
	// A blocked/unreachable homepage leaves RawHTML nil — the helper must not panic
	// and must not claim to have read a title.
	got := SeeItWorkSummary(precheck.Report{})
	if strings.Contains(got, "Title:") {
		t.Fatalf("summary %q claimed a title for nil RawHTML", got)
	}
}
