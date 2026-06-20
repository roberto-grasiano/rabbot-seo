package alerts

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// IncidentStore is the subset of *store.DB the pipeline calls. *store.DB satisfies
// it. The exported NewPipeline takes the concrete *store.DB (contract amendment
// §A/§E); the unexported newPipeline keeps this interface so unit tests can pass
// fakes.
type IncidentStore interface {
	GetOpenIncident(ctx context.Context, fingerprint string) (model.Alert, bool, error)
	OpenIncident(ctx context.Context, a model.Alert) (int64, error)
	UpdateIncident(ctx context.Context, a model.Alert) error
	CloseIncident(ctx context.Context, id int64, at time.Time, autoClosed bool) error

	// Member tracking lets a group incident (one incident keyed by
	// site+change_type+severity, eliding the URL) close only when its LAST member
	// URL recovers, instead of on the first member's recovery (Feature B).
	AddAlertMember(ctx context.Context, alertID int64, url string) error
	RemoveAlertMember(ctx context.Context, alertID int64, url string) (remaining int, err error)

	// HasOpenIncidentMember reports whether an open incident exists for fingerprint
	// with url already a tracked member. It backs Pipeline.HasOpenMember, the
	// idempotency probe a fixed-cadence evaluator uses to fire only on state change.
	HasOpenIncidentMember(ctx context.Context, fingerprint, url string) (bool, error)
}

// Dispatcher delivers a notify.Alert to routed notifiers (*notify.Dispatcher
// satisfies this).
type Dispatcher interface {
	Dispatch(ctx context.Context, a notify.Alert) error
}

// throttleKeyer is an OPTIONAL seam a Dispatcher may satisfy to expose the actual
// delivery target (routed notifier/channel name) for an alert. When the pipeline's
// Dispatcher implements it, the per-recipient hourly cap is keyed by that channel
// — so N sites funneling into one Slack channel share one bucket and the cap
// bounds messages-per-channel, as per_recipient_hourly_cap promises (F13). When a
// Dispatcher does NOT implement it (or returns ok=false), the throttle falls back
// to a site|severity bucket, preserving prior behavior. The live *notify.Dispatcher
// satisfies this (via notify.Registry.RouteTarget), so the cap protects the channel
// an operator targets.
type throttleKeyer interface {
	// ThrottleKey returns the routed delivery-target name for a and whether a target
	// was resolved (ok=false => no route matched; caller uses the fallback key).
	ThrottleKey(a notify.Alert) (string, bool)
}

// Caps holds the dedup window, per-recipient hourly cap, and incident auto-close
// duration. Applied via WithCaps.
type Caps struct {
	DedupWindow       time.Duration
	HourlyCap         int
	IncidentAutoClose time.Duration
}

// DigestSink receives over-cap / non-critical alerts for periodic digest delivery.
type DigestSink interface {
	Add(a notify.Alert)
}

// options is the resolved internal pipeline configuration.
type options struct {
	DedupWindow       time.Duration
	HourlyCap         int
	IncidentAutoClose time.Duration
	Now               func() time.Time
	Digest            DigestSink
	// DigestSeverities, when non-nil, restricts which over-cap non-critical
	// severities are buffered to the digest sink (alerting.digest.severities). A
	// nil set means "no severity filter" — every over-cap non-critical buffers.
	DigestSeverities map[model.Severity]bool
}

// PipelineOption configures the pipeline (functional-options form, contract §E).
type PipelineOption func(*options)

// WithCaps sets the dedup window, per-recipient hourly cap, and incident auto-close.
func WithCaps(c Caps) PipelineOption {
	return func(o *options) {
		o.DedupWindow = c.DedupWindow
		o.HourlyCap = c.HourlyCap
		o.IncidentAutoClose = c.IncidentAutoClose
	}
}

// WithClock injects a deterministic clock (defaults to time.Now).
func WithClock(now func() time.Time) PipelineOption {
	return func(o *options) { o.Now = now }
}

// WithDigestSink routes over-cap / non-critical alerts to a digest sink.
func WithDigestSink(sink DigestSink) PipelineOption {
	return func(o *options) { o.Digest = sink }
}

// WithDigestSeverities restricts which over-cap non-critical severities are
// buffered to the digest sink (wires alerting.digest.severities). An empty/nil
// list means "no filter" — every over-cap non-critical is buffered. A severity
// not in the list is dropped from the digest rather than buffered (F15).
func WithDigestSeverities(sevs []model.Severity) PipelineOption {
	return func(o *options) {
		if len(sevs) == 0 {
			o.DigestSeverities = nil
			return
		}
		m := make(map[model.Severity]bool, len(sevs))
		for _, s := range sevs {
			m[s] = true
		}
		o.DigestSeverities = m
	}
}

