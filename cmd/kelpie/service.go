package main

import (
	"context"
	"os"
	"runtime"

	"github.com/metalmatze/kelpie/internal/service"
)

type ServiceCmd struct {
	Install   ServiceInstallCmd   `cmd:"" help:"Run the daemon as a launchd LaunchAgent (macOS) or systemd user service (Linux)."`
	Uninstall ServiceUninstallCmd `cmd:"" help:"Stop the service and remove its unit."`
	Status    ServiceStatusCmd    `cmd:"" help:"Show whether the service is installed and running."`
}

// ServiceFlags are shared by every service subcommand.
type ServiceFlags struct {
	Label string `help:"launchd label (default dev.kelpie.daemon) or systemd unit name (default kelpie)."`
}

type ServiceInstallCmd struct {
	ServiceFlags
	Print bool `help:"Print the unit instead of writing and loading it; save it by hand to install manually."`
}

func (c *ServiceInstallCmd) Help() string {
	return `The unit bakes in this kelpie binary's path, --config, and the PATH and XDG_* variables of this shell, so gh, herdr and orca resolve as they do in your terminal. Run install again after PATH changes.

On Linux the service starts with your login session. To run without one: loginctl enable-linger $USER.`
}

func (c *ServiceInstallCmd) Run(ctx context.Context, g *Globals) error {
	s, err := service.New(runtime.GOOS, c.Label, g.Config, os.Stdout)
	if err != nil {
		return err
	}
	if c.Print {
		unit, err := s.Render()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(unit)
		return err
	}
	return s.Install(ctx)
}

type ServiceUninstallCmd struct{ ServiceFlags }

func (c *ServiceUninstallCmd) Run(ctx context.Context, g *Globals) error {
	s, err := service.New(runtime.GOOS, c.Label, g.Config, os.Stdout)
	if err != nil {
		return err
	}
	return s.Uninstall(ctx)
}

type ServiceStatusCmd struct{ ServiceFlags }

func (c *ServiceStatusCmd) Run(ctx context.Context, g *Globals) error {
	s, err := service.New(runtime.GOOS, c.Label, g.Config, os.Stdout)
	if err != nil {
		return err
	}
	return s.Status(ctx)
}
