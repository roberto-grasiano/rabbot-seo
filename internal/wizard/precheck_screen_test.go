package wizard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
)

func fixtureReport() precheck.Report {
	return precheck.Report{
		Verdict: precheck.VerdictGreen,
		Summary: "Green: this URL looks monitorable.",
		JS: precheck.JSDependency{
			Kind:             precheck.ServerRendered,
			VisibleWordCount: 120,
			Signals: []precheck.Signal{
				{Name: "next_data_payload", Present: false, Detail: "not present"},
				{Name: "missing_head_fields", Present: false, Detail: "server HTML carries head fields: title, h1"},
			},
		},
		Doctor: fetcher.DoctorReport{
			HomepageStatus: 200,
			RobotsVerdict:  "allowed",
		},
	}
}

func TestRenderReportPure(t *testing.T) {
	rep := fixtureReport()
	out := RenderReport(rep)
	if !strings.Contains(out, "GREEN") {
		t.Errorf("RenderReport missing verdict label GREEN:\n%s", out)
	}
	if !strings.Contains(out, rep.Summary) {
		t.Errorf("RenderReport missing summary:\n%s", out)
	}
	if !strings.Contains(out, "next_data_payload") {
		t.Errorf("RenderReport missing at least one signal name:\n%s", out)
	}
}

// quitsWith asserts the cmd, when invoked, yields a tea.QuitMsg.
func quitsWith(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a non-nil quit cmd, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg from cmd, got %T", cmd())
	}
}

func TestPrecheckModelUpdateDone(t *testing.T) {
	rep := fixtureReport()
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return rep, nil })

	next, cmd := m.Update(precheckDoneMsg{rep: rep})
	pm, ok := next.(precheckModel)
	if !ok {
		t.Fatalf("Update returned %T, want precheckModel", next)
	}
	if !pm.done {
		t.Error("expected done=true after precheckDoneMsg")
	}
	if pm.rep.Verdict != precheck.VerdictGreen {
		t.Errorf("rep not stored: verdict=%q", pm.rep.Verdict)
	}
	quitsWith(t, cmd)
}

func TestPrecheckModelUpdateError(t *testing.T) {
	wantErr := errors.New("boom")
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return precheck.Report{}, wantErr })

	next, cmd := m.Update(precheckDoneMsg{err: wantErr})
	pm := next.(precheckModel)
	if !pm.done {
		t.Error("expected done=true after error msg")
	}
	if !errors.Is(pm.err, wantErr) {
		t.Errorf("err = %v, want %v", pm.err, wantErr)
	}
	quitsWith(t, cmd)
}

func TestPrecheckModelUpdateSpinnerTick(t *testing.T) {
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return fixtureReport(), nil })

	next, cmd := m.Update(spinner.TickMsg{})
	pm := next.(precheckModel)
	if pm.done {
		t.Error("spinner tick should not mark done")
	}
	if cmd == nil {
		t.Error("spinner tick should return a non-nil cmd to keep ticking")
	}
}

func TestPrecheckModelInitNonNil(t *testing.T) {
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return fixtureReport(), nil })
	if m.Init() == nil {
		t.Error("Init should return a non-nil batch cmd")
	}
}

// TestPrecheckModelUpdateEscCancels pins the abort UX on the live precheck
// screen: pressing Esc must quit the program WITHOUT marking done, so the
// orchestrator's screenCancelled(!done) guard treats it as a cancel (matching
// huh forms, where Esc aborts). Before the fix Esc fell through to (m, nil) and
// the screen hung with no feedback.
func TestPrecheckModelUpdateEscCancels(t *testing.T) {
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return fixtureReport(), nil })

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm, ok := next.(precheckModel)
	if !ok {
		t.Fatalf("Update returned %T, want precheckModel", next)
	}
	if pm.done {
		t.Error("Esc must NOT mark done; the orchestrator's !done guard must see a cancel")
	}
	quitsWith(t, cmd)
}

// TestPrecheckModelUpdateCtrlCCancels pins that ctrl+c keeps quitting without
// marking done (regression guard alongside the new Esc branch).
func TestPrecheckModelUpdateCtrlCCancels(t *testing.T) {
	m := NewPrecheckModel(context.Background(), "https://example.com",
		func(context.Context) (precheck.Report, error) { return fixtureReport(), nil })

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	pm := next.(precheckModel)
	if pm.done {
		t.Error("ctrl+c must NOT mark done")
	}
	quitsWith(t, cmd)
}
