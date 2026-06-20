package obs

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics is the daemon's self-observability instrument layer: a bounded,
// cardinality-disciplined set of Prometheus instruments registered on a
// private, per-daemon registry. It is exposed (read-only) by the metrics
// listener and scraped by an operator's Prometheus.
//
// Cardinality discipline is non-negotiable: label values are closed enums
// (fetch/change classes, dispatch outcomes) or operator-config names
// (notifier) — never per-URL or per-site identifiers, and never a webhook URL
// or an error string. The whole label allow-list is {class, notifier, outcome,
// version}.
//
// A nil *Metrics is valid and no-ops on every method, so components can hold a
// *Metrics unconditionally and existing constructors/tests that pass nil need
// no changes.
type Metrics struct {
	reg *prometheus.Registry

	fetches       *prometheus.CounterVec // rabbot_fetches_total{class}
	fetchDuration prometheus.Histogram   // rabbot_fetch_duration_seconds
	changes       *prometheus.CounterVec // rabbot_changes_total{class}
	dispatched    *prometheus.CounterVec // rabbot_alerts_dispatched_total{notifier,outcome}
	digestDropped prometheus.Counter     // rabbot_digest_dropped_total
	dueURLs       prometheus.Gauge       // rabbot_due_urls
	dbSize        prometheus.Gauge       // rabbot_db_size_bytes
	// rabbot_crawls_in_flight and rabbot_build_info are GaugeFunc/const-gauge
	// collectors registered at construction; not held as fields.

	// inFlightFn is read on every scrape (HTTP server goroutine) and may be
	// installed at any time; atomic so call-site ordering never matters.
	inFlightFn atomic.Pointer[func() int]
}

// NewMetrics builds a *Metrics on a fresh private registry, registering the
// bounded rabbot_* instrument set plus the stock Go and process collectors.
// version labels rabbot_build_info (the only place it appears). The returned
// value is never nil; callers that want a no-op layer pass a literal nil
// *Metrics around instead.
func NewMetrics(version string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg}

	m.fetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rabbot_fetches_total",
		Help: "Total page fetches by access classification.",
	}, []string{"class"})

	m.fetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rabbot_fetch_duration_seconds",
		Help:    "Page fetch wall-clock duration in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	m.changes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rabbot_changes_total",
		Help: "Total detected changes by significance class.",
	}, []string{"class"})

	m.dispatched = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rabbot_alerts_dispatched_total",
		Help: "Total alert dispatch attempts by notifier and outcome.",
	}, []string{"notifier", "outcome"})

	m.digestDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rabbot_digest_dropped_total",
		Help: "Total digest entries dropped due to a full buffer.",
	})

	m.dueURLs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rabbot_due_urls",
		Help: "URLs currently due for a recheck (recheck backlog).",
	})

	m.dbSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rabbot_db_size_bytes",
		Help: "On-disk size of the database (main + WAL) in bytes.",
	})

	inFlight := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "rabbot_crawls_in_flight",
		Help: "Crawls currently in flight (concurrent page fetches).",
	}, func() float64 {
		fn := m.inFlightFn.Load()
		if fn == nil || *fn == nil {
			return 0
		}
		return float64((*fn)())
	})

	buildInfo := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "rabbot_build_info",
		Help:        "Build information; always 1, labelled by version.",
		ConstLabels: prometheus.Labels{"version": version},
	}, func() float64 { return 1 })

	reg.MustRegister(
		m.fetches,
		m.fetchDuration,
		m.changes,
		m.dispatched,
		m.digestDropped,
		m.dueURLs,
		m.dbSize,
		inFlight,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Registry returns the private registry backing this Metrics, for the metrics
// listener and tests. A nil *Metrics returns nil.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// ObserveFetch records one page fetch of the given class and its duration.
// class is a model.FetchClass value (ok|soft_block|hard_block|unreachable).
func (m *Metrics) ObserveFetch(class string, d time.Duration) {
	if m == nil {
		return
	}
	m.fetches.WithLabelValues(class).Inc()
	m.fetchDuration.Observe(d.Seconds())
}

// AddChanges adds n detected changes of the given class
// (cosmetic|substantive).
func (m *Metrics) AddChanges(class string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.changes.WithLabelValues(class).Add(float64(n))
}

// ObserveDispatch records one alert dispatch attempt through the single
// delivery funnel. notifier is the operator-config name (never a webhook URL).
// outcome is "ok" when err is nil, else "error" — the error itself is never
// used as a label value.
func (m *Metrics) ObserveDispatch(notifier string, err error) {
	if m == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.dispatched.WithLabelValues(notifier, outcome).Inc()
}

// AddDigestDropped records n digest entries dropped due to a full buffer.
// int64 matches the digest buffer's drop counter type — no narrowing at call sites.
func (m *Metrics) AddDigestDropped(n int64) {
	if m == nil || n == 0 {
		return
	}
	m.digestDropped.Add(float64(n))
}

// SetInFlightFunc installs the callback the rabbot_crawls_in_flight GaugeFunc
// reads on every scrape. The callback must be safe to call concurrently from
// the scrape path and must not touch the database. Passing nil restores the
// safe default (0).
func (m *Metrics) SetInFlightFunc(fn func() int) {
	if m == nil {
		return
	}
	m.inFlightFn.Store(&fn)
}

// SetDueURLs sets the rabbot_due_urls gauge (recheck backlog). Refreshed by
// the sampler goroutine off the scrape path — never from the scrape handler.
func (m *Metrics) SetDueURLs(n int) {
	if m == nil {
		return
	}
	m.dueURLs.Set(float64(n))
}

// SetDBSizeBytes sets the rabbot_db_size_bytes gauge. Refreshed by the sampler
// goroutine off the scrape path.
func (m *Metrics) SetDBSizeBytes(n int64) {
	if m == nil {
		return
	}
	m.dbSize.Set(float64(n))
}
