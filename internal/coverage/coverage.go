// Package coverage estimates how long a full crawl pass takes and roughly how much
// disk it uses, from the resolved crawl budget. It is pure and deterministic so the
// CLI (doctor, init) and Spec B's onboarding wizard can share one honest estimator.
//
// The numbers are honest approximations, not guarantees: real wall-clock depends on
// page latency, robots Crawl-delay, and error backoff. The per-host request rate the
// estimator models is the steady-state admission ceiling, which is exactly
// 1/per_host_rate: the frontier admits each host through ONE rate.Limiter
// (rate.Every(per_host_rate), burst 1), so per-host concurrency only covers fetch
// latency and never raises the admission rate. The daemon's global parallelism cap
// (at most 8 concurrent fetches across all hosts) bounds only the SUM across hosts, not
// any single host's rate. Estimate deliberately does not try to predict network
// conditions; callers surface it as "≈", never as a promise.
package coverage

import "time"

// perPageEstimate is the rough on-disk footprint per monitored page: one snapshot row
// (extracted title/meta/canonical/indexability + fetch metadata) plus the body hash
// and per-page change-tracking overhead. It is a calibrated approximation, documented
// as inexact (open-question #2): measure real snapshot rows to refine it. 12 KiB is a
// deliberately conservative middle estimate for a typical HTML page's stored metadata.
const perPageEstimate int64 = 12 * 1024 // 12 KiB

// maxEstimatePages clamps the page count before the footprint and full-pass math, so an
// absurd --pages value (or a runaway sitemap count) can never overflow the int64 byte
// multiply or render a nonsense figure. 100 million pages is far beyond any real
// monitored site and keeps pages × perPageEstimate (≈1.2 TB) comfortably within int64.
const maxEstimatePages = 100_000_000

// Result is the coverage estimate for a page count at a given crawl budget.
//
// (Named Result rather than Estimate because Go does not permit a type and a
// function to share an identifier in one package, and Estimate is the verb the
// callers invoke. This is the one deviation from the plan's pinned shape, which
// declared both as Estimate; everything else — fields, formulas, constants — is
// verbatim.)
type Result struct {
	// ReqPerSec is the effective steady-state requests/sec to the host.
	ReqPerSec float64
	// FullPass is the wall-clock to crawl `pages` once at this rate.
	FullPass time.Duration
	// ApproxBytes is the rough DB footprint = pages × perPageEstimate.
	ApproxBytes int64
}

// Estimate computes the coverage estimate for `pages` pages at a given per-host crawl
// budget. perHostRate is the minimum spacing between requests to the host (e.g. 2s).
//
//	ReqPerSec   = 1 / perHostRate.Seconds()   // the real per-host admission ceiling
//	FullPass    = pages / ReqPerSec
//	ApproxBytes = pages × perPageEstimate
//
// Per-host admission is 1/per_host_rate regardless of per-host concurrency: the frontier
// admits each host through a single rate.Limiter at burst 1, so concurrency only covers
// fetch latency and never raises the rate. The daemon's global-8 fetch cap bounds only
// the SUM across hosts, not a single host — so neither concurrency nor that cap enters
// this per-host number.
//
// Guards: a non-positive page count or a non-positive rate is unknowable and yields the
// zero Result — the caller renders that as "page count unknown — pass --pages N". The
// page count is clamped to maxEstimatePages before the multiply so an absurd --pages can
// never overflow int64.
func Estimate(pages int, perHostRate time.Duration) Result {
	if pages <= 0 || perHostRate <= 0 {
		return Result{}
	}
	if pages > maxEstimatePages {
		pages = maxEstimatePages
	}
	reqPerSec := 1.0 / perHostRate.Seconds()
	full := time.Duration(float64(pages) / reqPerSec * float64(time.Second))
	return Result{
		ReqPerSec:   reqPerSec,
		FullPass:    full,
		ApproxBytes: int64(pages) * perPageEstimate,
	}
}
