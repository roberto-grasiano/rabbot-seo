package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/slack-go/slack"
)

func TestBuildBlocksBasic(t *testing.T) {
	a := Alert{
		Site:         "example.com",
		URL:          "https://example.com/p",
		ChangeType:   "indexability",
		Severity:     model.SeverityCritical,
		Before:       "indexable",
		After:        "noindex",
		DetectedAt:   time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC),
		GroupKey:     "example.com|indexability",
		RelatedCount: 0,
		DeepLink:     "https://example.com/p",
	}
	blocks := BuildBlocks(a)
	if len(blocks.BlockSet) == 0 {
		t.Fatal("no blocks produced")
	}
	if len(blocks.BlockSet) > 50 {
		t.Errorf("exceeded Block Kit 50-block cap: %d", len(blocks.BlockSet))
	}
	hdr, ok := blocks.BlockSet[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("first block is not a header: %T", blocks.BlockSet[0])
	}
	if hdr.Text.Type != slack.PlainTextType {
		t.Errorf("header must be plain_text, got %q", hdr.Text.Type)
	}
	if !strings.Contains(strings.ToUpper(hdr.Text.Text), "CRITICAL") {
		t.Errorf("header should contain severity: %q", hdr.Text.Text)
	}
}

func TestBuildBlocksRelatedCount(t *testing.T) {
	a := Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		RelatedCount: 46, DetectedAt: time.Now(),
		Items: []AlertItem{{URL: "https://example.com/1", Before: "A", After: "B"}},
	}
	blocks := BuildBlocks(a)
	joined := blocksText(blocks)
	if !strings.Contains(joined, "46") {
		t.Errorf("expected related count 46 in payload: %q", joined)
	}
}

func TestBuildBlocksTruncatesSectionText(t *testing.T) {
	long := strings.Repeat("x", 5000)
	a := Alert{
		Site: "s", ChangeType: "content", Severity: model.SeverityInfo,
		Before: long, After: long, DetectedAt: time.Now(),
	}
	blocks := BuildBlocks(a)
	for i, b := range blocks.BlockSet {
		if sec, ok := b.(*slack.SectionBlock); ok && sec.Text != nil {
			if len(sec.Text.Text) > 3000 {
				t.Errorf("block %d section text exceeds 3000 chars: %d", i, len(sec.Text.Text))
			}
		}
	}
}

// TestBuildBlocksEscapesUntrustedMrkdwn guards F21: a monitored site controls
// its own title/canonical/meta, which land in Alert.Before/After and are rendered
// into a Slack mrkdwn section. Those mrkdwn metacharacters (& < >) must be escaped
// so a hostile page cannot inject a fake clickable link or block-breaking markup
// into the operator's alert channel.
func TestBuildBlocksEscapesUntrustedMrkdwn(t *testing.T) {
	a := Alert{
		Site: "example.com", URL: "https://example.com/p", ChangeType: "title",
		Severity:   model.SeverityWarning,
		Before:     "legit title",
		After:      "<https://evil.example|Click to re-verify your account> & <stuff>",
		DetectedAt: time.Now(),
	}
	joined := blocksText(BuildBlocks(a))
	// The raw mrkdwn link syntax must NOT survive into the rendered block.
	if strings.Contains(joined, "<https://evil.example|") {
		t.Errorf("untrusted mrkdwn link was not escaped: %q", joined)
	}
	// Metacharacters must be HTML-entity-escaped per Slack's mrkdwn rules.
	if !strings.Contains(joined, "&lt;") || !strings.Contains(joined, "&gt;") || !strings.Contains(joined, "&amp;") {
		t.Errorf("expected & < > to be escaped to &amp; &lt; &gt;, got: %q", joined)
	}
}

// TestBuildBlocksEscapesUntrustedItems guards F21 for the per-item branch: if
// Alert.Items is ever wired up, each item's URL/Before/After is also untrusted
// and must be escaped before entering mrkdwn. (The visible link label uses the
// operator-facing URL but the same content can carry hostile markup.)
func TestBuildBlocksEscapesUntrustedItems(t *testing.T) {
	a := Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		DetectedAt: time.Now(),
		Items: []AlertItem{{
			URL:    "https://example.com/p",
			Before: "ok",
			After:  "<https://evil.example|phish> & <x>",
		}},
	}
	joined := blocksText(BuildBlocks(a))
	if strings.Contains(joined, "<https://evil.example|") {
		t.Errorf("untrusted item mrkdwn link was not escaped: %q", joined)
	}
}

func TestBuildBlocksOperationalLabel(t *testing.T) {
	a := Alert{
		Site: "example.com", ChangeType: model.ChangeTypeMonitoringBlocked,
		Severity: model.SeverityCritical, Operational: true, DetectedAt: time.Now(),
	}
	joined := strings.ToLower(blocksText(BuildBlocks(a)))
	if !strings.Contains(joined, "access") && !strings.Contains(joined, "monitoring") {
		t.Errorf("operational alert must be labeled as an access/monitoring problem: %q", joined)
	}
}

// TestBuildBlocksEscapesUntrustedURLAndDeepLink guards the mrkdwn-injection class
// for the URL section field and the DeepLink context link. With discovery
// (Stream 2), a.URL / a.DeepLink can be a crawled, attacker-influenced URL whose
// path/query carries mrkdwn metacharacters; neither may inject a clickable fake
// link or break out of <...> link syntax in the operator's alert channel.
func TestBuildBlocksEscapesUntrustedURLAndDeepLink(t *testing.T) {
	hostile := "https://example.com/p?x=<https://evil.example|click>&y=z"
	a := Alert{
		Site: "example.com", URL: hostile, ChangeType: "title",
		Severity: model.SeverityWarning, DetectedAt: time.Now(),
		DeepLink: hostile,
	}
	joined := blocksText(BuildBlocks(a))
	if strings.Contains(joined, "<https://evil.example|") {
		t.Errorf("untrusted URL/DeepLink mrkdwn link was not escaped: %q", joined)
	}
	if !strings.Contains(joined, "view affected URL") {
		t.Errorf("deep link context entry should still render: %q", joined)
	}
	if !strings.Contains(joined, "&lt;") || !strings.Contains(joined, "&gt;") || !strings.Contains(joined, "&amp;") {
		t.Errorf("expected & < > escaped to entities: %q", joined)
	}
}
