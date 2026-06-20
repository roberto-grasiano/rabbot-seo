package precheck

import (
	"context"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Verdict is the overall traffic-light for the doctor report.
type Verdict string

const (
	// VerdictGreen means the URL is monitorable: its content is in the server HTML
	// (server-rendered) or recoverable from a hydration payload.
	VerdictGreen Verdict = "green"
	// VerdictYellow means proceed with caveats: the render mode is uncertain
	// (Unknown) or only partially recoverable.
	VerdictYellow Verdict = "yellow"
	// VerdictRed means do not expect reliable monitoring: the site is blocked,
	// unreachable, robots-disallowed, or shows a strong client-shell (needs-JS) hint.
	VerdictRed Verdict = "red"
)

// Report is the full doctor result: the reused fetcher preflight plus the JS detection
// and an overall Verdict. It embeds (never reimplements) the fetcher.DoctorReport so the
// robots/egress/blocked/UA/redirect facts are surfaced verbatim.
type Report struct {
	// Doctor is the reused fetcher preflight (robots/egress/blocked/UA/redirects).
	Doctor fetcher.DoctorReport
	// JS is the JS-dependency detection run over the homepage HTML.
	JS JSDependency
	// Verdict is the overall traffic-light, composed from Doctor (block/robots/
	// reachability) and JS.Kind.
	Verdict Verdict
	// Summary is the overall honest one-liner for the verdict line.
	Summary string
}

// Options configures Run. It mirrors the arguments fetcher.Doctor already takes so Run
// is a thin honest layer over the existing preflight, and is plain data so the CLI and
// the future TUI wizard can both build it.
type Options struct {
	// UserAgent is the resolved crawler User-Agent (cfg.ResolvedUserAgent(version)).
	UserAgent string
	// EgressEndpoint is the outbound-IP echo endpoint; "" skips the egress probe.
	EgressEndpoint string
	// Request carries per-site access settings (proxy/headers/basic-auth/cookies);
	// the zero value is fine for the common case.
	Request fetcher.Request
	// AllowPrivate is TEST-ONLY: it lets the loopback httptest targets through the
	// SSRF guard. Production callers pass false.
	AllowPrivate bool
}

// Run performs the honest precheck for url. It composes fetcher.Doctor for the
// robots/egress/blocked/UA preflight (which already fetches the homepage once and
// surfaces its body), runs Detect on that body, and grades an overall Verdict. It is
// pure orchestration — safe to call from the CLI or the future TUI wizard — and issues
// no live network in tests (callers pass AllowPrivate with an httptest URL).
func Run(ctx context.Context, url string, opts Options) (Report, error) {
	dr, err := fetcher.Doctor(ctx, url, opts.Request, opts.UserAgent, opts.EgressEndpoint, opts.AllowPrivate)
	if err != nil {
		return Report{}, err
	}

	// Reuse the homepage body the preflight already fetched (surfaced on DoctorReport)
	// — no second request. It is only populated for ok-class fetches, so a blocked or
	// unreachable homepage yields an empty body and Detect returns Unknown.
	js := Detect(dr.RawHTML, dr.ContentType)

	rep := Report{Doctor: dr, JS: js, Verdict: grade(dr, js)}
	rep.Summary = verdictSummary(rep.Verdict)
	return rep, nil
}

// grade composes the overall Verdict from the reused preflight facts and the JS hint.
// Reachability/robots/block facts dominate; a strong client-shell hint also reds out.
func grade(dr fetcher.DoctorReport, js JSDependency) Verdict {
	switch {
	case dr.Blocked,
		dr.RobotsVerdict == "disallowed",
		dr.FetchClass == model.FetchUnreachable,
		js.Kind == ClientShell:
		return VerdictRed
	case js.ContentVisibleToCrawler:
		return VerdictGreen
	default:
		return VerdictYellow
	}
}

// verdictSummary returns the honest one-liner for the overall verdict line.
func verdictSummary(v Verdict) string {
	switch v {
	case VerdictGreen:
		return "Green: this URL looks monitorable — its SEO content is reachable in the server's HTML."
	case VerdictRed:
		return "Red: Rabbot cannot reliably monitor this URL as-is — see the reasons below."
	default:
		return "Yellow: monitorable with caveats — review the signals and confirm in a browser."
	}
}
