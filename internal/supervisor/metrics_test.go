package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// Slice 4: the digest buffer's takeDropped count is mirrored into
// rabbot_digest_dropped_total on each flush. A cap-1 buffer fed 3 alerts drops 2;
// after a flush the gauge reflects exactly the dropped count.
func TestDigestBuffer_DropsMirrorMetric(t *testing.T) {
	m := obs.NewMetrics("vtest")
	buf := &digestBuffer{max: 1}

	// Three adds into a cap-1 buffer => 2 drop-oldest events.
	buf.Add(notify.Alert{Site: "a"})
	buf.Add(notify.Alert{Site: "b"})
	buf.Add(notify.Alert{Site: "c"})

	if dropped := buf.takeDropped(); dropped > 0 {
		m.AddDigestDropped(dropped)
	}

	const want = `
		# HELP rabbot_digest_dropped_total Total digest entries dropped due to a full buffer.
		# TYPE rabbot_digest_dropped_total counter
		rabbot_digest_dropped_total 2
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_digest_dropped_total"); err != nil {
		t.Errorf("rabbot_digest_dropped_total mismatch:\n%s", err)
	}
}

// Slice 4 (integration): a metered stack's DigestFlush mirrors buffer drops into
// rabbot_digest_dropped_total, and dispatched digest alerts are recorded via the
// shared dispatcher metrics (slice 3 through the same funnel).
func TestBuildAlertingStack_DigestFlushMetered(t *testing.T) {
	st := openTestStore(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack-digest", Type: "slack-webhook", URL: srv.URL},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{}, Notifier: "slack-digest"},
		},
		Alerting: config.AlertingConfig{
			DedupWindow: "5m", PerRecipientHourlyCap: 30, IncidentAutoCloseAfter: "24h",
		},
	}

	m := obs.NewMetrics("vtest")
	stack, err := BuildAlertingStack(cfg, st, srv.Client(), time.Now, nil, nil, WithStackMetrics(m))
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	// Drive the digest buffer directly through the sink seam so the test does not
	// depend on the pipeline's routing of over-cap alerts.
	sink := stack.digestSink
	if sink == nil {
		t.Fatal("stack.digestSink is nil; expected the supervisor-owned digest buffer")
	}
	sink.max = 1
	sink.Add(notify.Alert{Site: "a", ChangeType: "title"})
	sink.Add(notify.Alert{Site: "b", ChangeType: "title"}) // drops "a"
	sink.Add(notify.Alert{Site: "c", ChangeType: "title"}) // drops "b" => 2 dropped, "c" remains

	stack.DigestFlush(context.Background())

	// Drops mirrored.
	const wantDropped = `
		# HELP rabbot_digest_dropped_total Total digest entries dropped due to a full buffer.
		# TYPE rabbot_digest_dropped_total counter
		rabbot_digest_dropped_total 2
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(wantDropped), "rabbot_digest_dropped_total"); err != nil {
		t.Errorf("rabbot_digest_dropped_total mismatch:\n%s", err)
	}

	// The one surviving alert was dispatched and recorded by config name via the
	// shared dispatcher metrics (slice 3 through the single funnel).
	const wantDispatch = `
		# HELP rabbot_alerts_dispatched_total Total alert dispatch attempts by notifier and outcome.
		# TYPE rabbot_alerts_dispatched_total counter
		rabbot_alerts_dispatched_total{notifier="slack-digest",outcome="ok"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(wantDispatch), "rabbot_alerts_dispatched_total"); err != nil {
		t.Errorf("rabbot_alerts_dispatched_total mismatch:\n%s", err)
	}
}

// A nil-metrics stack (the default 6-arg construction) builds and flushes without
// panic — every existing caller is unchanged.
func TestBuildAlertingStack_NilMetricsSafe(t *testing.T) {
	st := openTestStore(t)
	cfg := config.Config{
		Notifiers: []config.NotifierConfig{{Name: "n", Type: "slack-webhook", URL: "https://hooks.slack.com/services/T/B/X"}},
		Routes:    []config.RouteConfig{{Match: map[string]string{}, Notifier: "n"}},
		Alerting:  config.AlertingConfig{DedupWindow: "5m", PerRecipientHourlyCap: 30, IncidentAutoCloseAfter: "24h"},
	}
	stack, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}
	stack.DigestFlush(context.Background()) // must not panic with nil metrics
}

var _ = model.Site{} // keep model import available for fixtures
