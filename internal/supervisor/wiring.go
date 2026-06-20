package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// AlertingStack bundles the constructed M2 components for the daemon.
type AlertingStack struct {
	Registry  notify.Registry
	Pipeline  *alerts.Pipeline
	Processor *scheduler.Processor

	// DigestFlush drains the internal digest buffer (over-cap / non-critical
	// alerts accrued by the pipeline) and dispatches each buffered alert via the
	// local dispatcher. The daemon wires it as the digest tick passed to
	// Pipeline.RegisterTimers. It stops early if ctx is cancelled.
	DigestFlush func(ctx context.Context)

	// digestSink is the supervisor-owned digest buffer captured by DigestFlush. It
	// is unexported (internal observability/test seam only) — the daemon drives the
	// buffer through the pipeline, never directly.
	digestSink *digestBuffer
}

// StackOption configures the alerting stack at construction. It is the additive
// seam for wiring optional cross-cutting concerns (e.g. self-observability)
// without changing BuildAlertingStack's positional signature, so every existing
// caller compiles unchanged.
type StackOption func(*stackConfig)

// stackConfig accumulates the optional construction settings.
type stackConfig struct {
	metrics     *obs.Metrics
	blastRadius func(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool)
}

// WithStackMetrics injects the daemon's self-observability layer. The single
// delivery funnel (the dispatcher) then records rabbot_alerts_dispatched_total by
// notifier config name, and each DigestFlush mirrors the buffer's drop count into
// rabbot_digest_dropped_total. A nil *Metrics no-ops throughout.
func WithStackMetrics(m *obs.Metrics) StackOption {
	return func(sc *stackConfig) { sc.metrics = m }
}

// WithStackBlastRadius threads the A9 link-graph enrichment lookup into the
// Processor it constructs, so a critical http_status alert (status >= 400) gains
// the broken page's inlink blast radius. The Processor is built INSIDE
// BuildAlertingStack (not in run.go), so this option is how run.go reaches the
// Processor's WithBlastRadius seam without exposing the Processor's construction.
// The func reads only the store (no sink needed), so run.go can build it from a
// sink-less Grapher before the full crawl-hook Grapher is assembled. A nil func
// (the graph feature disabled) leaves alerts un-enriched — the scope-gate
// severability.
func WithStackBlastRadius(fn func(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool)) StackOption {
	return func(sc *stackConfig) { sc.blastRadius = fn }
}

// defaultMaxPending caps the digest buffer so a noisy fleet (or a misconfigured
// rule firing on every crawl) cannot grow an unbounded slice of notify.Alerts —
// each of which carries Before/After content strings — between hourly flushes.
const defaultMaxPending = 10000

// digestBuffer is the supervisor-owned alerts.DigestSink: it accumulates
// over-cap / non-critical notify.Alerts the pipeline routes to it, draining them
// on demand (DigestFlush) for periodic delivery via the local dispatcher.
type digestBuffer struct {
	mu      sync.Mutex
	pending []notify.Alert
	max     int   // cap on pending; <=0 means defaultMaxPending
	dropped int64 // count of alerts dropped to honor the cap (observable)
}

// cap returns the effective maximum pending size.
func (b *digestBuffer) cap() int {
	if b.max <= 0 {
		return defaultMaxPending
	}
	return b.max
}

// Add implements alerts.DigestSink; safe for concurrent crawl workers. When the
// buffer is at capacity it drops the oldest entry (drop-oldest) and increments a
// dropped counter so the loss is observable, rather than growing without bound.
func (b *digestBuffer) Add(a notify.Alert) {
	b.mu.Lock()
	defer b.mu.Unlock()
	max := b.cap()
	if len(b.pending) >= max {
		// Drop oldest entries until there's room for one more.
		drop := len(b.pending) - max + 1
		b.pending = append(b.pending[:0], b.pending[drop:]...)
		b.dropped += int64(drop)
	}
	b.pending = append(b.pending, a)
}

