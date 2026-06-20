package wizard

import "github.com/roberto-grasiano/rabbot-seo/internal/precheck"

// PlainVerdict turns a precheck.Report into one plain, advisory sentence shown at
// go-live. It never blocks and never leaks internal enum names. Order of concern:
// blocked/unreachable first (most actionable), then the JS hint, then the all-clear.
func PlainVerdict(rep precheck.Report, host string) string {
	switch {
	case rep.Doctor.Blocked:
		return "Heads up: " + host + " is blocking us right now — your network's address may need " +
			"allow-listing, or the site may challenge bots. We'll keep trying politely."
	case rep.JS.Kind == precheck.ClientShell:
		return "Heads up: parts of " + host + " load with JavaScript, so we may not see everything a " +
			"browser does. We'll monitor what's in the page's HTML."
	case rep.Verdict == precheck.VerdictRed:
		return "Heads up: we're having trouble reading " + host + " right now. Monitoring will run, " +
			"but may be partial until that clears."
	case rep.Verdict == precheck.VerdictGreen:
		return "Looks great — we can read " + host + " fine."
	default:
		return "We can monitor " + host + ", with a few caveats worth a quick look later."
	}
}
