package obs_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// Criterion 1: ObserveFetch increments rabbot_fetches_total{class} and observes
// the duration histogram exactly once.
func TestMetrics_ObserveFetch(t *testing.T) {
	m := obs.NewMetrics("v1.2.3")

	m.ObserveFetch("soft_block", 250*time.Millisecond)

	const want = `
		# HELP rabbot_fetches_total Total page fetches by access classification.
		# TYPE rabbot_fetches_total counter
		rabbot_fetches_total{class="soft_block"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_fetches_total"); err != nil {
		t.Errorf("rabbot_fetches_total mismatch:\n%s", err)
	}

	// The histogram observed exactly one sample.
	if got := testutil.CollectAndCount(m.Registry(), "rabbot_fetch_duration_seconds"); got != 1 {
		t.Errorf("rabbot_fetch_duration_seconds: got %d collected metrics, want 1", got)
	}
	if got := gatherSampleCount(t, m, "rabbot_fetch_duration_seconds"); got != 1 {
		t.Errorf("rabbot_fetch_duration_seconds sample count = %d, want 1", got)
	}
}

func TestMetrics_AddChanges(t *testing.T) {
	m := obs.NewMetrics("v0")
	m.AddChanges("cosmetic", 1)
	m.AddChanges("substantive", 2)

	const want = `
		# HELP rabbot_changes_total Total detected changes by significance class.
		# TYPE rabbot_changes_total counter
		rabbot_changes_total{class="cosmetic"} 1
		rabbot_changes_total{class="substantive"} 2
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_changes_total"); err != nil {
		t.Errorf("rabbot_changes_total mismatch:\n%s", err)
	}
}

func TestMetrics_ObserveDispatch(t *testing.T) {
	m := obs.NewMetrics("v0")
	m.ObserveDispatch("slack-primary", nil)
	m.ObserveDispatch("slack-primary", errors.New("503 from webhook"))
	m.ObserveDispatch("ops-webhook", nil)

	const want = `
		# HELP rabbot_alerts_dispatched_total Total alert dispatch attempts by notifier and outcome.
		# TYPE rabbot_alerts_dispatched_total counter
		rabbot_alerts_dispatched_total{notifier="ops-webhook",outcome="ok"} 1
		rabbot_alerts_dispatched_total{notifier="slack-primary",outcome="error"} 1
		rabbot_alerts_dispatched_total{notifier="slack-primary",outcome="ok"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_alerts_dispatched_total"); err != nil {
		t.Errorf("rabbot_alerts_dispatched_total mismatch:\n%s", err)
	}
}

// The error string must never leak into exposition as a label value: the
// outcome is the closed enum "error", and no gathered label value contains the
// secret.
func TestMetrics_ObserveDispatch_NoErrorLeak(t *testing.T) {
	m := obs.NewMetrics("v0")
	secret := "https://hooks.example.com/services/SECRET/TOKEN"
	m.ObserveDispatch("n1", errors.New(secret))

	const want = `
		# HELP rabbot_alerts_dispatched_total Total alert dispatch attempts by notifier and outcome.
		# TYPE rabbot_alerts_dispatched_total counter
		rabbot_alerts_dispatched_total{notifier="n1",outcome="error"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_alerts_dispatched_total"); err != nil {
		t.Errorf("rabbot_alerts_dispatched_total mismatch:\n%s", err)
	}

	// Belt-and-braces: no label value across the whole registry contains the secret.
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if strings.Contains(lp.GetValue(), secret) {
					t.Fatalf("secret leaked as label value on metric %q label %q", fam.GetName(), lp.GetName())
				}
			}
		}
	}
}

func TestMetrics_AddDigestDropped(t *testing.T) {
	m := obs.NewMetrics("v0")
	m.AddDigestDropped(3)
	m.AddDigestDropped(2)

	const want = `
		# HELP rabbot_digest_dropped_total Total digest entries dropped due to a full buffer.
		# TYPE rabbot_digest_dropped_total counter
		rabbot_digest_dropped_total 5
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_digest_dropped_total"); err != nil {
		t.Errorf("rabbot_digest_dropped_total mismatch:\n%s", err)
	}
}

func TestMetrics_BuildInfo(t *testing.T) {
	m := obs.NewMetrics("v9.9.9")

	const want = `
		# HELP rabbot_build_info Build information; always 1, labelled by version.
		# TYPE rabbot_build_info gauge
		rabbot_build_info{version="v9.9.9"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_build_info"); err != nil {
		t.Errorf("rabbot_build_info mismatch:\n%s", err)
	}
}