// popFront removes and returns the oldest buffered alert under the lock. ok is
// false when the buffer is empty. Popping one at a time (rather than draining the
// whole slice up front) lets DigestFlush observe a cancelled ctx without losing
// the not-yet-dispatched remainder — they stay buffered for the next flush.
func (b *digestBuffer) popFront() (notify.Alert, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return notify.Alert{}, false
	}
	a := b.pending[0]
	// O(1) pop: advance the slice head instead of shifting the whole backing array
	// (the front-shift made a full DigestFlush O(n^2)). Zero the popped slot so the
	// alert can be GC'd, and reset to nil once empty to release the backing array.
	b.pending[0] = notify.Alert{}
	b.pending = b.pending[1:]
	if len(b.pending) == 0 {
		b.pending = nil
	}
	return a, true
}

// drain returns the buffered alerts and resets the buffer, under the lock.
func (b *digestBuffer) drain() []notify.Alert {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return nil
	}
	out := b.pending
	b.pending = nil
	return out
}

// takeDropped returns the number of alerts dropped to honor the cap since the
// last call and resets the counter, under the lock. This lets DigestFlush report
// only newly-dropped alerts each flush rather than re-logging a cumulative total.
func (b *digestBuffer) takeDropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.dropped
	b.dropped = 0
	return d
}

// Compile-time assertion that digestBuffer satisfies the alerts sink contract.
var _ alerts.DigestSink = (*digestBuffer)(nil)

// parseDur parses a config duration string, falling back to def on error/empty.
// A malformed (non-empty) value is logged at Warn via log (when non-nil) so the
// silent fallback to a default becomes observable: an operator who typos '7day'
// for '7d' would otherwise see alerting behavior silently diverge from config.
func parseDur(s string, def time.Duration, log *slog.Logger, key string) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		if log != nil {
			log.Warn("invalid alerting duration; using default", obs.KeyComponent, "alerts",
				"key", key, "value", s, "default", def.String(), obs.KeyError, err.Error())
		}
		return def
	}
	return d
}

// BuildAlertingStack constructs the notify registry, alerts pipeline, rules
// engine adapter, and post-fetch processor from config + the store. log (may be
// nil) records digest-delivery failures during DigestFlush.
// segLookup is the A7 hot-path segment-name lookup (registry-backed, in-memory):
// given a site id + URL it returns the segment names the URL belongs to, used to
// annotate emitted alert events for segment-based route matching. nil disables
// segment annotation (events carry no segments).
type segLookup func(siteID int64, url string) []string