// Pipeline is the incident state machine.
type Pipeline struct {
	store IncidentStore
	disp  Dispatcher
	opts  options
	th    *throttle

	// locks serializes the read-modify-write on a single incident identity. The
	// crawl scheduler dispatches up to MaxParallel fetches concurrently, and two
	// URLs sharing a group fingerprint (site+change_type+severity) would otherwise
	// both observe "no open incident" in GetOpenIncident and both OpenIncident —
	// a logical race the -race detector cannot see (it is on DB state, not memory).
	// Keyed by fingerprint so distinct incidents never contend.
	//
	// The keyspace is bounded by the number of distinct live incident fingerprints
	// (site+change_type+severity) — a small, operator-controlled set — so the map
	// never grows with traffic and unbounded growth is not a practical concern;
	// no eviction is needed.
	locks sync.Map // fingerprint -> *sync.Mutex
}

// lockFor returns the unlock func for the per-fingerprint mutex, acquired.
func (p *Pipeline) lockFor(fingerprint string) func() {
	mi, _ := p.locks.LoadOrStore(fingerprint, &sync.Mutex{})
	m := mi.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// NewPipeline builds a Pipeline over the concrete *store.DB and *notify.Dispatcher
// (contract amendment §E). Thresholds, clock, and digest sink are supplied via
// functional options: WithCaps, WithClock, WithDigestSink.
func NewPipeline(db *store.DB, dispatcher *notify.Dispatcher, opts ...PipelineOption) *Pipeline {
	return newPipeline(db, dispatcher, opts...)
}

// newPipeline is the interface-typed constructor used by unit tests (fakes).
func newPipeline(st IncidentStore, disp Dispatcher, opts ...PipelineOption) *Pipeline {
	o := options{Now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	p := &Pipeline{store: st, disp: disp, opts: o}
	p.th = newThrottle(o.HourlyCap, o.Now)
	return p
}

// Ingest processes one detected Event:
//   - resolve the incident identity (groupFingerprint: site+change_type+severity)
//   - if an open incident exists and the last notification is within DedupWindow,
//     accrue the affected count but do NOT re-notify (dedup)
//   - if open but outside the window, accrue + re-notify (incident update)
//   - if none open, open a new incident and notify
//
// Criticals bypass the hourly cap; non-criticals over the cap accrue silently
// (the digest path picks them up — see Task 11).
func (p *Pipeline) Ingest(ctx context.Context, e Event) error {
	now := p.opts.Now()
	fp := groupFingerprint(e)

	// Serialize the get-then-open/update for this fingerprint so concurrent crawl
	// workers cannot open duplicate incidents for the same identity.
	defer p.lockFor(fp)()

	cur, open, err := p.store.GetOpenIncident(ctx, fp)
	if err != nil {
		return err
	}

	if open {
		// Record this URL as a member of the open incident so the close path can
		// wait for the LAST member to recover (Feature B). Idempotent in the store
		// (INSERT OR IGNORE), so a URL that keeps failing across cycles is recorded
		// once. This runs under lockFor(fp) above, serialized against Resolve's
		// read-modify of the same member set.
		if err := p.addMember(ctx, cur.ID, e.URL); err != nil {
			return err
		}
		cur.AffectedCount++
		cur.LastUpdatedAt = now
		withinWindow := cur.LastNotifiedAt != nil && now.Sub(*cur.LastNotifiedAt) < p.opts.DedupWindow
		if withinWindow {
			return p.store.UpdateIncident(ctx, cur) // dedup: accrue only
		}
		// Re-notify as an incident update.
		t := now
		cur.LastNotifiedAt = &t
		if err := p.store.UpdateIncident(ctx, cur); err != nil {
			return err
		}
		return p.maybeDispatch(ctx, e, cur, now)
	}

	// New incident.
	t := now
	inc := model.Alert{
		SiteID:          e.SiteID,
		Fingerprint:     fp,
		GroupKey:        GroupKey(e.Site, e.ChangeType),
		Severity:        e.Severity,
		Status:          model.AlertOpen,
		AffectedCount:   1,
		FirstDetectedAt: now,
		LastUpdatedAt:   now,
		LastNotifiedAt:  &t,
		PayloadSummary:  payloadSummary(e.ChangeType),
	}
	id, err := p.store.OpenIncident(ctx, inc)
	if err != nil {
		return err
	}
	inc.ID = id
	// Record the URL that opened the incident as its first member (Feature B).
	if err := p.addMember(ctx, inc.ID, e.URL); err != nil {
		return err
	}
	return p.maybeDispatch(ctx, e, inc, now)
}

// addMember records url as a live member of incident alertID, skipping events
// that carry no URL (site-level events) — an empty-string "member" would be
// meaningless and would never be removed by a recovery. Idempotent in the store.
func (p *Pipeline) addMember(ctx context.Context, alertID int64, url string) error {
	if url == "" {
		return nil
	}
	return p.store.AddAlertMember(ctx, alertID, url)
}

// maybeDispatch gates delivery on the per-recipient hourly throttle. Non-criticals
// over the cap are accrued (incident already persisted) and, when a digest sink is
// configured and the severity is in the digest set, routed to it for periodic
// delivery. With NO digest sink, an over-cap non-critical is delivered immediately
// instead of dropped — losing the cap is safer than silently losing the alert (F14).
func (p *Pipeline) maybeDispatch(ctx context.Context, e Event, inc model.Alert, now time.Time) error {
	na := toNotifyAlert(e, inc, now)
	if !p.th.allow(p.throttleKey(e, na), e.Severity) {
		// Over cap: incident already persisted.
		if p.opts.Digest != nil {
			// Buffer to the digest only if the severity is in the configured digest
			// set (nil set => buffer everything). A non-digested over-cap severity
			// is suppressed (the cap deliberately silenced it).
			if p.opts.DigestSeverities == nil || p.opts.DigestSeverities[e.Severity] {
				p.opts.Digest.Add(na)
			}
			return nil
		}
		// No digest sink configured: degrade to immediate delivery so the alert is
		// never silently dropped.
		return p.disp.Dispatch(ctx, na)
	}
	return p.disp.Dispatch(ctx, na)
}

// throttleKey returns the per-recipient bucket key for the hourly cap. When the
// Dispatcher exposes the routed delivery target (throttleKeyer), the cap is keyed
// by that channel so many sites funneling into one channel share one bucket and
// the cap bounds messages-per-channel (F13). Otherwise it falls back to a
// site|severity bucket.
func (p *Pipeline) throttleKey(e Event, na notify.Alert) string {
	if k, ok := p.disp.(throttleKeyer); ok {
		if name, resolved := k.ThrottleKey(na); resolved {
			return name
		}
	}
	return GroupKey(e.Site, string(e.Severity))
}

// Resolve recovers this event's URL from the group incident for its identity (a
// group incident's fingerprint elides the URL, so all pages sharing
// site+change_type+severity map to one incident). The incident is closed only
// when its LAST member URL recovers (Feature B): RemoveAlertMember removes this
// URL and returns how many members remain; CloseIncident runs only when
// remaining==0, so a still-broken sibling keeps the incident open and no spurious
// resolve is dispatched.
//
// Fallback for legacy/empty incidents: an incident opened before member tracking
// (or whose member set was never populated) has no alert_members rows, so
// RemoveAlertMember returns remaining==0 and the incident closes immediately on
// the first Resolve — old incidents are never stranded open.
//
// The whole read-modify (RemoveAlertMember + the remaining check + CloseIncident)
// runs under the per-fingerprint lock, the same serialization as Ingest, so
// concurrent resolves of different URLs of one incident cannot race the count and
// the count cannot race a concurrent open/update of the same identity.
func (p *Pipeline) Resolve(ctx context.Context, e Event) error {
	now := p.opts.Now()
	fp := groupFingerprint(e)
	defer p.lockFor(fp)()
	cur, open, err := p.store.GetOpenIncident(ctx, fp)
	if err != nil {
		return err
	}
	if !open {
		return nil
	}
	remaining, err := p.store.RemoveAlertMember(ctx, cur.ID, e.URL)
	if err != nil {
		return err
	}
	if remaining > 0 {
		// A sibling URL is still broken — keep the incident open, do not dispatch a
		// resolve.
		return nil
	}
	return p.store.CloseIncident(ctx, cur.ID, now, false)
}

// HasOpenMember reports whether e's URL is already a tracked member of an open
// incident for e's identity (the group fingerprint site+change_type+severity). It is
// the idempotency probe for a fixed-cadence signal evaluator (the daily GSC pull):
// when true, the evaluator skips re-Ingesting a still-firing per-URL signal so the
// incident is notified ONCE and stays quiet until it resolves and recurs, instead of
// re-paging every tick (the daily-re-page noise the per-event DedupWindow, ~minutes,
// cannot suppress against a day-long cadence). It returns false when no incident is
// open for the identity OR e's URL is not yet a member — so a newly-affected URL still
// Ingests (registering membership and notifying). It computes the same groupFingerprint
// Ingest/Resolve use, keeping the incident identity the single source of truth.
func (p *Pipeline) HasOpenMember(ctx context.Context, e Event) (bool, error) {
	if e.URL == "" {
		return false, nil
	}
	return p.store.HasOpenIncidentMember(ctx, groupFingerprint(e), e.URL)
}

// payloadSummary builds the incident's JSON payload summary. It uses encoding/json
// so a change_type containing a quote (or any other special character) is escaped
// rather than producing malformed JSON via string concatenation.
func payloadSummary(changeType string) string {
	b, err := json.Marshal(map[string]string{"change_type": changeType})
	if err != nil {
		// Unreachable for string values, but never emit broken JSON on the off
		// chance: fall back to an empty object.
		return "{}"
	}
	return string(b)
}

func toNotifyAlert(e Event, inc model.Alert, now time.Time) notify.Alert {
	related := inc.AffectedCount - 1
	if related < 0 {
		related = 0
	}
	return notify.Alert{
		Site:         e.Site,
		URL:          e.URL,
		ChangeType:   e.ChangeType,
		Severity:     e.Severity,
		Before:       e.Before,
		After:        e.After,
		DetectedAt:   now,
		GroupKey:     inc.GroupKey,
		RelatedCount: related,
		DeepLink:     e.DeepLink,
		Operational:  e.Operational,
		Segments:     e.Segments,
	}
}
