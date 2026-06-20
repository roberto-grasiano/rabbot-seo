package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// TestResolveDaemonInstanceKeyMintsOnFreshDir asserts that on a fresh data dir
// (no key file yet) the daemon helper mints and returns a 32-byte instance key,
// without panicking or returning nil. This is the happy path: a first-run daemon
// gets a working key.
func TestResolveDaemonInstanceKeyMintsOnFreshDir(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "debug")
	path := filepath.Join(t.TempDir(), "instance.key")

	key := resolveDaemonInstanceKey(path, logger)
	if len(key) != 32 {
		t.Fatalf("fresh dir: got key len %d, want 32", len(key))
	}
}

// TestResolveDaemonInstanceKeyDegradesOnMalformedKey is the regression guard for
// the spec's daemon fail-safe (§Security / §Error handling): a present-but-
// unreadable / malformed instance.key must NOT crash the daemon. The helper must
// log the condition and return a nil key, so verify.Verify (which fails safe on a
// zero-length key) demotes affected sites to throttled rather than aborting the
// entire crawl/alert/control stack.
func TestResolveDaemonInstanceKeyDegradesOnMalformedKey(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, "debug")
	path := filepath.Join(t.TempDir(), "instance.key")
	// A non-hex body is malformed: LoadOrCreateInstanceKey fails closed (error),
	// but the DAEMON path must degrade rather than propagate that as a crash.
	if err := os.WriteFile(path, []byte("not-hex"), 0o600); err != nil {
		t.Fatalf("seed malformed key: %v", err)
	}

	key := resolveDaemonInstanceKey(path, logger)
	if key != nil {
		t.Fatalf("malformed key: got non-nil key (len %d), want nil so the daemon degrades", len(key))
	}

	// The condition must be logged (component=supervisor), never the key bytes.
	logged := buf.String()
	if !strings.Contains(logged, "supervisor") {
		t.Errorf("expected a supervisor-component log line, got: %q", logged)
	}
	if strings.Contains(logged, "not-hex") {
		t.Errorf("malformed key BYTES must never be logged; log was: %q", logged)
	}
}
