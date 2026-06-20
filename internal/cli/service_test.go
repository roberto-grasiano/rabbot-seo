package cli

import "testing"

func TestServiceSubcommands(t *testing.T) {
	cmd := newServiceCmd(BuildInfo{Version: "0.0.1"})
	want := []string{"install", "uninstall", "start", "stop", "status"}
	have := map[string]bool{}
	for _, c := range cmd.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("service command missing subcommand %q", w)
		}
	}
}

func TestServiceConfigBuilds(t *testing.T) {
	cfg := serviceConfig()
	if cfg.Name != "rabbot" {
		t.Errorf("service name = %q, want rabbot", cfg.Name)
	}
	if len(cfg.Arguments) == 0 || cfg.Arguments[0] != "run" {
		t.Errorf("service arguments = %v, want it to invoke 'run'", cfg.Arguments)
	}
}