func BuildAlertingStack(cfg config.Config, st *store.DB, client *http.Client, now func() time.Time, log *slog.Logger, segmentsFor segLookup, opts ...StackOption) (*AlertingStack, error) {
	if now == nil {
		now = time.Now
	}
	var sc stackConfig
	for _, opt := range opts {
		opt(&sc)
	}

	// Notifiers. This construction switch is the SINGLE point of concrete-type
	// knowledge (A1 design): each case builds one backend from its config. The
	// notify constructors validate per-type required fields and fail HERE, at
	// startup, naming the notifier and never echoing a secret — so an incomplete
	// email/webhook config fails daemon launch, not first send. The public type
	// strings are pinned in config (config.NotifierType*).
	byName := make(map[string]notify.Notifier, len(cfg.Notifiers))
	for _, nc := range cfg.Notifiers {
		switch nc.Type {
		case config.NotifierTypeSlack:
			byName[nc.Name] = notify.NewSlackNotifier(nc.Name, nc.URL, client)
		case config.NotifierTypeEmail:
			n, err := notify.NewEmailNotifier(notify.EmailConfig{
				Name:           nc.Name,
				Host:           nc.SMTPHost,
				Port:           nc.SMTPPort,
				Username:       nc.Username,
				Password:       nc.Password,
				From:           nc.From,
				To:             nc.To,
				AllowPlaintext: nc.AllowPlaintext,
			})
			if err != nil {
				return nil, err
			}
			byName[nc.Name] = n
		case config.NotifierTypeWebhook:
			n, err := notify.NewWebhookNotifier(nc.Name, nc.URL, nc.Headers, client)
			if err != nil {
				return nil, err
			}
			byName[nc.Name] = n
		default:
			return nil, fmt.Errorf("rabbot: unknown notifier type %q for %q", nc.Type, nc.Name)
		}
	}
	registry := notify.NewRegistry(byName, cfg.Routes)
	// The dispatcher is the SINGLE delivery funnel (pipeline + digest flush), so
	// wiring metrics here observes every push outcome by notifier config name.
	dispatcher := notify.NewDispatcher(registry, notify.WithMetrics(sc.metrics))

	// Digest buffer: over-cap / non-critical alerts accrue here and are flushed
	// on the digest tick instead of being dropped.
	buf := &digestBuffer{}

	// Alerts pipeline.
	pipeline := alerts.NewPipeline(st, dispatcher,
		alerts.WithCaps(alerts.Caps{
			DedupWindow:       parseDur(cfg.Alerting.DedupWindow, 5*time.Minute, log, "alerting.dedup_window"),
			HourlyCap:         cfg.Alerting.PerRecipientHourlyCap,
			IncidentAutoClose: parseDur(cfg.Alerting.IncidentAutoCloseAfter, 24*time.Hour, log, "alerting.incident_auto_close_after"),
		}),
		alerts.WithClock(now),
		alerts.WithDigestSink(buf),
	)

	// Rules engine.
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, now)

	// Processor deps adapter (bridges store + engine + pipeline to ProcessorDeps).
	deps := &procDeps{store: st, engine: engine, pipeline: pipeline, now: now}
	var procOpts []scheduler.ProcessorOption
	if segmentsFor != nil {
		procOpts = append(procOpts, scheduler.WithSegmentsFor(segmentsFor))
	}
	if sc.metrics != nil {
		procOpts = append(procOpts, scheduler.WithMetrics(sc.metrics))
	}
	if sc.blastRadius != nil {
		procOpts = append(procOpts, scheduler.WithBlastRadius(sc.blastRadius))
	}
	processor := scheduler.NewProcessor(deps, diff.DefaultSimhashThreshold, now, procOpts...)

	return &AlertingStack{
		Registry:   registry,
		Pipeline:   pipeline,
		Processor:  processor,
		digestSink: buf,
		DigestFlush: func(ctx context.Context) {
			// Check ctx BEFORE touching the buffer: a cancelled ctx (e.g. daemon
			// shutdown) leaves every buffered alert intact for the next flush.
			if ctx.Err() != nil {
				return
			}
			if dropped := buf.takeDropped(); dropped > 0 {
				// The buffer hit its cap since the last flush — surface the loss both
				// in the log and as rabbot_digest_dropped_total (nil metrics no-ops).
				sc.metrics.AddDigestDropped(dropped)
				if log != nil {
					log.Warn("digest buffer overflow: dropped alerts", obs.KeyComponent, "alerts",
						"dropped", dropped)
				}
			}
			// Pop one at a time so a mid-flush cancellation loses zero alerts: the
			// not-yet-dispatched remainder stays buffered for the next flush.
			for {
				if ctx.Err() != nil {
					return
				}
				a, ok := buf.popFront()
				if !ok {
					return
				}
				if derr := dispatcher.Dispatch(ctx, a); derr != nil && log != nil {
					// Don't drop digest-delivery failures silently — a transient
					// Slack outage during the hourly flush would otherwise be invisible.
					log.Error("digest delivery failed", obs.KeyComponent, "alerts",
						"site", a.Site, "change_type", a.ChangeType, obs.KeyError, derr.Error())
				}
			}
		},
	}, nil
}

// SitemapURLStore adapts the concrete *store.DB to scheduler.URLStore (the sitemap
// watch's reconcile + live-coverage dep). The adapter lives here because store
// cannot import scheduler (scheduler imports store — that would cycle), so the
// scheduler.SitemapLiveCounts return type is produced on this side of the seam.
type SitemapURLStore struct {
	DB *store.DB
}

// ReconcileSitemapMembership passes through to the store.
func (s SitemapURLStore) ReconcileSitemapMembership(ctx context.Context, siteID int64, locs []string, additiveOnly bool) error {
	return s.DB.ReconcileSitemapMembership(ctx, siteID, locs, additiveOnly)
}

