package coverage

import (
	"testing"
	"time"
)

// TestEstimateGolden pins the spec's worked example: 10,000 pages at a 2s per-host
// spacing → 0.5 req/s (the real per-host admission ceiling is 1/per_host_rate, since
// the frontier admits a host through ONE rate.Limiter at burst 1), a ~5h33m full pass,
// and a footprint of pages × perPageEstimate. The duration is asserted within a 1s band
// because it is a float→Duration computation, not an exact integer.
func TestEstimateGolden(t *testing.T) {
	est := Estimate(10000, 2*time.Second)

	if got, want := est.ReqPerSec, 0.5; got != want {
		t.Errorf("ReqPerSec = %v, want %v", got, want)
	}
	// pages / reqPerSec = 10000 / 0.5 = 20000s = 5h33m20s.
	wantFull := 20000 * time.Second
	if diff := est.FullPass - wantFull; diff < -time.Second || diff > time.Second {
		t.Errorf("FullPass = %v, want ~%v", est.FullPass, wantFull)
	}
	if got, want := est.ApproxBytes, int64(10000)*perPageEstimate; got != want {
		t.Errorf("ApproxBytes = %d, want %d", got, want)
	}
}

// TestEstimateUnknownCount proves the "page count unknown" signal: a non-positive
// page count (no sitemap, no --pages) yields the zero Result, which the caller
// renders as "page count unknown — pass --pages N".
func TestEstimateUnknownCount(t *testing.T) {
	for _, pages := range []int{0, -1} {
		if est := Estimate(pages, 2*time.Second); est != (Result{}) {
			t.Errorf("Estimate(%d, …) = %+v, want zero Result", pages, est)
		}
	}
}

// TestEstimateGuards covers the defensive branch: a non-positive rate is unknowable
// (zero Result).
func TestEstimateGuards(t *testing.T) {
	if est := Estimate(100, 0); est != (Result{}) {
		t.Errorf("Estimate(100, 0, …) = %+v, want zero Result for non-positive rate", est)
	}
}

// TestEstimateRateIndependentOfConcurrency proves the honest model: the per-host
// request rate is 1/per_host_rate, NOT concurrency/per_host_rate. The frontier admits
// a host through a single rate.Limiter (rate.Every(per_host_rate), burst 1), so
// concurrency only covers fetch latency — it never raises the admission ceiling. A 2s
// spacing therefore yields 0.5 req/s no matter the per-host concurrency.
func TestEstimateRateIndependentOfConcurrency(t *testing.T) {
	est := Estimate(10000, 2*time.Second)
	if got, want := est.ReqPerSec, 0.5; got != want {
		t.Errorf("ReqPerSec at 2s spacing = %v, want %v (1/per_host_rate, concurrency-independent)", got, want)
	}
}

// TestEstimatePageOverflowClamp proves an absurd --pages value cannot overflow the
// int64 byte footprint multiply: pages is clamped to maxEstimatePages before the
// pages × perPageEstimate computation, so ApproxBytes stays finite and positive.
func TestEstimatePageOverflowClamp(t *testing.T) {
	// A pages value whose product with perPageEstimate would overflow int64 if
	// computed naively. Clamped to maxEstimatePages first, the result is bounded.
	est := Estimate(int(^uint(0)>>1), time.Second) // max int
	if est.ApproxBytes <= 0 {
		t.Errorf("ApproxBytes = %d, want a positive bounded value (no overflow)", est.ApproxBytes)
	}
	wantBytes := int64(maxEstimatePages) * perPageEstimate
	if est.ApproxBytes != wantBytes {
		t.Errorf("ApproxBytes = %d, want %d (clamped to maxEstimatePages)", est.ApproxBytes, wantBytes)
	}
	if est.FullPass <= 0 {
		t.Errorf("FullPass = %v, want a positive bounded value", est.FullPass)
	}
}
