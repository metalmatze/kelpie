package main

import (
	"context"
	"errors"
	"os"

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
	Print bool   `help:"Print the unit instead of writing and loading it; save it by hand to install manually."`
	OS    string `help:"Render for this OS (darwin or linux) instead of this one. Only with --print." enum:",darwin,linux" default:""`
}

func (c *ServiceInstallCmd) Help() string {
	return `The unit bakes in this kelpie binary's path, --config, and the PATH and XDG_* variables of this shell, so gh, herdr and orca resolve as they do in your terminal. The unit keeps the PATH it was installed with, so run install again after it changes (a new tool directory, a moved fnm or nvm shim).

It coexists with the herdr plugin: whichever starts the daemon second finds the lock held and exits 0. The unit restarts only on failure, so if the plugin's daemon started first, the unit's stays stopped until the next login.

On Linux the service starts with your login session. To run without one: loginctl enable-linger $USER.`
}

func (c *ServiceInstallCmd) Run(ctx context.Context, g *Globals) error {
	goos := service.Current
	if c.OS != "" {
		if !c.Print {
			return errors.New("--os only works with --print")
		}
		goos = c.OS
	}
	s, err := service.New(goos, c.Label, g.Config, os.Stdout)
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
	s, err := service.New(service.Current, c.Label, g.Config, os.Stdout)
	if err != nil {
		return err
	}
	return s.Uninstall(ctx)
}

type ServiceStatusCmd struct{ ServiceFlags }

func (c *ServiceStatusCmd) Run(ctx context.Context, g *Globals) error {
	s, err := service.New(service.Current, c.Label, g.Config, os.Stdout)
	if err != nil {
		return err
	}
	return s.Status(ctx)
}