// SitemapLiveCounts adapts the store's result struct to the scheduler-side type.
func (s SitemapURLStore) SitemapLiveCounts(ctx context.Context, siteID int64) (scheduler.SitemapLiveCounts, error) {
	c, err := s.DB.SitemapLiveCounts(ctx, siteID)
	if err != nil {
		return scheduler.SitemapLiveCounts{}, err
	}
	return scheduler.SitemapLiveCounts{
		SitemappedUncrawled: c.SitemappedUncrawled,
		CrawledNotInSitemap: c.CrawledNotInSitemap,
		InSitemapTotal:      c.InSitemapTotal,
	}, nil
}

// Compile-time assertion that SitemapURLStore satisfies the sitemap watch seam.
var _ scheduler.URLStore = SitemapURLStore{}

// procDeps adapts the concrete store/engine/pipeline to scheduler.ProcessorDeps.
type procDeps struct {
	store    *store.DB
	engine   *rules.Engine
	pipeline *alerts.Pipeline
	// now is the daemon clock (injectable for tests), used to stamp health-score
	// rows; nil falls back to time.Now. store.RecordHealthScores re-UTCs it.
	now func() time.Time
}

func (d *procDeps) RecordChanges(ctx context.Context, changes []model.Change) error {
	return d.store.RecordChanges(ctx, changes)
}

// ApplyRules evaluates the rule set and returns the findings the engine NEWLY opened
// this crawl (Feature A). It snapshots the URL's open issues before applying, then
// diffs the open set after: any rule_id newly present is a freshly-opened finding the
// scheduler bridges into the alert path. An already-open (refreshed) finding is NOT
// returned — it already alerted on the crawl it opened.
func (d *procDeps) ApplyRules(ctx context.Context, urlID int64, importance float64, newSnap, oldSnap model.Snapshot, changes []model.Change, truncated bool) ([]scheduler.NewFinding, error) {
	urlIDp := urlID
	before, err := d.store.ListIssues(ctx, store.IssueFilter{URLID: &urlIDp, OpenOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list open issues (pre-apply): %w", err)
	}
	wasOpen := make(map[string]bool, len(before))
	for _, iss := range before {
		wasOpen[iss.RuleID] = true
	}
	if err := d.engine.Apply(ctx, rules.EvalContext{
		URLID: urlID, Importance: importance, New: newSnap, Old: oldSnap, Changes: changes, Truncated: truncated,
	}); err != nil {
		return nil, err
	}
	after, err := d.store.ListIssues(ctx, store.IssueFilter{URLID: &urlIDp, OpenOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list open issues (post-apply): %w", err)
	}
	var newly []scheduler.NewFinding
	for _, iss := range after {
		if wasOpen[iss.RuleID] {
			continue
		}
		field, ok := scheduler.BridgeFieldForRule(iss.RuleID)
		if !ok {
			field = iss.RuleID
		}
		newly = append(newly, scheduler.NewFinding{Field: field, Severity: iss.Severity, Detail: iss.Detail})
	}
	return newly, nil
}

func (d *procDeps) HandleFetchClass(ctx context.Context, ac alerts.AccessContext, seo []alerts.Event) (bool, error) {
	return d.pipeline.HandleFetchClass(ctx, ac, seo)
}

func (d *procDeps) IngestEvent(ctx context.Context, e alerts.Event) error {
	return d.pipeline.Ingest(ctx, e)
}

func (d *procDeps) ResolveEvent(ctx context.Context, e alerts.Event) error {
	return d.pipeline.Resolve(ctx, e)
}

// RecordHealthScore (A6) recomputes and (on change) persists the health score for
// the URL's site and the segments containing urlID, via store.RecordHealthScores.
// Stamped with the daemon clock (UTC re-applied by the store) so test clocks and
// dedup timestamps stay coherent with the rest of the pipeline.
func (d *procDeps) RecordHealthScore(ctx context.Context, siteID, urlID int64) error {
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	return d.store.RecordHealthScores(ctx, siteID, urlID, now())
}

// Compile-time assertion that procDeps satisfies the scheduler seam contract.
var _ scheduler.ProcessorDeps = (*procDeps)(nil)
