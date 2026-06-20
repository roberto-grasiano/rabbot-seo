package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// failingNotifier always returns the given error from Notify, carrying a config
// name distinct from any webhook URL.
type failingNotifier struct {
	name string
	err  error
}

func (f *failingNotifier) Name() string { return f.name }
func (f *failingNotifier) Notify(ctx context.Context, a Alert) error {
	return f.err
}

// Criterion 4: Dispatch records one dispatch per delivered notifier, labelled by
// the OPERATOR-CONFIG NAME (never the webhook URL); outcome derives from the
// returned error. The exposition never contains the secret webhook URL.
func TestDispatch_RecordsMetricsByConfigName(t *testing.T) {
	secret := "https://hooks.example.com/services/SECRET/TOKEN"
	okNotifier := &recordingNotifier{name: "slack-ok"}
	badNotifier := &failingNotifier{name: "ops-webhook", err: errors.New(secret + " 503")}

	routes := []config.RouteConfig{
		{Match: map[string]string{"severity": "critical"}, Notifier: "ops-webhook"},
		{Match: map[string]string{}, Notifier: "slack-ok"}, // fallback
	}
	reg := NewRegistry(map[string]Notifier{"slack-ok": okNotifier, "ops-webhook": badNotifier}, routes)

	m := obs.NewMetrics("vtest")
	d := NewDispatcher(reg, WithMetrics(m))

	// A succeeding delivery (fallback route -> slack-ok).
	if err := d.Dispatch(context.Background(), Alert{Site: "x", Severity: model.SeverityInfo}); err != nil {
		t.Fatalf("Dispatch(ok): %v", err)
	}
	// A failing delivery (critical route -> ops-webhook).
	if err := d.Dispatch(context.Background(), Alert{Site: "x", Severity: model.SeverityCritical}); err == nil {
		t.Fatal("Dispatch(error) returned nil, want the notifier error")
	}

	const want = `
		# HELP rabbot_alerts_dispatched_total Total alert dispatch attempts by notifier and outcome.
		# TYPE rabbot_alerts_dispatched_total counter
		rabbot_alerts_dispatched_total{notifier="ops-webhook",outcome="error"} 1
		rabbot_alerts_dispatched_total{notifier="slack-ok",outcome="ok"} 1
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_alerts_dispatched_total"); err != nil {
		t.Errorf("rabbot_alerts_dispatched_total mismatch:\n%s", err)
	}

	// Exposition must NEVER contain the webhook URL — not as a label value, not
	// anywhere in the gathered families.
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		for _, met := range fam.GetMetric() {
			for _, lp := range met.GetLabel() {
				if strings.Contains(lp.GetValue(), "hooks.example.com") {
					t.Fatalf("webhook URL leaked into exposition: metric %q label %q=%q",
						fam.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

// A nil *Metrics dispatcher must dispatch normally and never panic.
func TestDispatch_NilMetricsSafe(t *testing.T) {
	n := &recordingNotifier{name: "slack"}
	routes := []config.RouteConfig{{Match: map[string]string{}, Notifier: "slack"}}
	reg := NewRegistry(map[string]Notifier{"slack": n}, routes)

	d := NewDispatcher(reg) // no metrics option -> nil-safe
	if err := d.Dispatch(context.Background(), Alert{Site: "x", Severity: model.SeverityInfo}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(n.got) != 1 {
		t.Errorf("expected 1 delivery, got %d", len(n.got))
	}
}
