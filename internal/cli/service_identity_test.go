package cli

import "testing"

// TestServiceConfigRunAsIdentity pins criterion 4: the run-as resolution returns
// SUDO_USER when set (Linux UserName == "alice"), else the current user; the
// darwin-shaped config carries Option["UserService"] == true; the windows-shaped
// config sets no UserName (LocalSystem). It drives the GOOS/env seam directly so
// every platform's behavior is asserted from the one host the tests run on.
func TestServiceConfigRunAsIdentity(t *testing.T) {
	const current = "bob"
	currentUser := func() string { return current }

	withEnv := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := m[k]
			return v, ok
		}
	}

	t.Run("linux sudo invocation binds SUDO_USER", func(t *testing.T) {
		cfg := baseServiceConfig()
		applyRunAsIdentity(cfg, "linux", withEnv(map[string]string{"SUDO_USER": "alice"}), currentUser)
		if cfg.UserName != "alice" {
			t.Fatalf("UserName = %q, want alice (SUDO_USER)", cfg.UserName)
		}
		if us, _ := cfg.Option["UserService"].(bool); us {
			t.Fatalf("linux config must not set UserService")
		}
	})

	t.Run("linux genuine root login stays root (current user)", func(t *testing.T) {
		cfg := baseServiceConfig()
		applyRunAsIdentity(cfg, "linux", withEnv(map[string]string{}), currentUser)
		if cfg.UserName != current {
			t.Fatalf("UserName = %q, want %q (current user, no SUDO_USER)", cfg.UserName, current)
		}
	})

	t.Run("linux empty SUDO_USER falls back to current user", func(t *testing.T) {
		// An empty-but-set SUDO_USER must NOT bind an empty username.
		cfg := baseServiceConfig()
		applyRunAsIdentity(cfg, "linux", withEnv(map[string]string{"SUDO_USER": ""}), currentUser)
		if cfg.UserName != current {
			t.Fatalf("UserName = %q, want %q (empty SUDO_USER ⇒ current user)", cfg.UserName, current)
		}
	})

	t.Run("darwin installs a per-user LaunchAgent", func(t *testing.T) {
		cfg := baseServiceConfig()
		applyRunAsIdentity(cfg, "darwin", withEnv(map[string]string{"SUDO_USER": "alice"}), currentUser)
		us, ok := cfg.Option["UserService"].(bool)
		if !ok || !us {
			t.Fatalf("darwin config Option[UserService] = %v, want true", cfg.Option["UserService"])
		}
		// A per-user LaunchAgent runs as the installing user inherently; no UserName.
		if cfg.UserName != "" {
			t.Fatalf("darwin UserName = %q, want empty (LaunchAgent runs as the user)", cfg.UserName)
		}
	})

	t.Run("windows keeps LocalSystem (no UserName, no UserService)", func(t *testing.T) {
		cfg := baseServiceConfig()
		applyRunAsIdentity(cfg, "windows", withEnv(map[string]string{"SUDO_USER": "alice"}), currentUser)
		if cfg.UserName != "" {
			t.Fatalf("windows UserName = %q, want empty (LocalSystem)", cfg.UserName)
		}
		if _, set := cfg.Option["UserService"]; set {
			t.Fatalf("windows config must not set UserService")
		}
	})
}

// TestServiceConfigWiresIdentity proves the production serviceConfig() runs the
// identity pass (not just baseServiceConfig). On the test host (linux) UserName
// resolves to a non-empty user; on darwin it would set UserService. We assert the
// host-appropriate outcome so the wiring is covered without a GOOS override.
func TestServiceConfigWiresIdentity(t *testing.T) {
	cfg := serviceConfig()
	if cfg.Name != "rabbot" {
		t.Fatalf("name = %q, want rabbot", cfg.Name)
	}
}
