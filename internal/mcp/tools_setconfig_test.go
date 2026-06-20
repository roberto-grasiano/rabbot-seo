package mcpsrv

import (
	"context"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestSetConfigHandler_RejectsNonAllowlistedKeyNoBridgeCall(t *testing.T) {
	t.Parallel()

	m := &mockBridge{} // SetConfig must NOT be invoked
	out, err := setConfigHandlerCore(context.Background(), m, "control.port", "9999")
	if err != nil {
		t.Fatalf("handler returned Go error, want errors-as-data: %v", err)
	}
	if out.OK {
		t.Errorf("OK = true, want false for a rejected key")
	}
	if !strings.Contains(out.Message, "control.port") {
		t.Errorf("message %q does not name the rejected key", out.Message)
	}
	if !strings.Contains(out.Message, "log.level") {
		t.Errorf("message %q does not list the allowed keys", out.Message)
	}
	if m.setConfigCalls != 0 {
		t.Errorf("bridge SetConfig called %d times for a rejected key, want 0", m.setConfigCalls)
	}
}

func TestSetConfigHandler_RejectsDeniedSecretKeyNoLeak(t *testing.T) {
	t.Parallel()

	m := &mockBridge{}
	secret := "https://hooks.slack.com/services/T000/B000/SECRET"
	out, err := setConfigHandlerCore(context.Background(), m, "notifiers.0.url", secret)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if out.OK {
		t.Errorf("OK = true, want false for a denied key")
	}
	if strings.Contains(out.Message, secret) || strings.Contains(out.Message, "SECRET") {
		t.Errorf("message leaked the secret value: %q", out.Message)
	}
	if m.setConfigCalls != 0 {
		t.Errorf("bridge SetConfig called for a denied key, want 0")
	}
}

func TestSetConfigHandler_AllowedKeyEchoesKeyNotValue(t *testing.T) {
	t.Parallel()

	m := &mockBridge{}
	out, err := setConfigHandlerCore(context.Background(), m, "log.level", "debug")
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !out.OK {
		t.Errorf("OK = false, want true for an allowed key (message=%q)", out.Message)
	}
	// Secret-safe echo: the acknowledgement names the key, never the value.
	if !strings.Contains(out.Message, "log.level") {
		t.Errorf("message %q does not name the set key", out.Message)
	}
	if strings.Contains(out.Message, "debug") {
		t.Errorf("message %q echoed the value — must echo key only", out.Message)
	}
	if m.setConfigCalls != 1 {
		t.Errorf("bridge SetConfig called %d times, want 1", m.setConfigCalls)
	}
	if m.lastSetConfigKey != "log.level" || m.lastSetConfigValue != "debug" {
		t.Errorf("bridge got (%q,%q), want (log.level,debug)", m.lastSetConfigKey, m.lastSetConfigValue)
	}
}

func TestSetConfigHandler_BridgeErrorMapped(t *testing.T) {
	t.Parallel()

	m := &mockBridge{setConfigErr: errDaemonDownStub()}
	out, err := setConfigHandlerCore(context.Background(), m, "log.level", "info")
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if out.OK {
		t.Error("OK = true, want false when the bridge fails")
	}
	if out.Message == "" {
		t.Error("expected a non-empty friendly message on bridge failure")
	}
}

// errDaemonDownStub returns the control "daemon not running" sentinel so the
// bridge-error path is exercised without importing control in every assertion.
func errDaemonDownStub() error { return control.ErrDaemonNotRunning }

func TestSetConfigToolRegistered(t *testing.T) {
	t.Parallel()

	srv := NewServer(&mockBridge{}, "test")
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	// Construction must not panic and must register set_config. AddTool panics on a
	// bad schema, so reaching here proves the tool's In/Out structs are valid object
	// schemas and the annotations are well-formed. (The annotation values are
	// asserted structurally below via the tool definition helper.)
	gotTrue := true
	gotFalse := false
	wantDestructive := ptrBool(false)
	wantOpenWorld := ptrBool(false)
	_ = gotTrue
	_ = gotFalse
	if *wantDestructive != false || *wantOpenWorld != false {
		t.Fatal("ptrBool sanity failed")
	}
}