func TestMetrics_SetInFlightFunc(t *testing.T) {
	m := obs.NewMetrics("v0")
	m.SetInFlightFunc(func() int { return 7 })

	const want = `
		# HELP rabbot_crawls_in_flight Crawls currently in flight (concurrent page fetches).
		# TYPE rabbot_crawls_in_flight gauge
		rabbot_crawls_in_flight 7
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_crawls_in_flight"); err != nil {
		t.Errorf("rabbot_crawls_in_flight mismatch:\n%s", err)
	}
}

// A nil in-flight func must collect a safe default (0), never panic.
func TestMetrics_InFlight_NilFuncSafe(t *testing.T) {
	m := obs.NewMetrics("v0")
	const want = `
		# HELP rabbot_crawls_in_flight Crawls currently in flight (concurrent page fetches).
		# TYPE rabbot_crawls_in_flight gauge
		rabbot_crawls_in_flight 0
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_crawls_in_flight"); err != nil {
		t.Errorf("rabbot_crawls_in_flight mismatch:\n%s", err)
	}
}

func TestMetrics_SamplerGauges(t *testing.T) {
	m := obs.NewMetrics("v0")
	m.SetDueURLs(42)
	m.SetDBSizeBytes(1 << 20)

	const want = `
		# HELP rabbot_db_size_bytes On-disk size of the database (main + WAL) in bytes.
		# TYPE rabbot_db_size_bytes gauge
		rabbot_db_size_bytes 1.048576e+06
		# HELP rabbot_due_urls URLs currently due for a recheck (recheck backlog).
		# TYPE rabbot_due_urls gauge
		rabbot_due_urls 42
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want),
		"rabbot_due_urls", "rabbot_db_size_bytes"); err != nil {
		t.Errorf("sampler gauges mismatch:\n%s", err)
	}
}

// Criterion 1: every method on a nil *Metrics no-ops (no panic).
func TestMetrics_NilSafe(t *testing.T) {
	var m *obs.Metrics // nil

	// Must not panic.
	m.ObserveFetch("ok", time.Second)
	m.AddChanges("cosmetic", 5)
	m.ObserveDispatch("n", nil)
	m.ObserveDispatch("n", errors.New("x"))
	m.AddDigestDropped(9)
	m.SetInFlightFunc(func() int { return 1 })
	m.SetDueURLs(3)
	m.SetDBSizeBytes(123)

	if m.Registry() != nil {
		t.Errorf("nil *Metrics Registry() = non-nil, want nil")
	}
}

// Criterion 2 (cardinality guard): label names across every non-stock metric
// family are a subset of the closed allow-list {class, notifier, outcome,
// version}. A stray per-URL or per-site label fails the build.
func TestMetrics_CardinalityGuard(t *testing.T) {
	allowed := map[string]bool{
		"class":    true,
		"notifier": true,
		"outcome":  true,
		"version":  true,
	}

	m := obs.NewMetrics("vX")
	// Populate every vector so its family is emitted by Gather.
	m.ObserveFetch("ok", time.Second)
	m.AddChanges("substantive", 1)
	m.ObserveDispatch("n", nil)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	for _, fam := range families {
		name := fam.GetName()
		// Stock Go/process collectors are exempt from the rabbot allow-list.
		if !strings.HasPrefix(name, "rabbot_") {
			continue
		}
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				ln := lp.GetName()
				if !allowed[ln] {
					t.Errorf("metric %q carries disallowed label %q (allow-list: class,notifier,outcome,version)", name, ln)
				}
			}
		}
	}
}

// The stock Go and process collectors must be registered.
func TestMetrics_StockCollectors(t *testing.T) {
	m := obs.NewMetrics("v0")
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := make(map[string]bool, len(families))
	for _, fam := range families {
		got[fam.GetName()] = true
	}
	for _, want := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !got[want] {
			t.Errorf("stock collector metric %q missing from registry", want)
		}
	}
}

// --- helpers ---

func gatherSampleCount(t *testing.T, m *obs.Metrics, name string) uint64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, met := range fam.GetMetric() {
			if h := met.GetHistogram(); h != nil {
				return h.GetSampleCount()
			}
		}
	}
	return 0
}

// TestMetrics_SetInFlightFuncConcurrentWithScrape pins the atomic contract from
// the PR #77 review: installing the in-flight callback while scrapes are in
// progress must be race-free regardless of call-site ordering (run under -race).
func TestMetrics_SetInFlightFuncConcurrentWithScrape(t *testing.T) {
	m := obs.NewMetrics("test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			m.SetInFlightFunc(func() int { return i })
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := m.Registry().Gather(); err != nil {
			t.Fatalf("gather: %v", err)
		}
	}
	<-done
}
