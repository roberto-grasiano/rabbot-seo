package wizard

import (
	"bytes"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

// SeeItWorkSummary renders a short, friendly "here's what we just read from your
// site" block from a precheck Report, so a newcomer watches the pipeline do
// something real before trusting it. It is honest about reading the server HTML
// only; it does NOT claim a change happened (that needs two snapshots over time).
//
// fetcher.DoctorReport exposes no parsed Title — it surfaces the served homepage
// body verbatim as rep.Doctor.RawHTML (nil on a blocked/unreachable fetch). We
// extract the <title> from that HTML here (mirroring internal/precheck's own
// goquery title read) rather than re-fetching; an empty/missing title simply omits
// the bullet, never crashes, and never invents a value.
func SeeItWorkSummary(rep precheck.Report) string {
	var b strings.Builder
	b.WriteString("Here's what we can see right now:\n")
	if t := titleFromHTML(rep.Doctor.RawHTML); t != "" {
		b.WriteString("  • Title: " + t + "\n")
	}
	b.WriteString("We'll let you know when this kind of thing changes.\n")
	return b.String()
}

// titleFromHTML pulls the <title> text out of the served homepage HTML, returning
// "" when the body is empty, unparsable, or has no title. Best-effort and
// non-fatal by design: a parse error degrades to no title line, never an error.
func titleFromHTML(rawHTML []byte) string {
	if len(rawHTML) == 0 {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawHTML))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Find("title").First().Text())
}
