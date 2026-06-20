package cli

import (
	"fmt"
	"os"
	"os/user"
	"runtime"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"github.com/roberto-grasiano/rabbot-seo/internal/supervisor"
)

// baseServiceConfig returns the platform-agnostic kardianos definition. The run-as
// identity (UserName / per-user LaunchAgent) is layered on by applyRunAsIdentity so
// the bare config is easy to assert in tests.
func baseServiceConfig() *service.Config {
	return &service.Config{
		Name:        "rabbot",
		DisplayName: "Rabbot-SEO",
		Description: "Real-time SEO monitoring gateway",
		Arguments:   []string{"run"},
		Option: service.KeyValue{
			"DelayedAutoStart": true,
		},
	}
}

// serviceConfig returns the kardianos service definition used for install/control,
// with the run-as identity resolved for the current host (see applyRunAsIdentity).
func serviceConfig() *service.Config {
	cfg := baseServiceConfig()
	applyRunAsIdentity(cfg, runtime.GOOS, os.LookupEnv, currentServiceUser)
	return cfg
}

// applyRunAsIdentity layers the per-platform run-as identity onto cfg so the
// installed unit runs as the installing user and reads THAT user's config/data,
// instead of root's empty config (the pre-fix foot-gun):
//
//   - linux: set Config.UserName to the installing user — SUDO_USER when present and
//     non-empty (the `sudo rabbot service install` case), else the current user.
//     kardianos emits systemd `User=` only when UserName is set; a genuine root login
//     (no SUDO_USER) resolves to root, byte-identical to today's VPS posture.
//   - darwin: set Option["UserService"] = true ⇒ a per-user LaunchAgent in
//     ~/Library/LaunchAgents (installs without sudo, runs as the installing user); no
//     UserName is needed — the agent inherently runs as its owner.
//   - windows: unchanged (LocalSystem). kardianos maps UserName → ServiceStartName,
//     which requires an account password; per-user SCM services are a different
//     mechanism. The per-user gap on Windows is a documented limitation.
//
// goos, lookupEnv and currentUser are injected so the table-driven test exercises
// every platform from the one host it runs on.
func applyRunAsIdentity(cfg *service.Config, goos string, lookupEnv func(string) (string, bool), currentUser func() string) {
	switch goos {
	case "linux":
		if v, ok := lookupEnv("SUDO_USER"); ok && v != "" {
			cfg.UserName = v
			return
		}
		cfg.UserName = currentUser()
	case "darwin":
		if cfg.Option == nil {
			cfg.Option = service.KeyValue{}
		}
		cfg.Option["UserService"] = true
	default:
		// windows and any other platform: keep the manager default (LocalSystem on
		// Windows). Documented limitation; never silently bound to a wrong identity.
	}
}

// currentServiceUser returns the current login user for the run-as identity, or ""
// when it cannot be determined (kardianos then omits `User=`, falling back to the
// manager default rather than binding a bogus name).
func currentServiceUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// newService builds a kardianos service bound to an (unstarted) Daemon. The
// service manager calls daemon.Start, which itself opens the store and loop.
func newService() (service.Service, *supervisor.Daemon, error) {
	d := &supervisor.Daemon{}
	svc, err := service.New(d, serviceConfig())
	if err != nil {
		return nil, nil, err
	}
	return svc, d, nil
}

func newServiceCmd(bi BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and control the rabbot OS service",
	}
	action := func(name string, fn func(service.Service) error) *cobra.Command {
		return &cobra.Command{
			Use:   name,
			Short: fmt.Sprintf("%s the rabbot service", name),
			RunE: func(cmd *cobra.Command, args []string) error {
				svc, _, err := newService()
				if err != nil {
					return err
				}
				if err := fn(svc); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "service %s: ok\n", name)
				return nil
			},
		}
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show the rabbot service status",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, _, err := newService()
			if err != nil {
				return err
			}
			st, err := svc.Status()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", statusString(st))
			return nil
		},
	}

	cmd.AddCommand(
		action("install", service.Service.Install),
		action("uninstall", service.Service.Uninstall),
		action("start", service.Service.Start),
		action("stop", service.Service.Stop),
		statusCmd,
	)
	return cmd
}

func statusString(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}
