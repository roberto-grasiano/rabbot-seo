package alerts

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestFingerprintGolden pins the Fingerprint output to a known hash so any
// accidental change to the hashed field set (e.g. A7 adding Segments to Event)
// is caught: segments annotate and route alerts but must NEVER re-group or
// re-dedup incidents, so the fingerprint must stay byte-identical.
func TestFingerprintGolden(t *testing.T) {
	e := Event{
		Site:       "https://example.com",
		URL:        "https://example.com/blog/post",
		ChangeType: "title",
		Severity:   model.SeverityWarning,
	}
	// Golden value: sha256 of the NUL-separated tuple
	// "https://example.com\x00https://example.com/blog/post\x00title\x00warning".
	const want = "db43b64947d53d4ad5835ef46a3ac8e5b149bcb91f3ef6f09b6e6cbc6ea89efb"
	got := Fingerprint(e)
	if got != want {
		t.Fatalf("Fingerprint output drifted: got %q, want %q (the hashed field set must stay byte-identical)", got, want)
	}

	// Adding segments to the event must not change the fingerprint.
	withSeg := e
	withSeg.Segments = []string{"content", "featured"}
	if Fingerprint(withSeg) != got {
		t.Errorf("Segments must not affect Fingerprint: %q != %q", Fingerprint(withSeg), got)
	}

	// The group fingerprint (URL elided) must likewise be segment-invariant.
	if groupFingerprint(withSeg) != groupFingerprint(e) {
		t.Error("Segments must not affect groupFingerprint")
	}
}

// TestGroupKeyUnaffectedBySegments confirms GroupKey is computed purely from
// site + change_type and ignores any segment annotation.
func TestGroupKeyUnaffectedBySegments(t *testing.T) {
	if got := GroupKey("https://example.com", "title"); got != "https://example.com|title" {
		t.Errorf("GroupKey = %q, want site|change_type form", got)
	}
}

// TestFingerprintStableAcrossSeverityOnly is a focused guard that the hashed
// tuple is exactly (site, url, change_type, severity) and nothing else: two
// events differing only in Segments share a fingerprint; differing in severity
// do not.
func TestFingerprintStableAcrossSeverityOnly(t *testing.T) {
	base := Event{Site: "s", URL: "u", ChangeType: "ct", Severity: model.SeverityWarning}
	a := base
	a.Segments = []string{"x"}
	b := base
	b.Before, b.After, b.DeepLink, b.URLID = "old", "new", "link", 99
	if Fingerprint(a) != Fingerprint(base) {
		t.Error("Segments changed fingerprint")
	}
	if Fingerprint(b) != Fingerprint(base) {
		t.Error("non-hashed fields (before/after/deeplink/urlid) changed fingerprint")
	}
	c := base
	c.Severity = model.SeverityCritical
	if Fingerprint(c) == Fingerprint(base) {
		t.Error("severity should be part of the fingerprint")
	}
}
