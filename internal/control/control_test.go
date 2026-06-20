package control

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateTokenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.token")

	tok1, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("first LoadOrCreateToken: %v", err)
	}
	if len(tok1) < 32 {
		t.Errorf("token too short: %d chars", len(tok1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	// Windows maps NTFS attrs onto Go's mode bits (0666/0444), so an exact
	// 0600 assertion can't hold there; assert perms only where they exist.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file perms = %o, want 600", perm)
		}
	}

	tok2, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateToken: %v", err)
	}
	if tok1 != tok2 {
		t.Errorf("token not stable across reads: %q vs %q", tok1, tok2)
	}
}

// TestLoadOrCreateTokenTightensPerms is the regression for finding Low-token-perms:
// the LOAD path (an existing non-empty token file) previously returned the token
// without re-checking its permissions — os.WriteFile's 0600 only applies on
// create. A token file that ended up group/world-readable (e.g. 0644 from a
// restore or a misconfigured deploy) leaked the bearer secret. LoadOrCreateToken
// must re-tighten an over-permissive existing file to 0600 while preserving the
// stored token value.
func TestLoadOrCreateTokenTightensPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.token")

	const want = "preexisting-token-value"
	if err := os.WriteFile(path, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("seed token file: %v", err)
	}

	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if tok != want {
		t.Errorf("token value = %q, want %q (must preserve existing token)", tok, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file perms = %o, want 600 (must re-tighten on load)", perm)
		}
	}
}

func TestStatusResponseShape(t *testing.T) {
	// Compile-time guard that the contract §4 fields exist with the right types.
	sr := StatusResponse{
		Version:     "1.0.0",
		Uptime:      "1h",
		Paused:      false,
		SiteCount:   3,
		URLCount:    10,
		DueCount:    0,
		QueueDepth:  0,
		LastCrawlAt: "",
		EgressIP:    nil, // []string; populated by M1's richer Status hook
	}
	if sr.SiteCount != 3 {
		t.Errorf("SiteCount = %d, want 3", sr.SiteCount)
	}
}
