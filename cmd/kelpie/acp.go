package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/metalmatze/kelpie/internal/acp"
	"github.com/metalmatze/kelpie/internal/control"
)

type AcpCmd struct {
	Run  AcpRunCmd  `cmd:"" help:"Run an ACP agent behind kelpie; use it as the agent command in your editor."`
	List AcpListCmd `cmd:"" help:"List the ACP sessions kelpie can prompt."`
	Send AcpSendCmd `cmd:"" help:"Prompt an ACP session."`
}

func acpDir() string { return filepath.Join(control.StateDir(), "acp") }

type AcpRunCmd struct {
	Agent []string `arg:"" passthrough:"" help:"The agent command and its arguments."`
}

// Run logs to a file: stdout is the protocol, and editors bury the agent's
// stderr in their own logs.
func (c *AcpRunCmd) Run(ctx context.Context) error {
	if len(c.Agent) > 0 && c.Agent[0] == "--" {
		c.Agent = c.Agent[1:]
	}
	if len(c.Agent) == 0 {
		return fmt.Errorf("no agent command")
	}
	if err := os.MkdirAll(acpDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(acpDir(), "proxy.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	log := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})).With("pid", os.Getpid())
	log.Info("start", "agent", c.Agent)
	p := &acp.Proxy{Agent: c.Agent, Dir: acpDir(), Log: log}
	err = p.Run(ctx, os.Stdin, os.Stdout)
	log.Info("exit", "err", err)
	return err
}

type AcpListCmd struct{}

func (AcpListCmd) Run() error {
	regs, err := acp.List(acpDir())
	if err != nil {
		return err
	}
	for _, r := range regs {
		for _, s := range r.Sessions {
			state := "idle"
			if s.Busy {
				state = "busy"
			}
			fmt.Printf("%d  %s  %-4s  %s  (%s)\n", r.PID, s.ID, state, s.Cwd, strings.Join(r.Agent, " "))
		}
	}
	return nil
}

type AcpSendCmd struct {
	Session string   `help:"Session ID as kelpie acp list shows it; default: the newest session of the only proxy."`
	Text    []string `arg:"" help:"Prompt text."`
}

func (c *AcpSendCmd) Run() error {
	regs, err := acp.List(acpDir())
	if err != nil {
		return err
	}
	socket := ""
	for _, r := range regs {
		for _, s := range r.Sessions {
			if s.ID == c.Session {
				socket = r.Socket
			}
		}
	}
	if c.Session == "" {
		if len(regs) != 1 {
			return fmt.Errorf("%d ACP proxies run; pass --session", len(regs))
		}
		socket = regs[0].Socket
	}
	if socket == "" {
		return fmt.Errorf("no ACP session %q", c.Session)
	}
	sid, err := acp.Send(socket, c.Session, strings.Join(c.Text, " "))
	if err != nil {
		return err
	}
	fmt.Println("prompted", sid)
	return nil
}
