package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/slack-go/slack"
)

// Block Kit platform caps.
const (
	maxBlocks       = 50
	maxSectionChars = 3000
	maxTotalChars   = 40000
	maxItemsPerMsg  = 10
)

// truncate clamps s to n runes with an ellipsis marker. It counts and cuts by
// rune (not byte) so a multibyte UTF-8 sequence is never split into invalid bytes
// — Slack rejects malformed UTF-8. n caps the rune count; the byte length stays
// well within Slack's byte limits (each rune is at most 4 bytes, and the caps
// here are ≤3000).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

// mrkdwnEscaper escapes the three Slack mrkdwn metacharacters per Slack's
// documented rules. Order matters: & must be escaped first so the &lt;/&gt;
// entities introduced for < and > are not themselves re-escaped.
var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeMrkdwn neutralizes Slack mrkdwn metacharacters in untrusted text so a
// monitored third-party page cannot inject clickable fake links or block-breaking
// markup (e.g. <https://evil|label>) into the operator's alert channel via fields
// it controls — title, canonical, meta, hreflang — which flow into Before/After.
func escapeMrkdwn(s string) string {
	return mrkdwnEscaper.Replace(s)
}

func severityEmoji(sev model.Severity) string {
	switch sev {
	case model.SeverityCritical:
		return "🔴"
	case model.SeverityWarning:
		return "🟠"
	default:
		return "🔵"
	}
}

// BuildBlocks renders an Alert into a Block Kit message that respects the
// 50-block / 3000-char-per-section / ~40000-total caps. Operational (access)
// incidents are clearly labeled and never framed as SEO regressions.
func BuildBlocks(a Alert) slack.Blocks {
	var set []slack.Block

	kind := "SEO change"
	if a.Operational {
		kind = "ACCESS / MONITORING problem"
	}
	headerText := fmt.Sprintf("%s [%s] %s — %s",
		severityEmoji(a.Severity), strings.ToUpper(string(a.Severity)), kind, a.ChangeType)
	set = append(set, slack.NewHeaderBlock(
		slack.NewTextBlockObject(slack.PlainTextType, truncate(headerText, 150), true, false)))

	// Section fields: site / url / change-type / detected-at.
	urlField := a.URL
	if urlField == "" {
		urlField = "(site-level)"
	}
	fields := []*slack.TextBlockObject{
		slack.NewTextBlockObject(slack.MarkdownType, "*Site:*\n"+truncate(a.Site, 1000), false, false),
		slack.NewTextBlockObject(slack.MarkdownType, "*URL:*\n"+escapeMrkdwn(truncate(urlField, 1000)), false, false),
		slack.NewTextBlockObject(slack.MarkdownType, "*Change type:*\n"+truncate(a.ChangeType, 1000), false, false),
		slack.NewTextBlockObject(slack.MarkdownType, "*Detected:*\n"+a.DetectedAt.UTC().Format("2006-01-02 15:04 MST"), false, false),
	}
	set = append(set, slack.NewSectionBlock(nil, fields, nil))

	// before -> after, truncated, mrkdwn. Before/After are operator-untrusted
	// (they carry crawled title/canonical/meta), so escape them AFTER truncation
	// (truncation is rune-based; escaping a clamped string keeps entities intact)
	// and never escape the literal ~/→/* framing we add ourselves.
	if a.Before != "" || a.After != "" {
		ba := fmt.Sprintf("~%s~ → *%s*",
			escapeMrkdwn(truncate(a.Before, maxSectionChars/2-8)),
			escapeMrkdwn(truncate(a.After, maxSectionChars/2-8)))
		set = append(set, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, truncate(ba, maxSectionChars), false, false), nil, nil))
	}

	// Rolled-up items (paginated to maxItemsPerMsg).
	shown := 0
	for _, it := range a.Items {
		if shown >= maxItemsPerMsg || len(set) >= maxBlocks-2 {
			break
		}
		// Items are untrusted (per-page crawled URL + before/after); escape every
		// field after truncation so a hostile page cannot inject mrkdwn. The link
		// target is escaped too: a malformed/hostile URL must not break out of the
		// <...> link syntax.
		line := fmt.Sprintf("• <%s|%s>: ~%s~ → *%s*",
			escapeMrkdwn(it.URL), escapeMrkdwn(truncate(it.URL, 80)),
			escapeMrkdwn(truncate(it.Before, 200)), escapeMrkdwn(truncate(it.After, 200)))
		set = append(set, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, truncate(line, maxSectionChars), false, false), nil, nil))
		shown++
	}

	// Context: +N related, deep link.
	ctxParts := []string{}
	if a.RelatedCount > 0 {
		ctxParts = append(ctxParts, fmt.Sprintf("+%d related pages", a.RelatedCount))
	}
	if a.DeepLink != "" {
		// DeepLink is an operator-untrusted crawled URL flowing into <...> link
		// syntax; escape it (like the Items link targets above) so a hostile URL
		// containing '>' cannot break out of the link and inject channel markup.
		ctxParts = append(ctxParts, fmt.Sprintf("<%s|view affected URL>", escapeMrkdwn(a.DeepLink)))
	} else if a.URL != "" {
		ctxParts = append(ctxParts, "run `rabbot history "+a.URL+"`")
	}
	if len(ctxParts) > 0 {
		set = append(set, slack.NewContextBlock("",
			slack.NewTextBlockObject(slack.MarkdownType, truncate(strings.Join(ctxParts, "  •  "), maxSectionChars), false, false)))
	}

	if len(set) > maxBlocks {
		set = set[:maxBlocks]
	}
	return slack.Blocks{BlockSet: set}
}

// blocksText concatenates the rendered text of all blocks (test helper).
func blocksText(b slack.Blocks) string {
	var sb strings.Builder
	for _, blk := range b.BlockSet {
		switch v := blk.(type) {
		case *slack.HeaderBlock:
			if v.Text != nil {
				sb.WriteString(v.Text.Text + "\n")
			}
		case *slack.SectionBlock:
			if v.Text != nil {
				sb.WriteString(v.Text.Text + "\n")
			}
			for _, f := range v.Fields {
				sb.WriteString(f.Text + "\n")
			}
		case *slack.ContextBlock:
			for _, el := range v.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok {
					sb.WriteString(t.Text + "\n")
				}
			}
		}
	}
	return sb.String()
}
