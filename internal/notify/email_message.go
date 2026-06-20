package notify

import (
	"fmt"
	"strings"
)

// buildEmailMessage renders an Alert into a complete plain-text RFC 5322 message
// (headers + blank line + body) with CRLF line endings, ready to hand to
// smtp.Client.Data (which performs dot-stuffing). It is plain text only — no
// MIME multipart, no HTML — by design (A1 scope).
func buildEmailMessage(from string, to []string, a Alert) []byte {
	var b strings.Builder

	// Headers. Date/Message-ID are intentionally omitted: the receiving MTA adds a
	// Received/Date, and an injected Message-ID is not required for delivery; this
	// keeps the message construction a pure function of the alert (deterministic,
	// testable) with no clock or hostname dependency.
	writeHeader(&b, "From", from)
	writeHeader(&b, "To", strings.Join(to, ", "))
	writeHeader(&b, "Subject", emailSubject(a))
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=utf-8")
	b.WriteString("\r\n") // blank line: end of headers

	b.WriteString(emailBody(a))
	return []byte(b.String())
}

// emailSubject is a single-line subject carrying severity + site + change_type so
// it is scannable in an inbox and routable by mail filters. It is collapsed to one
// line: CR/LF in any field would otherwise allow header injection.
func emailSubject(a Alert) string {
	kind := "SEO change"
	if a.Operational {
		kind = "ACCESS/MONITORING problem"
	}
	subj := fmt.Sprintf("[Rabbot] [%s] %s — %s — %s",
		strings.ToUpper(string(a.Severity)), kind, a.Site, a.ChangeType)
	return collapseHeader(subj)
}

// emailBody is the plain-text body: a labeled block of the alert fields plus the
// rolled-up affected pages. Untrusted crawled values (title/canonical/meta in
// Before/After, per-page URLs) are safe in a text/plain body — there is no markup
// to inject — but they ARE sanitized of control characters so a hostile page
// cannot smuggle terminal escape sequences into an operator's mail client.
func emailBody(a Alert) string {
	var b strings.Builder

	kind := "SEO change"
	if a.Operational {
		kind = "ACCESS / MONITORING problem"
	}
	fmt.Fprintf(&b, "%s detected by Rabbot.\r\n\r\n", kind)
	writeBodyField(&b, "Severity", strings.ToUpper(string(a.Severity)))
	writeBodyField(&b, "Site", a.Site)
	if a.URL != "" {
		writeBodyField(&b, "URL", a.URL)
	} else {
		writeBodyField(&b, "URL", "(site-level)")
	}
	writeBodyField(&b, "Change type", a.ChangeType)
	writeBodyField(&b, "Detected", a.DetectedAt.UTC().Format("2006-01-02 15:04 MST"))

	if a.Before != "" || a.After != "" {
		b.WriteString("\r\n")
		writeBodyField(&b, "Before", a.Before)
		writeBodyField(&b, "After", a.After)
	}

	if len(a.Items) > 0 {
		b.WriteString("\r\nAffected pages:\r\n")
		for _, it := range a.Items {
			fmt.Fprintf(&b, "  - %s: %s -> %s\r\n",
				sanitizeText(it.URL), sanitizeText(it.Before), sanitizeText(it.After))
		}
	}

	if a.RelatedCount > 0 {
		fmt.Fprintf(&b, "\r\n+%d related pages.\r\n", a.RelatedCount)
	}
	if a.DeepLink != "" {
		fmt.Fprintf(&b, "View: %s\r\n", sanitizeText(a.DeepLink))
	}
	return b.String()
}

// writeHeader writes one CRLF-terminated header line. The value is collapsed to a
// single line to prevent header injection from any field that flows into a header.
func writeHeader(b *strings.Builder, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(collapseHeader(value))
	b.WriteString("\r\n")
}

// writeBodyField writes a "Label: value" body line (value control-sanitized).
func writeBodyField(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "%s: %s\r\n", label, sanitizeText(value))
}

// collapseHeader removes CR/LF (header-injection defense) and other control
// characters from a value destined for an email header, replacing each with a
// single space and trimming the result.
func collapseHeader(s string) string {
	return strings.TrimSpace(stripControl(s, ' '))
}

// sanitizeText strips control characters (including the ESC that begins a
// terminal escape sequence) from body text, preserving ordinary spaces. It keeps
// no CR/LF — body line framing is added by the caller — so a value cannot inject
// extra lines either.
func sanitizeText(s string) string {
	return stripControl(s, ' ')
}

// stripControl replaces every ASCII control character (0x00–0x1F and 0x7F) in s
// with repl, leaving all printable and multibyte-UTF-8 runes intact.
func stripControl(s string, repl rune) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return repl
		}
		return r
	}, s)
}
